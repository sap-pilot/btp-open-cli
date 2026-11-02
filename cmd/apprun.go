package cmd

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"

	toonenc "github.com/toon-format/toon-go"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

// sawConcurrency bounds how many start/stop API calls run at once across the
// whole matched set.
const sawConcurrency = 8

// sawMatchedApp is one app that passed org/space/name-pattern filtering,
// annotated with the org/space names and the already-authenticated client
// for its region.
type sawMatchedApp struct {
	Client    *cf.Client
	OrgName   string
	SpaceName string
	AppGUID   string
	AppName   string
	State     string
}

type sawRegionResult struct {
	Region string
	Apps   []sawMatchedApp
	Err    error
}

// ── TOON preview document ────────────────────────────────────────────────────

type sawPreviewApp struct {
	AppGUID string `toon:"app_guid"`
	AppName string `toon:"app_name"`
	State   string `toon:"state"`
}

type sawPreviewSpace struct {
	SpaceName string          `toon:"space_name"`
	Apps      []sawPreviewApp `toon:"apps"`
}

type sawPreviewOrg struct {
	OrgName string            `toon:"org_name"`
	Spaces  []sawPreviewSpace `toon:"spaces"`
}

type sawPreviewDoc struct {
	Orgs []sawPreviewOrg `toon:"orgs"`
}

// resolveMatchedApps resolves every app whose name matches patternsCSV
// (comma-separated patterns, each a glob via filepath.Match if it contains
// * ? [, otherwise a case-insensitive substring; a '!'-prefixed pattern
// reverses that one pattern — see matchesNegatablePattern), across the
// region/org/space scope selected by the command's --regions/--org/--orgs/
// --excludeOrgs/--spaces/--space flags, in parallel across regions.
func resolveMatchedApps(ctx context.Context, cmd *cobra.Command, patternsCSV string) ([]sawMatchedApp, error) {
	creds, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("not logged in — run: bo login --regions <region>")
	}

	appsFile, _ := cmd.Flags().GetString("apps")
	if appsFile != "" {
		warnIgnoredScopeFlagsForAppsCSV(cmd)
		return resolveMatchedAppsFromCSV(ctx, creds, appsFile, patternsCSV)
	}

	regionsFlag, _ := cmd.Flags().GetString("regions")
	orgsFile, _ := cmd.Flags().GetString("orgs")
	excludeOrgsFile, _ := cmd.Flags().GetString("excludeOrgs")
	orgGUID, _ := cmd.Flags().GetString("org")
	spacesFlag, _ := cmd.Flags().GetString("spaces")
	spaceFlag, _ := cmd.Flags().GetString("space")

	if spaceFlag != "" {
		if spacesFlag != "" {
			spacesFlag += "," + spaceFlag
		} else {
			spacesFlag = spaceFlag
		}
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
		return nil, fmt.Errorf("no regions configured — run: bo login --regions <region1,region2>")
	}

	var includeOrgs cosOrgSet
	if orgsFile != "" {
		includeOrgs, err = parseCosOrgCSV(orgsFile)
		if err != nil {
			return nil, fmt.Errorf("invalid --orgs CSV: %w", err)
		}
	}
	var excludeOrgs cosOrgSet
	if excludeOrgsFile != "" {
		excludeOrgs, err = parseCosOrgCSV(excludeOrgsFile)
		if err != nil {
			return nil, fmt.Errorf("invalid --excludeOrgs CSV: %w", err)
		}
	}

	if orgGUID == "" && orgsFile == "" {
		includeOrgs, err = resolveDefaultOrgScope(creds)
		if err != nil {
			return nil, err
		}
	}

	results := make([]sawRegionResult, len(apiURLs))
	var wg sync.WaitGroup
	for i, apiURL := range apiURLs {
		wg.Add(1)
		go func(idx int, url string) {
			defer wg.Done()
			regionName := store.APIURLToRegion(url)

			tok, ok := creds.Tokens[url]
			if !ok {
				results[idx] = sawRegionResult{Region: regionName,
					Err: fmt.Errorf("no token — run: bo login --regions %s", regionName)}
				return
			}
			client := cf.NewClient(url, tok.AccessToken)
			client.SetTokenRefresher(makeTokenRefresher(url, tok.AccessToken))

			orgs, err := client.ListOrganizations(ctx)
			if err != nil {
				results[idx] = sawRegionResult{Region: regionName,
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
				results[idx] = sawRegionResult{Region: regionName}
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
				results[idx] = sawRegionResult{Region: regionName,
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
				results[idx] = sawRegionResult{Region: regionName}
				return
			}

			apps, err := client.ListAppsBySpaces(ctx, spaceGUIDs)
			if err != nil {
				results[idx] = sawRegionResult{Region: regionName,
					Err: fmt.Errorf("listing apps: %w", err)}
				return
			}

			var matched []sawMatchedApp
			for _, a := range apps {
				if !matchesNegatablePattern(patternsCSV, a.Name) {
					continue
				}
				info := spaceByGUID[a.Relationships.Space.Data.GUID]
				matched = append(matched, sawMatchedApp{
					Client:    client,
					OrgName:   info.OrgName,
					SpaceName: info.Name,
					AppGUID:   a.GUID,
					AppName:   a.Name,
					State:     a.State,
				})
			}

			results[idx] = sawRegionResult{Region: regionName, Apps: matched}
		}(i, apiURL)
	}
	wg.Wait()

	var allMatched []sawMatchedApp
	for _, r := range results {
		if r.Err != nil {
			fmt.Fprintf(os.Stderr, "warning: region %q: %v\n", r.Region, r.Err)
			continue
		}
		allMatched = append(allMatched, r.Apps...)
	}

	sortMatchedApps(allMatched)

	return allMatched, nil
}

// sortMatchedApps sorts matched apps by org name, then space name, then app
// name, for a deterministic preview/execution order.
func sortMatchedApps(matched []sawMatchedApp) {
	sort.Slice(matched, func(i, j int) bool {
		if matched[i].OrgName != matched[j].OrgName {
			return matched[i].OrgName < matched[j].OrgName
		}
		if matched[i].SpaceName != matched[j].SpaceName {
			return matched[i].SpaceName < matched[j].SpaceName
		}
		return matched[i].AppName < matched[j].AppName
	})
}

// appCSVRef is one row of an --apps CSV, as produced by `bo apps --format
// csv` (or a hand-trimmed subset of it) — used to target specific apps
// directly by GUID, bypassing org/space discovery and the `bo orgs` default
// scope entirely.
type appCSVRef struct {
	Region    string
	OrgName   string
	SpaceGUID string
	SpaceName string
	AppGUID   string
	AppName   string
}

// appsCSVRequiredColumns are the columns an --apps CSV must have, matching
// the header `bo apps --format csv` writes; any other columns (app_state,
// app_created_at, ...) are ignored.
var appsCSVRequiredColumns = []string{"region_id", "org_name", "space_id", "space_name", "app_id", "app_name"}

// parseAppsCSV reads a CSV of app references for --apps. The header must
// contain appsCSVRequiredColumns, in any order.
func parseAppsCSV(path string) ([]appCSVRef, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening file: %w", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("reading header: %w", err)
	}
	col := make(map[string]int, len(header))
	for i, h := range header {
		col[strings.TrimSpace(h)] = i
	}
	idx := make(map[string]int, len(appsCSVRequiredColumns))
	minCols := 0
	for _, name := range appsCSVRequiredColumns {
		i, ok := col[name]
		if !ok {
			return nil, fmt.Errorf("invalid header — must include columns: %s (any order)",
				strings.Join(appsCSVRequiredColumns, ", "))
		}
		idx[name] = i
		if i+1 > minCols {
			minCols = i + 1
		}
	}

	var refs []appCSVRef
	for line := 2; ; line++ {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if len(row) < minCols {
			return nil, fmt.Errorf("line %d: expected at least %d columns, got %d", line, minCols, len(row))
		}
		refs = append(refs, appCSVRef{
			Region:    strings.TrimSpace(row[idx["region_id"]]),
			OrgName:   strings.TrimSpace(row[idx["org_name"]]),
			SpaceGUID: strings.TrimSpace(row[idx["space_id"]]),
			SpaceName: strings.TrimSpace(row[idx["space_name"]]),
			AppGUID:   strings.TrimSpace(row[idx["app_id"]]),
			AppName:   strings.TrimSpace(row[idx["app_name"]]),
		})
	}
	return refs, nil
}

// apiURLForRegion finds the already-authenticated API URL for region among
// creds.Tokens (matched via store.APIURLToRegion), so --apps CSV rows only
// ever hit a region the current login session already has a token for,
// rather than guessing a URL from the region code.
func apiURLForRegion(creds *store.Credentials, region string) (string, bool) {
	for apiURL := range creds.Tokens {
		if strings.EqualFold(store.APIURLToRegion(apiURL), region) {
			return apiURL, true
		}
	}
	return "", false
}

// scopeFlagsIgnoredByAppsCSV lists the org/space targeting flags that --apps
// (direct CSV targeting) makes moot, since each CSV row already carries its
// own exact region/org/space/app. The `bo orgs` default scope isn't a flag,
// so it's mentioned separately by the caller.
var scopeFlagsIgnoredByAppsCSV = []string{"regions", "org", "orgs", "excludeOrgs", "spaces", "space"}

// warnIgnoredScopeFlagsForAppsCSV warns when --apps is combined with any
// org/space targeting flag that it silently overrides.
func warnIgnoredScopeFlagsForAppsCSV(cmd *cobra.Command) {
	var ignored []string
	for _, name := range scopeFlagsIgnoredByAppsCSV {
		if cmd.Flags().Changed(name) {
			ignored = append(ignored, "--"+name)
		}
	}
	if len(ignored) > 0 {
		fmt.Fprintf(os.Stderr, "warning: --apps targets apps directly by region/org/space from the CSV; ignoring %s (and any 'bo orgs' default scope)\n",
			strings.Join(ignored, ", "))
	}
}

// resolveMatchedAppsFromCSV implements --apps: resolves the exact apps
// listed in an app CSV (see parseAppsCSV), filtered by patternsCSV against
// each row's app_name, fetching each app's live state directly by GUID
// (grouped by space, per region) rather than discovering it via org/space
// listing.
func resolveMatchedAppsFromCSV(ctx context.Context, creds *store.Credentials, path, patternsCSV string) ([]sawMatchedApp, error) {
	refs, err := parseAppsCSV(path)
	if err != nil {
		return nil, fmt.Errorf("invalid --apps CSV: %w", err)
	}

	var filtered []appCSVRef
	for _, ref := range refs {
		if matchesNegatablePattern(patternsCSV, ref.AppName) {
			filtered = append(filtered, ref)
		}
	}
	if len(filtered) == 0 {
		return nil, nil
	}

	var regionOrder []string
	byRegion := make(map[string][]appCSVRef)
	for _, ref := range filtered {
		if _, ok := byRegion[ref.Region]; !ok {
			regionOrder = append(regionOrder, ref.Region)
		}
		byRegion[ref.Region] = append(byRegion[ref.Region], ref)
	}

	var allMatched []sawMatchedApp
	for _, region := range regionOrder {
		regionRefs := byRegion[region]
		apiURL, ok := apiURLForRegion(creds, region)
		if !ok {
			fmt.Fprintf(os.Stderr, "warning: region %q: no token — run: bo login --regions %s\n", region, region)
			continue
		}
		tok := creds.Tokens[apiURL]
		client := cf.NewClient(apiURL, tok.AccessToken)
		client.SetTokenRefresher(makeTokenRefresher(apiURL, tok.AccessToken))

		seenSpace := make(map[string]bool, len(regionRefs))
		var spaceGUIDs []string
		for _, ref := range regionRefs {
			if !seenSpace[ref.SpaceGUID] {
				seenSpace[ref.SpaceGUID] = true
				spaceGUIDs = append(spaceGUIDs, ref.SpaceGUID)
			}
		}

		apps, err := client.ListAppsBySpaces(ctx, spaceGUIDs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: region %q: listing apps: %v\n", region, err)
			continue
		}
		appByGUID := make(map[string]cf.App, len(apps))
		for _, a := range apps {
			appByGUID[a.GUID] = a
		}

		for _, ref := range regionRefs {
			app, ok := appByGUID[ref.AppGUID]
			if !ok {
				fmt.Fprintf(os.Stderr, "warning: app %q (%s) in %s/%s not found — skipping\n",
					ref.AppName, ref.AppGUID, ref.OrgName, ref.SpaceName)
				continue
			}
			allMatched = append(allMatched, sawMatchedApp{
				Client:    client,
				OrgName:   ref.OrgName,
				SpaceName: ref.SpaceName,
				AppGUID:   app.GUID,
				AppName:   app.Name,
				State:     app.State,
			})
		}
	}

	sortMatchedApps(allMatched)

	return allMatched, nil
}

// printSawPreview renders the matched apps as a TOON document nested
// org -> space -> apps.
func printSawPreview(w io.Writer, matched []sawMatchedApp) error {
	var doc sawPreviewDoc
	var curOrg *sawPreviewOrg
	var curSpace *sawPreviewSpace

	flushSpace := func() {
		if curSpace != nil && curOrg != nil {
			curOrg.Spaces = append(curOrg.Spaces, *curSpace)
		}
		curSpace = nil
	}
	flushOrg := func() {
		flushSpace()
		if curOrg != nil {
			doc.Orgs = append(doc.Orgs, *curOrg)
		}
		curOrg = nil
	}

	for _, a := range matched {
		if curOrg == nil || curOrg.OrgName != a.OrgName {
			flushOrg()
			curOrg = &sawPreviewOrg{OrgName: a.OrgName}
		}
		if curSpace == nil || curSpace.SpaceName != a.SpaceName {
			flushSpace()
			curSpace = &sawPreviewSpace{SpaceName: a.SpaceName}
		}
		curSpace.Apps = append(curSpace.Apps, sawPreviewApp{
			AppGUID: a.AppGUID,
			AppName: a.AppName,
			State:   a.State,
		})
	}
	flushOrg()

	out, err := toonenc.Marshal(doc, toonenc.WithIndent(2))
	if err != nil {
		return fmt.Errorf("encoding TOON: %w", err)
	}
	if _, err := w.Write(out); err != nil {
		return err
	}
	_, err = w.Write([]byte("\n"))
	return err
}

// sawAppPatternArgs validates the positional <app_name_patterns> argument
// shared by start-apps/stop-apps/restart-apps/restage-apps: required by
// default, but optional (defaulting to "*", matching every row) when --apps
// is given, since the CSV already names the exact apps to target.
func sawAppPatternArgs(cmd *cobra.Command, args []string) error {
	appsFile, _ := cmd.Flags().GetString("apps")
	if appsFile != "" {
		return cobra.MaximumNArgs(1)(cmd, args)
	}
	return cobra.ExactArgs(1)(cmd, args)
}

// parseAppStrategy reads and validates the --strategy flag shared by
// restart-apps/restage-apps. "" and "null" (not given, or given explicitly)
// both mean no deployment strategy — the classic, downtime-causing action;
// "rolling" and "canary" mean a zero-downtime deployment with that
// strategy. The returned string is "", "rolling", or "canary".
func parseAppStrategy(cmd *cobra.Command) (string, error) {
	raw, _ := cmd.Flags().GetString("strategy")
	strategy := strings.ToLower(strings.TrimSpace(raw))
	switch strategy {
	case "", "null":
		return "", nil
	case "rolling", "canary":
		return strategy, nil
	default:
		return "", fmt.Errorf("invalid --strategy value %q: must be canary, rolling, or null", raw)
	}
}

// runStartStopApps implements the shared body of start-apps/stop-apps:
// resolve matched apps, show a TOON preview and confirm (unless -y), then
// invoke action on each matched app with bounded concurrency. verb is the
// base form used in "Apps to %s" / "N app(s) to %s" (e.g. "start"); ingForm
// and pastForm are its "-ing"/past-tense spellings (e.g. "starting"/
// "started") used in per-app progress and the final summary.
//
// args[0] (app_name_patterns) is required unless --apps is given, in which
// case omitting it defaults to "*" (every row of the --apps CSV) — see
// sawAppPatternArgs.
func runStartStopApps(cmd *cobra.Command, args []string, verb, ingForm, pastForm string, action func(*cf.Client, context.Context, string) (*cf.App, error)) error {
	patternsCSV := "*"
	if len(args) > 0 {
		patternsCSV = strings.TrimSpace(args[0])
		if patternsCSV == "" {
			return fmt.Errorf("app_name_patterns cannot be empty")
		}
	}
	skipConfirm, _ := cmd.Flags().GetBool("yes")
	appsFile, _ := cmd.Flags().GetString("apps")

	ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer cancel()

	matched, err := resolveMatchedApps(ctx, cmd, patternsCSV)
	if err != nil {
		return err
	}
	if len(matched) == 0 {
		if appsFile != "" {
			return fmt.Errorf("no apps matched — check the --apps CSV and the name patterns")
		}
		return fmt.Errorf("no apps matched — check --org/--orgs, --spaces, and the name patterns")
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Apps to %s:\n", verb)
	if err := printSawPreview(out, matched); err != nil {
		return err
	}
	fmt.Fprintf(out, "\n%d app(s) to %s.\n", len(matched), verb)

	if !skipConfirm {
		fmt.Fprint(out, "\nProceed? [y/N] ")
		text, ok := readLine(ctx)
		if !ok || strings.ToLower(text) != "y" {
			fmt.Fprintln(out, "Aborted.")
			return nil
		}
	}

	var (
		execWg           sync.WaitGroup
		mu               sync.Mutex
		succeeded, fails int
	)
	sem := make(chan struct{}, sawConcurrency)
	for _, a := range matched {
		execWg.Add(1)
		sem <- struct{}{}
		go func(a sawMatchedApp) {
			defer execWg.Done()
			defer func() { <-sem }()

			if _, err := action(a.Client, ctx, a.AppGUID); err != nil {
				mu.Lock()
				fails++
				mu.Unlock()
				fmt.Fprintf(os.Stderr, "  ! ERROR %s %s/%s/%s: %v\n", ingForm, a.OrgName, a.SpaceName, a.AppName, err)
				return
			}

			mu.Lock()
			succeeded++
			mu.Unlock()
			fmt.Fprintf(out, "  - %s: %s/%s/%s\n", pastForm, a.OrgName, a.SpaceName, a.AppName)
		}(a)
	}
	execWg.Wait()

	fmt.Fprintf(out, "\nDone: %d %s, %d failed.\n", succeeded, pastForm, fails)
	return nil
}
