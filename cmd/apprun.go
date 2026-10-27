package cmd

import (
	"context"
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

	creds, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("not logged in — run: bo login --regions <region>")
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

	sort.Slice(allMatched, func(i, j int) bool {
		if allMatched[i].OrgName != allMatched[j].OrgName {
			return allMatched[i].OrgName < allMatched[j].OrgName
		}
		if allMatched[i].SpaceName != allMatched[j].SpaceName {
			return allMatched[i].SpaceName < allMatched[j].SpaceName
		}
		return allMatched[i].AppName < allMatched[j].AppName
	})

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
func runStartStopApps(cmd *cobra.Command, args []string, verb, ingForm, pastForm string, action func(*cf.Client, context.Context, string) (*cf.App, error)) error {
	patternsCSV := strings.TrimSpace(args[0])
	if patternsCSV == "" {
		return fmt.Errorf("app_name_patterns cannot be empty")
	}
	skipConfirm, _ := cmd.Flags().GetBool("yes")

	ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer cancel()

	matched, err := resolveMatchedApps(ctx, cmd, patternsCSV)
	if err != nil {
		return err
	}
	if len(matched) == 0 {
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
