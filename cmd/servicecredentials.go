package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

// scEnvConcurrency bounds how many /v3/apps/:guid/env calls run at once
// across all matched apps, so a large match set doesn't open hundreds of
// simultaneous requests against the CF API.
const scEnvConcurrency = 8

// scMatchedApp is one app that passed org/space/name filtering, carrying the
// org and space names it needs to be annotated with in the final output.
type scMatchedApp struct {
	App       cf.App
	OrgName   string
	SpaceName string
}

type scRegionResult struct {
	Region string
	APIURL string
	Apps   []scMatchedApp
	Err    error
}

// scEnvJob is one app whose environment still needs to be fetched, paired
// with the already-authenticated client for its region.
type scEnvJob struct {
	client    *cf.Client
	app       cf.App
	orgName   string
	spaceName string
}

var serviceCredentialsCmd = &cobra.Command{
	Use:   "service-credentials",
	Short: "Extract VCAP_SERVICES credentials from matching apps across orgs",
	Long: `Look through apps across one or more regions and organizations, fetch each
matching app's bound service credentials from VCAP_SERVICES (via its system
environment variables), and output them as a flat JSON array — one entry per
service binding, with org and space name added into each entry.

If neither --org nor --orgs is given, the default org scope selected via
'bo orgs' is used. If no default scope has been set either, run 'bo orgs' to
pick one interactively, or pass --org/--orgs directly.

Use --spaces to narrow to specific space names: comma-separated, matched
case-insensitively, exact name (not a substring). Use --apps to narrow to
specific app names: comma-separated patterns, e.g. '*-srv,*-app' — each is a
glob (via filepath.Match) if it contains * ? [, otherwise a case-insensitive
substring. Use --services to keep only service bindings whose label (service
type, e.g. "hana") or name (service instance name) contains any of these
comma-separated, case-insensitive keywords.

Fetching an app's environment requires at least Space Developer access to the
space it's in; apps that can't be read are reported as warnings on stderr and
skipped rather than aborting the whole command.

Use --output/-o to write the result to a file instead of stdout.

If --regions is omitted the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		orgsFile, _ := cmd.Flags().GetString("orgs")
		excludeOrgsFile, _ := cmd.Flags().GetString("excludeOrgs")
		orgGUID, _ := cmd.Flags().GetString("org")
		spacesFlag, _ := cmd.Flags().GetString("spaces")
		appsFlag, _ := cmd.Flags().GetString("apps")
		servicesFlag, _ := cmd.Flags().GetString("services")
		outputFile, _ := cmd.Flags().GetString("output")

		creds, err := store.Load()
		if err != nil {
			return fmt.Errorf("not logged in — run: bo login --regions <region>")
		}

		var apiURLs []string
		if regionsFlag != "" {
			for _, r := range splitCSV(regionsFlag) {
				apiURLs = append(apiURLs, store.RegionToAPIURL(r))
			}
		} else {
			apiURLs = creds.ActiveAPIURLs
		}
		if len(apiURLs) == 0 {
			return fmt.Errorf("no regions configured — run: bo login --regions <region1,region2>")
		}

		var includeOrgs cosOrgSet
		if orgsFile != "" {
			includeOrgs, err = parseCosOrgCSV(orgsFile)
			if err != nil {
				return fmt.Errorf("invalid --orgs CSV: %w", err)
			}
		}
		var excludeOrgs cosOrgSet
		if excludeOrgsFile != "" {
			excludeOrgs, err = parseCosOrgCSV(excludeOrgsFile)
			if err != nil {
				return fmt.Errorf("invalid --excludeOrgs CSV: %w", err)
			}
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		if orgGUID == "" && orgsFile == "" {
			includeOrgs, err = resolveDefaultOrgScope(creds)
			if err != nil {
				return err
			}
		}

		// Phase 1: resolve matching apps per region (orgs → spaces → apps),
		// in parallel across regions.
		results := make([]scRegionResult, len(apiURLs))
		var wg sync.WaitGroup
		for i, apiURL := range apiURLs {
			wg.Add(1)
			go func(idx int, url string) {
				defer wg.Done()
				regionName := store.APIURLToRegion(url)

				tok, ok := creds.Tokens[url]
				if !ok {
					results[idx] = scRegionResult{Region: regionName,
						Err: fmt.Errorf("no token — run: bo login --regions %s", regionName)}
					return
				}
				client := cf.NewClient(url, tok.AccessToken)
				client.SetTokenRefresher(makeTokenRefresher(url, tok.AccessToken))

				orgs, err := client.ListOrganizations(ctx)
				if err != nil {
					results[idx] = scRegionResult{Region: regionName, APIURL: url,
						Err: fmt.Errorf("listing orgs: %w", err)}
					return
				}

				var filteredOrgs []cf.Organization
				for _, org := range orgs {
					if orgGUID != "" && org.GUID != orgGUID {
						continue
					}
					if len(includeOrgs) > 0 && !includeOrgs.matches(regionName, org.GUID, org.Name) {
						continue
					}
					if len(excludeOrgs) > 0 && excludeOrgs.matches(regionName, org.GUID, org.Name) {
						continue
					}
					filteredOrgs = append(filteredOrgs, org)
				}
				if len(filteredOrgs) == 0 {
					results[idx] = scRegionResult{Region: regionName, APIURL: url}
					return
				}

				orgNameByGUID := make(map[string]string, len(filteredOrgs))
				orgGUIDs := make([]string, len(filteredOrgs))
				for i, o := range filteredOrgs {
					orgGUIDs[i] = o.GUID
					orgNameByGUID[o.GUID] = o.Name
				}

				spaces, err := client.ListSpacesByOrgs(ctx, orgGUIDs)
				if err != nil {
					results[idx] = scRegionResult{Region: regionName, APIURL: url,
						Err: fmt.Errorf("listing spaces: %w", err)}
					return
				}

				type spaceInfo struct{ Name, OrgName string }
				spaceByGUID := make(map[string]spaceInfo)
				var spaceGUIDs []string
				for _, sp := range spaces {
					if !scMatchesSpaceName(spacesFlag, sp.Name) {
						continue
					}
					spaceGUIDs = append(spaceGUIDs, sp.GUID)
					spaceByGUID[sp.GUID] = spaceInfo{
						Name:    sp.Name,
						OrgName: orgNameByGUID[sp.Relationships.Organization.Data.GUID],
					}
				}
				if len(spaceGUIDs) == 0 {
					results[idx] = scRegionResult{Region: regionName, APIURL: url}
					return
				}

				apps, err := client.ListAppsBySpaces(ctx, spaceGUIDs)
				if err != nil {
					results[idx] = scRegionResult{Region: regionName, APIURL: url,
						Err: fmt.Errorf("listing apps: %w", err)}
					return
				}

				var matched []scMatchedApp
				for _, a := range apps {
					if !scMatchesAppPattern(appsFlag, a.Name) {
						continue
					}
					info := spaceByGUID[a.Relationships.Space.Data.GUID]
					matched = append(matched, scMatchedApp{App: a, OrgName: info.OrgName, SpaceName: info.Name})
				}

				results[idx] = scRegionResult{Region: regionName, APIURL: url, Apps: matched}
			}(i, apiURL)
		}
		wg.Wait()

		// Phase 2: fetch VCAP_SERVICES for every matched app (bounded
		// concurrency across the whole match set, not just per region).
		var jobs []scEnvJob
		for _, r := range results {
			if r.Err != nil {
				fmt.Fprintf(os.Stderr, "warning: region %q: %v\n", r.Region, r.Err)
				continue
			}
			if len(r.Apps) == 0 {
				continue
			}
			tok := creds.Tokens[r.APIURL]
			client := cf.NewClient(r.APIURL, tok.AccessToken)
			client.SetTokenRefresher(makeTokenRefresher(r.APIURL, tok.AccessToken))
			for _, a := range r.Apps {
				jobs = append(jobs, scEnvJob{client: client, app: a.App, orgName: a.OrgName, spaceName: a.SpaceName})
			}
		}

		var (
			records   []map[string]interface{}
			recordsMu sync.Mutex
			jobWg     sync.WaitGroup
		)
		sem := make(chan struct{}, scEnvConcurrency)
		for _, j := range jobs {
			jobWg.Add(1)
			sem <- struct{}{}
			go func(j scEnvJob) {
				defer jobWg.Done()
				defer func() { <-sem }()

				vcapServices, err := j.client.GetAppEnv(ctx, j.app.GUID)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: app %q: fetching environment: %v\n", j.app.Name, err)
					return
				}

				var appRecords []map[string]interface{}
				for _, bindings := range vcapServices {
					for _, svc := range bindings {
						label, _ := svc["label"].(string)
						name, _ := svc["name"].(string)
						if servicesFlag != "" && !skipMatches(servicesFlag, label, name) {
							continue
						}
						svc["org"] = j.orgName
						svc["space"] = j.spaceName
						appRecords = append(appRecords, svc)
					}
				}
				if len(appRecords) == 0 {
					return
				}
				recordsMu.Lock()
				records = append(records, appRecords...)
				recordsMu.Unlock()
			}(j)
		}
		jobWg.Wait()

		out, closeOut, err := resolveOutputWriter(outputFile)
		if err != nil {
			return err
		}
		defer closeOut()

		if records == nil {
			records = []map[string]interface{}{}
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(records); err != nil {
			return fmt.Errorf("encoding JSON: %w", err)
		}
		return nil
	},
}

// scMatchesSpaceName reports whether spaceName should be included given
// --spaces' comma-separated list of exact (case-insensitive) space names; an
// empty list matches every space.
func scMatchesSpaceName(spaceNamesCSV, spaceName string) bool {
	if spaceNamesCSV == "" {
		return true
	}
	for _, n := range splitCSV(spaceNamesCSV) {
		if strings.EqualFold(n, spaceName) {
			return true
		}
	}
	return false
}

// scMatchesAppPattern reports whether appName should be included given
// --apps' comma-separated list of patterns; each pattern is treated as a glob
// (via filepath.Match) when it contains any of * ? [, otherwise as a
// case-insensitive substring. An empty list matches every app.
func scMatchesAppPattern(patternsCSV, appName string) bool {
	if patternsCSV == "" {
		return true
	}
	nameLower := strings.ToLower(appName)
	for _, p := range splitCSV(patternsCSV) {
		pl := strings.ToLower(p)
		if strings.ContainsAny(p, "*?[") {
			if m, _ := filepath.Match(pl, nameLower); m {
				return true
			}
			continue
		}
		if strings.Contains(nameLower, pl) {
			return true
		}
	}
	return false
}

func init() {
	serviceCredentialsCmd.GroupID = "cf-org"
	rootCmd.AddCommand(serviceCredentialsCmd)
	serviceCredentialsCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	serviceCredentialsCmd.Flags().String("org", "", "Org GUID to target; only apps from this org will be fetched")
	serviceCredentialsCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	serviceCredentialsCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,org_id,org_name)")
	serviceCredentialsCmd.Flags().String("spaces", "", "Only include apps in spaces with these comma-separated, exact (case-insensitive) space names")
	serviceCredentialsCmd.Flags().String("apps", "", "Only include apps whose name matches any of these comma-separated patterns (glob, e.g. '*-srv,*-app', or substring)")
	serviceCredentialsCmd.Flags().String("services", "", "Only include service bindings whose label or name contain any of these comma-separated, case-insensitive keywords")
	serviceCredentialsCmd.Flags().StringP("output", "o", "", "Write the JSON array to this file instead of stdout")
}
