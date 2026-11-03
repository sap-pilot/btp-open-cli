package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	MtaID         string `json:"mta_id"               toon:"mta_id"`
	ID            string `json:"app_id"               toon:"app_id"`
	Name          string `json:"app_name"             toon:"app_name"`
	State         string `json:"app_state"            toon:"app_state"`
	CreatedAt     string `json:"app_created_at"       toon:"app_created_at"`
	UpdatedAt     string `json:"app_updated_at"       toon:"app_updated_at"`
	Instances     int    `json:"process_instances"     toon:"process_instances"`
	MemoryInMB    int    `json:"process_memory_in_mb"  toon:"process_memory_in_mb"`
	DiskInMB      int    `json:"process_disk_in_mb"    toon:"process_disk_in_mb"`
	TotalMemoryMB int    `json:"app_total_memory_mb"   toon:"app_total_memory_mb"`

	// Env holds the app's own user-provided environment variables (not
	// VCAP_SERVICES or any other derived/system environment) — populated
	// only when --include-env is given, and otherwise omitted entirely.
	Env map[string]string `json:"app_env,omitempty" toon:"app_env,omitempty"`
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
	Client *cf.Client // non-nil whenever a token was found, even on a later error
	Orgs   []cf.Organization
	Spaces []cf.Space
	Apps   []cf.App
	Procs  map[string]cf.Process // appGUID → web process
	Err    error
}

// appsEnvConcurrency bounds how many /v3/apps/:guid/environment_variables
// calls run at once across the whole matched app set when --include-env is
// given, so a large result set doesn't open hundreds of simultaneous
// requests against the CF API.
const appsEnvConcurrency = 8

// appsSkippedEnvKeys holds env var names (matched case-insensitively)
// excluded from --include-env output. DEPLOY_ATTRIBUTES is injected by
// deploy tooling (not something a user set), is specific to the
// app/environment it was deployed from, and isn't meant to be replayed onto
// another org/space via 'bo update-apps-env'. This is the only filtering
// applied: secret/password/key-named variables are deliberately included in
// full, unmasked — they're exactly the values 'bo update-apps-env' needs to
// recreate on a target app.
var appsSkippedEnvKeys = map[string]bool{
	"DEPLOY_ATTRIBUTES": true,
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

If neither --org nor --orgs is given, the default org scope selected via
'bo orgs' is used. If no default scope has been set either, run 'bo orgs' to
pick one interactively, or pass --org/--orgs directly.

Use --include/--exclude to narrow results further: each accepts a
comma-separated list of keywords, and an app matches if mta_id, app_id,
app_name, app_state, app_created_at, app_updated_at, or process_memory_in_mb
contains any of them (case-insensitive).

Use --output/-o to write the result to a file instead of stdout. If --format
is not explicitly given, it is auto-detected from --output's extension:
.json selects json, .csv selects csv — otherwise (including no --output)
the default toon applies.

Use --include-env to also fetch each app's own user-provided environment
variables (as "app_env" — not VCAP_SERVICES or any other derived/system
environment) via one extra API call per app; fetching it requires at least
Space Developer access to the app's space, and a per-app failure is reported
as a warning on stderr rather than aborting the whole command. Secret/
password/key-named variables are included in full, never masked or
skipped — they're exactly what 'bo update-apps-env' needs to recreate on a
target app. The only variable excluded is DEPLOY_ATTRIBUTES, injected by
deploy tooling rather than set by a user and not meaningful to replay onto
another org/space. The written file (JSON or TOON) can be fed straight into
'bo update-apps-env' to replay those same values onto a different
org/space. This is sensitive output — treat it the same as
'bo service-credentials'.

If --regions is omitted the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		orgsFile, _ := cmd.Flags().GetString("orgs")
		excludeOrgsFile, _ := cmd.Flags().GetString("excludeOrgs")
		orgGUID, _ := cmd.Flags().GetString("org")
		outputFile, _ := cmd.Flags().GetString("output")
		format, _ := cmd.Flags().GetString("format")
		if !cmd.Flags().Changed("format") {
			if auto := autoFormatFromOutputExt(outputFile, map[string]string{"json": ".json", "csv": ".csv"}); auto != "" {
				format = auto
			}
		}
		includePattern, _ := cmd.Flags().GetString("include")
		excludePattern, _ := cmd.Flags().GetString("exclude")

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
				client.SetTokenRefresher(makeTokenRefresher(url, tok.AccessToken))

				// Step 1: orgs.
				orgs, err := client.ListOrganizations(ctx)
				if err != nil {
					results[idx] = appsRegionResult{Region: regionName, Client: client,
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
					results[idx] = appsRegionResult{Region: regionName, Client: client}
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
					results[idx] = appsRegionResult{Region: regionName, Client: client,
						Err: fmt.Errorf("listing spaces: %w", err)}
					return
				}
				slog.Debug("spaces fetched", "region", regionName, "count", len(spaces))

				if len(spaces) == 0 {
					results[idx] = appsRegionResult{Region: regionName, Client: client, Orgs: filteredOrgs}
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
					results[idx] = appsRegionResult{Region: regionName, Client: client,
						Err: fmt.Errorf("listing apps: %w", appsErr)}
					return
				}
				if procsErr != nil {
					results[idx] = appsRegionResult{Region: regionName, Client: client,
						Err: fmt.Errorf("listing processes: %w", procsErr)}
					return
				}
				slog.Debug("apps and processes fetched", "region", regionName,
					"apps", len(apps), "procs", len(procs))

				results[idx] = appsRegionResult{
					Region: regionName,
					Client: client,
					Orgs:   filteredOrgs,
					Spaces: spaces,
					Apps:   apps,
					Procs:  procs,
				}
			}(i, apiURL)
		}
		wg.Wait()

		doc, errs := buildAppsDoc(results, includePattern, excludePattern)
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "warning: %v\n", e)
		}

		if includeEnv, _ := cmd.Flags().GetBool("include-env"); includeEnv {
			enrichAppsDocWithEnv(ctx, &doc, results)
		}

		out, closeOut, err := resolveOutputWriter(outputFile)
		if err != nil {
			return err
		}
		defer closeOut()
		colorMode, _ := cmd.Flags().GetString("color")
		color := consoleColorEnabled(outputFile, colorMode)

		switch strings.ToLower(format) {
		case "json":
			return renderAppsJSON(out, doc)
		case "csv":
			return renderAppsCSV(out, doc, color)
		default:
			return renderAppsToon(out, doc, color)
		}
	},
}

// enrichAppsDocWithEnv fetches each app's own user-provided environment
// variables (bounded concurrency across the whole doc) and populates its
// Env field in place. results supplies the already-authenticated client for
// each region; an app whose region has no client (e.g. a region-level error)
// is left without Env. A per-app fetch failure is reported as a warning on
// stderr and that app is simply left without Env, rather than aborting the
// whole command — fetching environment requires at least Space Developer
// access, which isn't guaranteed for every app in scope.
func enrichAppsDocWithEnv(ctx context.Context, doc *appsOutDoc, results []appsRegionResult) {
	clientByRegion := make(map[string]*cf.Client, len(results))
	for _, r := range results {
		if r.Client != nil {
			clientByRegion[r.Region] = r.Client
		}
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, appsEnvConcurrency)
	for ri := range doc.Regions {
		client := clientByRegion[doc.Regions[ri].ID]
		if client == nil {
			continue
		}
		for oi := range doc.Regions[ri].Orgs {
			for si := range doc.Regions[ri].Orgs[oi].Spaces {
				for ai := range doc.Regions[ri].Orgs[oi].Spaces[si].Apps {
					app := &doc.Regions[ri].Orgs[oi].Spaces[si].Apps[ai]
					wg.Add(1)
					sem <- struct{}{}
					go func(app *appsOutApp) {
						defer wg.Done()
						defer func() { <-sem }()
						env, err := client.GetAppEnvVars(ctx, app.ID)
						if err != nil {
							fmt.Fprintf(os.Stderr, "warning: app %q: fetching environment variables: %v\n", app.Name, err)
							return
						}
						for k := range env {
							if appsSkippedEnvKeys[strings.ToUpper(k)] {
								delete(env, k)
							}
						}
						app.Env = env
					}(app)
				}
			}
		}
	}
	wg.Wait()
}

// ── output builders ───────────────────────────────────────────────────────────

func buildAppsDoc(results []appsRegionResult, includePattern, excludePattern string) (appsOutDoc, []error) {
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
						MtaID:         app.Metadata.Annotations.MtaID,
						ID:            app.GUID,
						Name:          app.Name,
						State:         app.State,
						CreatedAt:     app.CreatedAt,
						UpdatedAt:     app.UpdatedAt,
						Instances:     proc.Instances,
						MemoryInMB:    proc.MemoryInMB,
						DiskInMB:      proc.DiskInMB,
						TotalMemoryMB: proc.Instances * proc.MemoryInMB,
					}
					if appsMatchesIncludeExclude(a, includePattern, excludePattern) {
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

func renderAppsToon(w io.Writer, doc appsOutDoc, color bool) error {
	out, err := toonenc.Marshal(doc, toonenc.WithIndent(2))
	if err != nil {
		return fmt.Errorf("encoding TOON: %w", err)
	}
	out = colorizeToon(out, color)
	if _, err = w.Write(out); err != nil {
		return err
	}
	_, err = fmt.Fprintln(w)
	return err
}

func renderAppsJSON(w io.Writer, doc appsOutDoc) error {
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding JSON: %w", err)
	}
	fmt.Fprintln(w, string(out))
	return nil
}

func renderAppsCSV(w io.Writer, doc appsOutDoc, color bool) error {
	csvW := newRainbowCSVWriter(w, color)
	defer csvW.Flush()

	if err := csvW.Write([]string{
		"region_id", "org_id", "org_name",
		"space_id", "space_name",
		"app_mta_id", "app_id", "app_name", "app_state",
		"app_created_at", "app_updated_at",
		"process_instances", "process_memory_in_mb", "process_disk_in_mb", "app_total_memory_mb",
		"app_env",
	}); err != nil {
		return err
	}

	for _, r := range doc.Regions {
		for _, o := range r.Orgs {
			for _, sp := range o.Spaces {
				for _, a := range sp.Apps {
					var envCol string
					if len(a.Env) > 0 {
						envJSON, err := json.Marshal(a.Env)
						if err != nil {
							return fmt.Errorf("encoding app_env for %q: %w", a.Name, err)
						}
						envCol = string(envJSON)
					}
					if err := csvW.Write([]string{
						r.ID, o.ID, o.Name,
						sp.ID, sp.Name,
						a.MtaID, a.ID, a.Name, a.State,
						a.CreatedAt, a.UpdatedAt,
						strconv.Itoa(a.Instances),
						strconv.Itoa(a.MemoryInMB),
						strconv.Itoa(a.DiskInMB),
						strconv.Itoa(a.TotalMemoryMB),
						envCol,
					}); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// appsMatchesIncludeExclude applies --include/--exclude keyword filtering
// (comma-separated, case-insensitive, matched if any keyword is a substring
// of any field) against mta_id, app_id, app_name, app_state, app_created_at,
// app_updated_at, and process_memory_in_mb.
func appsMatchesIncludeExclude(a appsOutApp, includePattern, excludePattern string) bool {
	fields := []string{
		a.MtaID, a.ID, a.Name, a.State, a.CreatedAt, a.UpdatedAt, strconv.Itoa(a.MemoryInMB),
	}
	if includePattern != "" && !skipMatches(includePattern, fields...) {
		return false
	}
	if excludePattern != "" && skipMatches(excludePattern, fields...) {
		return false
	}
	return true
}

func init() {
	appsCmd.GroupID = "cf-org"
	rootCmd.AddCommand(appsCmd)
	appsCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	appsCmd.Flags().String("org", "", "Org GUID to target; only apps from this org will be fetched")
	appsCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	appsCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,org_id,org_name)")
	appsCmd.Flags().StringP("output", "o", "", "Write output to this file instead of stdout (use this, not shell '>', since the interactive org picker also writes to stdout)")
	appsCmd.Flags().String("format", "toon", "Output format: toon (default), json, or csv; auto-detected from --output's extension (.json or .csv) when --format is not explicitly given")
	appsCmd.Flags().String("include", "", "Only include apps where mta_id, app_id, app_name, app_state, app_created_at, app_updated_at, or process_memory_in_mb contain any of these comma-separated keywords")
	appsCmd.Flags().String("exclude", "", "Exclude apps where mta_id, app_id, app_name, app_state, app_created_at, app_updated_at, or process_memory_in_mb contain any of these comma-separated keywords")
	appsCmd.Flags().Bool("include-env", false, "Also fetch and include each app's own user-provided environment variables (app_env) — requires Space Developer access per app; this is sensitive output, see README")
}
