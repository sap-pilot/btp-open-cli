package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

// cupsConcurrency bounds how many create/update API calls run at once across
// the whole planned batch.
const cupsConcurrency = 8

// cupsEntry is one source service record parsed from a service-credentials.json
// file (see the `service-credentials` command's output).
type cupsEntry struct {
	Name        string
	Label       string
	Plan        string
	Tags        []string
	Credentials map[string]interface{}
}

// cupsTargetSpace is one org/space pair that matched --space within a target
// org, along with the user-provided services that already exist there.
type cupsTargetSpace struct {
	OrgName   string
	SpaceGUID string
	SpaceName string
	Existing  map[string]string // lower(service name) → instance GUID
}

type cupsRegionResult struct {
	Region  string
	Client  *cf.Client
	Targets []cupsTargetSpace
	Err     error
}

// cupsPlannedItem is one user-provided service instance to create or update,
// after org/space resolution, --include/--exclude filtering, and --postfix.
type cupsPlannedItem struct {
	Client       *cf.Client
	OrgName      string
	SpaceGUID    string
	SpaceName    string
	ServiceName  string
	Label        string
	Plan         string
	Tags         []string
	Credentials  map[string]interface{}
	ExistingGUID string // non-empty if a same-named service already exists (update, not create)
}

var createUpsCmd = &cobra.Command{
	Use:   "create-ups <service-credentials.json>",
	Short: "Create user-provided services from a service-credentials.json export",
	Long: `Create (or update) user-provided services in a specific space across one or
more target orgs, using the service name, tags, and credentials taken from a
JSON file produced by 'bo service-credentials' — an array of objects with at
least "name" and "credentials"; "label", "plan", and "tags" are used when
present.

For each target org (--org/--orgs/--excludeOrgs, or the default scope set via
'bo orgs' if none of those is given) the space named --space is located, and
every filtered service from the input file is created there as a
user-provided service instance. The same set of services is created in every
target org's matching space — this broadcasts the file's contents; it does
not look at the file's own "org"/"space" fields to decide where to target
(those are informational only, carried over from 'bo service-credentials').

--postfix is appended to each service's name, e.g. "my-hdi" with
--postfix=-ups becomes "my-hdi-ups".

--include/--exclude each accept a comma-separated list of keywords; a
service is included/excluded if its label or name contains any of them
(case-insensitive).

Before creating anything, a preview table (org, space, service-name, label,
plan) is shown and confirmation is required unless -y is given; declining it
aborts the whole command. If any planned service already exists in its
target space, a second, separate confirmation to overwrite it is required
unless -y is given — declining only that one skips the existing (overwrite)
services and still creates the new ones, rather than aborting everything.

If --regions is omitted the regions from the last login are used.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		spaceName, _ := cmd.Flags().GetString("space")
		postfix, _ := cmd.Flags().GetString("postfix")
		skipConfirm, _ := cmd.Flags().GetBool("yes")
		includePattern, _ := cmd.Flags().GetString("include")
		excludePattern, _ := cmd.Flags().GetString("exclude")
		regionsFlag, _ := cmd.Flags().GetString("regions")
		orgsFile, _ := cmd.Flags().GetString("orgs")
		excludeOrgsFile, _ := cmd.Flags().GetString("excludeOrgs")
		orgGUID, _ := cmd.Flags().GetString("org")

		entries, err := parseCupsEntriesFile(args[0])
		if err != nil {
			return fmt.Errorf("invalid service-credentials JSON: %w", err)
		}

		var filtered []cupsEntry
		for _, e := range entries {
			if includePattern != "" && !skipMatches(includePattern, e.Label, e.Name) {
				continue
			}
			if excludePattern != "" && skipMatches(excludePattern, e.Label, e.Name) {
				continue
			}
			filtered = append(filtered, e)
		}
		if len(filtered) == 0 {
			return fmt.Errorf("no services left after --include/--exclude filtering")
		}

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

		// Phase 1: resolve target org+space (and existing UPS names in it)
		// per region, in parallel.
		results := make([]cupsRegionResult, len(apiURLs))
		var wg sync.WaitGroup
		for i, apiURL := range apiURLs {
			wg.Add(1)
			go func(idx int, url string) {
				defer wg.Done()
				regionName := store.APIURLToRegion(url)

				tok, ok := creds.Tokens[url]
				if !ok {
					results[idx] = cupsRegionResult{Region: regionName,
						Err: fmt.Errorf("no token — run: bo login --regions %s", regionName)}
					return
				}
				client := cf.NewClient(url, tok.AccessToken)
				client.SetTokenRefresher(makeTokenRefresher(url, tok.AccessToken))

				orgs, err := client.ListOrganizations(ctx)
				if err != nil {
					results[idx] = cupsRegionResult{Region: regionName, Client: client,
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
					results[idx] = cupsRegionResult{Region: regionName, Client: client}
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
					results[idx] = cupsRegionResult{Region: regionName, Client: client,
						Err: fmt.Errorf("listing spaces: %w", err)}
					return
				}

				var targets []cupsTargetSpace
				for _, sp := range spaces {
					if !strings.EqualFold(sp.Name, spaceName) {
						continue
					}
					orgGUIDForSpace := sp.Relationships.Organization.Data.GUID
					orgName := orgNameByGUID[orgGUIDForSpace]

					existingInstances, err := client.ListUserProvidedServiceInstancesInSpace(ctx, sp.GUID)
					if err != nil {
						fmt.Fprintf(os.Stderr, "warning: [%s] %s/%s: listing existing services: %v\n",
							regionName, orgName, sp.Name, err)
						continue
					}
					existing := make(map[string]string, len(existingInstances))
					for _, inst := range existingInstances {
						existing[strings.ToLower(inst.Name)] = inst.GUID
					}
					targets = append(targets, cupsTargetSpace{
						OrgName:   orgName,
						SpaceGUID: sp.GUID,
						SpaceName: sp.Name,
						Existing:  existing,
					})
				}

				results[idx] = cupsRegionResult{Region: regionName, Client: client, Targets: targets}
			}(i, apiURL)
		}
		wg.Wait()

		// Build the full plan: every (target org/space) × (filtered service).
		var planned []cupsPlannedItem
		for _, r := range results {
			if r.Err != nil {
				fmt.Fprintf(os.Stderr, "warning: region %q: %v\n", r.Region, r.Err)
				continue
			}
			for _, t := range r.Targets {
				for _, e := range filtered {
					finalName := e.Name + postfix
					planned = append(planned, cupsPlannedItem{
						Client:       r.Client,
						OrgName:      t.OrgName,
						SpaceGUID:    t.SpaceGUID,
						SpaceName:    t.SpaceName,
						ServiceName:  finalName,
						Label:        e.Label,
						Plan:         e.Plan,
						Tags:         e.Tags,
						Credentials:  e.Credentials,
						ExistingGUID: t.Existing[strings.ToLower(finalName)],
					})
				}
			}
		}
		if len(planned) == 0 {
			return fmt.Errorf("no target org has a space named %q (or no services matched --include/--exclude)", spaceName)
		}

		sort.Slice(planned, func(i, j int) bool {
			if planned[i].OrgName != planned[j].OrgName {
				return planned[i].OrgName < planned[j].OrgName
			}
			if planned[i].SpaceName != planned[j].SpaceName {
				return planned[i].SpaceName < planned[j].SpaceName
			}
			return planned[i].ServiceName < planned[j].ServiceName
		})

		out := cmd.OutOrStdout()
		fmt.Fprintln(out, "Services to create/update:")
		printCupsTable(out, planned)

		var existingItems []cupsPlannedItem
		createCount, updateCount := 0, 0
		for _, p := range planned {
			if p.ExistingGUID != "" {
				updateCount++
				existingItems = append(existingItems, p)
			} else {
				createCount++
			}
		}
		fmt.Fprintf(out, "\n%d to create, %d to update (already exist).\n", createCount, updateCount)

		if !skipConfirm {
			fmt.Fprint(out, "\nProceed? [y/N] ")
			text, ok := readLine(ctx)
			if !ok || strings.ToLower(text) != "y" {
				fmt.Fprintln(out, "Aborted.")
				return nil
			}
		}

		// toExecute starts as every planned item; declining the separate
		// overwrite confirmation below drops the existing (update) ones from
		// it without aborting the creates — the two confirmations gate
		// independent decisions, not one all-or-nothing batch.
		toExecute := planned
		if len(existingItems) > 0 && !skipConfirm {
			fmt.Fprintf(out, "\n%d service(s) already exist and will be overwritten:\n", len(existingItems))
			for _, p := range existingItems {
				fmt.Fprintf(out, "  %s / %s / %s\n", p.OrgName, p.SpaceName, p.ServiceName)
			}
			fmt.Fprint(out, "Overwrite existing service(s)? [y/N] ")
			text, ok := readLine(ctx)
			if !ok || strings.ToLower(text) != "y" {
				fmt.Fprintln(out, "Skipping existing services — proceeding with new ones only.")
				var newOnly []cupsPlannedItem
				for _, p := range planned {
					if p.ExistingGUID == "" {
						newOnly = append(newOnly, p)
					}
				}
				toExecute = newOnly
			}
		}
		if len(toExecute) == 0 {
			fmt.Fprintln(out, "Nothing to do.")
			return nil
		}

		// Phase 3: create/update, bounded concurrency across the whole batch.
		var (
			execWg                            sync.WaitGroup
			mu                                sync.Mutex
			createdCount, updatedCount, fails int
		)
		sem := make(chan struct{}, cupsConcurrency)
		for _, p := range toExecute {
			execWg.Add(1)
			sem <- struct{}{}
			go func(p cupsPlannedItem) {
				defer execWg.Done()
				defer func() { <-sem }()

				tags := p.Tags
				if tags == nil {
					tags = []string{}
				}

				if p.ExistingGUID != "" {
					if err := p.Client.UpdateUserProvidedServiceInstance(ctx, p.ExistingGUID, p.Credentials, tags); err != nil {
						mu.Lock()
						fails++
						mu.Unlock()
						fmt.Fprintf(os.Stderr, "  ! ERROR updating %s / %s / %s: %v\n", p.OrgName, p.SpaceName, p.ServiceName, err)
						return
					}
					mu.Lock()
					updatedCount++
					mu.Unlock()
					fmt.Fprintf(out, "  ~ updated: %s / %s / %s\n", p.OrgName, p.SpaceName, p.ServiceName)
					return
				}

				if err := p.Client.CreateUserProvidedServiceInstance(ctx, p.ServiceName, p.SpaceGUID, p.Credentials, tags); err != nil {
					mu.Lock()
					fails++
					mu.Unlock()
					fmt.Fprintf(os.Stderr, "  ! ERROR creating %s / %s / %s: %v\n", p.OrgName, p.SpaceName, p.ServiceName, err)
					return
				}
				mu.Lock()
				createdCount++
				mu.Unlock()
				fmt.Fprintf(out, "  + created: %s / %s / %s\n", p.OrgName, p.SpaceName, p.ServiceName)
			}(p)
		}
		execWg.Wait()

		fmt.Fprintf(out, "\nDone: %d created, %d updated, %d failed.\n", createdCount, updatedCount, fails)
		return nil
	},
}

// parseCupsEntriesFile reads and parses a service-credentials.json file (an
// array of objects as produced by `bo service-credentials`) into cupsEntry
// values. Entries without a usable "name" are skipped with a warning.
func parseCupsEntriesFile(path string) ([]cupsEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw []map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing JSON: %w", err)
	}

	entries := make([]cupsEntry, 0, len(raw))
	for i, r := range raw {
		name, _ := r["name"].(string)
		if name == "" {
			fmt.Fprintf(os.Stderr, "warning: entry %d has no usable \"name\" field — skipping\n", i)
			continue
		}
		credsMap, _ := r["credentials"].(map[string]interface{})
		if credsMap == nil {
			credsMap = map[string]interface{}{}
		}
		label, _ := r["label"].(string)
		plan, _ := r["plan"].(string)
		entries = append(entries, cupsEntry{
			Name:        name,
			Label:       label,
			Plan:        plan,
			Tags:        cupsStringSlice(r["tags"]),
			Credentials: credsMap,
		})
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no usable service entries found in %s", path)
	}
	return entries, nil
}

// cupsStringSlice converts a decoded JSON value (expected to be a []interface{}
// of strings, e.g. a "tags" array) into a []string, skipping non-string
// elements. Returns nil if v isn't a slice.
func cupsStringSlice(v interface{}) []string {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// printCupsTable prints the planned create/update items as an aligned table
// with columns org, space, service-name, label, plan.
func printCupsTable(w io.Writer, items []cupsPlannedItem) {
	headers := []string{"ORG", "SPACE", "SERVICE-NAME", "LABEL", "PLAN"}
	rows := make([][]string, len(items))
	for i, p := range items {
		rows[i] = []string{p.OrgName, p.SpaceName, p.ServiceName, p.Label, p.Plan}
	}

	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, v := range row {
			if len(v) > widths[i] {
				widths[i] = len(v)
			}
		}
	}

	printRow := func(cols []string) {
		parts := make([]string, len(cols))
		for i, v := range cols {
			parts[i] = fmt.Sprintf("%-*s", widths[i], v)
		}
		fmt.Fprintln(w, strings.TrimRight(strings.Join(parts, "  "), " "))
	}
	printRow(headers)
	for _, row := range rows {
		printRow(row)
	}
}

func init() {
	createUpsCmd.GroupID = "cf-org"
	rootCmd.AddCommand(createUpsCmd)
	createUpsCmd.Flags().String("space", "", "Target space name (exact, case-insensitive) within each target org to create the services in (required)")
	createUpsCmd.Flags().String("postfix", "", "Suffix appended to each service name, e.g. '-ups'")
	createUpsCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompts")
	createUpsCmd.Flags().String("include", "", "Only include services whose label or name contain any of these comma-separated, case-insensitive keywords")
	createUpsCmd.Flags().String("exclude", "", "Exclude services whose label or name contain any of these comma-separated, case-insensitive keywords")
	createUpsCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	createUpsCmd.Flags().String("org", "", "Org GUID to target; only this org will be targeted")
	createUpsCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	createUpsCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,org_id,org_name)")
	_ = createUpsCmd.MarkFlagRequired("space")
}
