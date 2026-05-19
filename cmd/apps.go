package cmd

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"

	toonenc "github.com/toon-format/toon-go"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

// ── output types ──────────────────────────────────────────────────────────────

type appsOutApp struct {
	MtaID      string `json:"mta_id"               toon:"mta_id"`
	ID         string `json:"app_id"               toon:"app_id"`
	Name       string `json:"app_name"             toon:"app_name"`
	State      string `json:"app_state"            toon:"app_state"`
	CreatedAt  string `json:"app_created_at"       toon:"app_created_at"`
	UpdatedAt  string `json:"app_updated_at"       toon:"app_updated_at"`
	Instances  int    `json:"process_instances"    toon:"process_instances"`
	MemoryInMB int    `json:"process_memory_in_mb" toon:"process_memory_in_mb"`
	DiskInMB   int    `json:"process_disk_in_mb"   toon:"process_disk_in_mb"`
}

type appsOutSpace struct {
	ID   string       `json:"space_id"   toon:"space_id"`
	Name string       `json:"space_name" toon:"space_name"`
	Apps []appsOutApp `json:"apps"       toon:"apps"`
}

type appsOutOrg struct {
	ID     string         `json:"org_id"   toon:"org_id"`
	Name   string         `json:"org_name" toon:"org_name"`
	Spaces []appsOutSpace `json:"spaces"   toon:"spaces"`
}

type appsOutRegion struct {
	ID   string       `json:"region_id" toon:"region_id"`
	Orgs []appsOutOrg `json:"orgs"      toon:"orgs"`
}

type appsOutDoc struct {
	Regions []appsOutRegion `json:"regions" toon:"regions"`
}

// ── fetch result ──────────────────────────────────────────────────────────────

type appsRegionResult struct {
	Region string
	Orgs   []cf.Organization
	Spaces []cf.Space
	Apps   []cf.App
	Procs  map[string]cf.Process // appGUID → web process
	Err    error
}

// ── command ───────────────────────────────────────────────────────────────────

var appsCmd = &cobra.Command{
	Use:   "apps",
	Short: "List apps across all accessible organizations",
	Long: `List Cloud Foundry applications across one or more regions and organizations.

For each region the command fetches organizations, spaces, apps, and web
process metrics in parallel, then assembles the result.

Output formats (--format):
  toon  Token-Oriented Object Notation — compact, human-readable (default)
  json  JSON document
  csv   Flat CSV rows, one per app

If --regions is omitted the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		orgsFile, _ := cmd.Flags().GetString("orgs")
		excludeOrgsFile, _ := cmd.Flags().GetString("excludeOrgs")
		orgGUID, _ := cmd.Flags().GetString("org")
		format, _ := cmd.Flags().GetString("format")
		filter, _ := cmd.Flags().GetString("filter")

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

		results := make([]appsRegionResult, len(apiURLs))
		var wg sync.WaitGroup

		for i, apiURL := range apiURLs {
			wg.Add(1)
			go func(idx int, url string) {
				defer wg.Done()
				regionName := store.APIURLToRegion(url)
				slog.Debug("fetching apps", "region", regionName)

				tok, ok := creds.Tokens[url]
				if !ok {
					results[idx] = appsRegionResult{Region: regionName,
						Err: fmt.Errorf("no token — run: bo login --regions %s", regionName)}
					return
				}

				client := cf.NewClient(url, tok.AccessToken)

				// Step 1: orgs.
				orgs, err := client.ListOrganizations(ctx)
				if err != nil {
					results[idx] = appsRegionResult{Region: regionName,
						Err: fmt.Errorf("listing orgs: %w", err)}
					return
				}

				// Apply org filters.
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
					results[idx] = appsRegionResult{Region: regionName}
					return
				}
				slog.Debug("orgs filtered", "region", regionName, "count", len(filteredOrgs))

				// Step 2: spaces filtered by org GUIDs.
				orgGUIDs := make([]string, len(filteredOrgs))
				for i, o := range filteredOrgs {
					orgGUIDs[i] = o.GUID
				}
				spaces, err := client.ListSpacesByOrgs(ctx, orgGUIDs)
				if err != nil {
					results[idx] = appsRegionResult{Region: regionName,
						Err: fmt.Errorf("listing spaces: %w", err)}
					return
				}
				slog.Debug("spaces fetched", "region", regionName, "count", len(spaces))

				if len(spaces) == 0 {
					results[idx] = appsRegionResult{Region: regionName, Orgs: filteredOrgs}
					return
				}

				spaceGUIDs := make([]string, len(spaces))
				for i, s := range spaces {
					spaceGUIDs[i] = s.GUID
				}

				// Steps 3+4: apps and processes in parallel.
				var apps []cf.App
				var procs map[string]cf.Process
				var appsErr, procsErr error
				var innerWg sync.WaitGroup
				innerWg.Add(2)

				go func() {
					defer innerWg.Done()
					apps, appsErr = client.ListAppsBySpaces(ctx, spaceGUIDs)
				}()
				go func() {
					defer innerWg.Done()
					procs, procsErr = client.ListProcessesBySpaces(ctx, spaceGUIDs)
				}()
				innerWg.Wait()

				if appsErr != nil {
					results[idx] = appsRegionResult{Region: regionName,
						Err: fmt.Errorf("listing apps: %w", appsErr)}
					return
				}
				if procsErr != nil {
					results[idx] = appsRegionResult{Region: regionName,
						Err: fmt.Errorf("listing processes: %w", procsErr)}
					return
				}
				slog.Debug("apps and processes fetched", "region", regionName,
					"apps", len(apps), "procs", len(procs))

				results[idx] = appsRegionResult{
					Region: regionName,
					Orgs:   filteredOrgs,
					Spaces: spaces,
					Apps:   apps,
					Procs:  procs,
				}
			}(i, apiURL)
		}
		wg.Wait()

		switch strings.ToLower(format) {
		case "json":
			return writeAppsJSON(results, filter)
		case "csv":
			return writeAppsCSV(results, filter)
		default:
			return writeAppsToon(results, filter)
		}
	},
}

// ── output builders ───────────────────────────────────────────────────────────

func buildAppsDoc(results []appsRegionResult, filter string) (appsOutDoc, []error) {
	var doc appsOutDoc
	var errs []error

	for _, r := range results {
		if r.Err != nil {
			errs = append(errs, fmt.Errorf("region %q: %w", r.Region, r.Err))
			continue
		}

		// Index spaces and apps by their parent GUIDs.
		spacesByOrg := make(map[string][]cf.Space)
		for _, s := range r.Spaces {
			orgGUID := s.Relationships.Organization.Data.GUID
			spacesByOrg[orgGUID] = append(spacesByOrg[orgGUID], s)
		}
		appsBySpace := make(map[string][]cf.App)
		for _, a := range r.Apps {
			sguid := a.Relationships.Space.Data.GUID
			appsBySpace[sguid] = append(appsBySpace[sguid], a)
		}

		or := appsOutRegion{ID: r.Region}
		for _, org := range r.Orgs {
			oo := appsOutOrg{ID: org.GUID, Name: org.Name}
			for _, sp := range spacesByOrg[org.GUID] {
				os_ := appsOutSpace{ID: sp.GUID, Name: sp.Name}
				for _, app := range appsBySpace[sp.GUID] {
					proc := r.Procs[app.GUID]
					a := appsOutApp{
						MtaID:      app.Metadata.Annotations.MtaID,
						ID:         app.GUID,
						Name:       app.Name,
						State:      app.State,
						CreatedAt:  app.CreatedAt,
						UpdatedAt:  app.UpdatedAt,
						Instances:  proc.Instances,
						MemoryInMB: proc.MemoryInMB,
						DiskInMB:   proc.DiskInMB,
					}
					if appsMatchesFilter(a, filter) {
						os_.Apps = append(os_.Apps, a)
					}
				}
				if len(os_.Apps) > 0 {
					oo.Spaces = append(oo.Spaces, os_)
				}
			}
			if len(oo.Spaces) > 0 {
				or.Orgs = append(or.Orgs, oo)
			}
		}
		if len(or.Orgs) > 0 {
			doc.Regions = append(doc.Regions, or)
		}
	}
	return doc, errs
}

func writeAppsToon(results []appsRegionResult, filter string) error {
	doc, errs := buildAppsDoc(results, filter)
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}
	out, err := toonenc.Marshal(doc, toonenc.WithIndent(2))
	if err != nil {
		return fmt.Errorf("encoding TOON: %w", err)
	}
	if _, err = os.Stdout.Write(out); err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout)
	return err
}

func writeAppsJSON(results []appsRegionResult, filter string) error {
	doc, errs := buildAppsDoc(results, filter)
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding JSON: %w", err)
	}
	fmt.Fprintln(os.Stdout, string(out))
	return nil
}

func writeAppsCSV(results []appsRegionResult, filter string) error {
	doc, errs := buildAppsDoc(results, filter)
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}

	w := csv.NewWriter(os.Stdout)
	defer w.Flush()

	if err := w.Write([]string{
		"region_id", "org_id", "org_name",
		"space_id", "space_name",
		"app_mta_id", "app_id", "app_name", "app_state",
		"app_created_at", "app_updated_at",
		"process_instances", "process_memory_in_mb", "process_disk_in_mb",
	}); err != nil {
		return err
	}

	for _, r := range doc.Regions {
		for _, o := range r.Orgs {
			for _, sp := range o.Spaces {
				for _, a := range sp.Apps {
					if err := w.Write([]string{
						r.ID, o.ID, o.Name,
						sp.ID, sp.Name,
						a.MtaID, a.ID, a.Name, a.State,
						a.CreatedAt, a.UpdatedAt,
						strconv.Itoa(a.Instances),
						strconv.Itoa(a.MemoryInMB),
						strconv.Itoa(a.DiskInMB),
					}); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// appsMatchesFilter reports whether the app matches the filter string
// (case-insensitive substring against mta_id, app_id, app_name, app_state,
// app_created_at, app_updated_at, and process_memory_in_mb).
func appsMatchesFilter(a appsOutApp, filter string) bool {
	if filter == "" {
		return true
	}
	fl := strings.ToLower(filter)
	return strings.Contains(strings.ToLower(a.MtaID), fl) ||
		strings.Contains(strings.ToLower(a.ID), fl) ||
		strings.Contains(strings.ToLower(a.Name), fl) ||
		strings.Contains(strings.ToLower(a.State), fl) ||
		strings.Contains(strings.ToLower(a.CreatedAt), fl) ||
		strings.Contains(strings.ToLower(a.UpdatedAt), fl) ||
		strings.Contains(strconv.Itoa(a.MemoryInMB), filter)
}

func init() {
	rootCmd.AddCommand(appsCmd)
	appsCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	appsCmd.Flags().String("org", "", "Org GUID to target; only apps from this org will be fetched")
	appsCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	appsCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,org_id,org_name)")
	appsCmd.Flags().String("format", "toon", "Output format: toon (default), json, or csv")
	appsCmd.Flags().String("filter", "", "Case-insensitive substring filter on mta_id, app_id, app_name, app_state, app_created_at, app_updated_at, process_memory_in_mb")
}
