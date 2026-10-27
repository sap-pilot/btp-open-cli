package cmd

import (
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

// dskConcurrency bounds how many delete API calls run at once across the
// whole matched set.
const dskConcurrency = 8

// dskMatchedKey is one service key (service credential binding of type
// "key") that passed org/space/name-pattern filtering, annotated with the
// org/space/instance names and the already-authenticated client for its
// region.
type dskMatchedKey struct {
	Client       *cf.Client
	OrgName      string
	SpaceName    string
	InstanceGUID string
	InstanceName string
	KeyGUID      string
	KeyName      string
}

type dskRegionResult struct {
	Region string
	Keys   []dskMatchedKey
	Err    error
}

// ── TOON preview document ────────────────────────────────────────────────────

type dskPreviewKey struct {
	KeyGUID string `toon:"key_guid"`
	KeyName string `toon:"key_name"`
}

type dskPreviewInstance struct {
	InstanceGUID string          `toon:"instance_guid"`
	InstanceName string          `toon:"instance_name"`
	ServiceKeys  []dskPreviewKey `toon:"service_keys"`
}

type dskPreviewSpace struct {
	SpaceName        string               `toon:"space_name"`
	ServiceInstances []dskPreviewInstance `toon:"service_instances"`
}

type dskPreviewOrg struct {
	OrgName string            `toon:"org_name"`
	Spaces  []dskPreviewSpace `toon:"spaces"`
}

type dskPreviewDoc struct {
	Orgs []dskPreviewOrg `toon:"orgs"`
}

var deleteServiceKeysCmd = &cobra.Command{
	Use:   "delete-service-keys <service_key_name_patterns>",
	Short: "Delete service keys matching name patterns, across orgs and spaces",
	Long: `Delete every service key (service credential binding of type "key") whose
name matches <service_key_name_patterns> — a comma-separated list of
patterns, e.g. 'btp*,temp*' or '*' (matches every key name). Each pattern is
a glob (via filepath.Match) if it contains * ? [, otherwise a case-insensitive
substring. A pattern prefixed with '!' reverses that pattern: '!btp*' selects
every key name that does NOT match 'btp*'. A name is selected if any pattern
in the list — negated or not — matches it.

If neither --org nor --orgs is given, the default org scope selected via
'bo orgs' is used. --spaces/--space narrows to specific space names
(comma-separated, exact, case-insensitive) within each target org; if
omitted, every accessible space in scope is searched.

⚠️ This is destructive and cannot be undone.

Without -y, every matched key is listed in a TOON preview (org -> space ->
service_instance -> service_keys) and a "Proceed? [y/N]" confirmation is
required — declining aborts the whole command without deleting anything.
-y skips this confirmation.

If --regions is omitted the regions from the last login are used.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		patternsCSV := strings.TrimSpace(args[0])
		if patternsCSV == "" {
			return fmt.Errorf("service_key_name_patterns cannot be empty")
		}

		regionsFlag, _ := cmd.Flags().GetString("regions")
		orgsFile, _ := cmd.Flags().GetString("orgs")
		excludeOrgsFile, _ := cmd.Flags().GetString("excludeOrgs")
		orgGUID, _ := cmd.Flags().GetString("org")
		spacesFlag, _ := cmd.Flags().GetString("spaces")
		spaceFlag, _ := cmd.Flags().GetString("space")
		skipConfirm, _ := cmd.Flags().GetBool("yes")

		if spaceFlag != "" {
			if spacesFlag != "" {
				spacesFlag += "," + spaceFlag
			} else {
				spacesFlag = spaceFlag
			}
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

		// Phase 1: resolve matching service keys (orgs -> spaces -> service
		// instances -> service credential bindings of type "key"), in
		// parallel across regions.
		results := make([]dskRegionResult, len(apiURLs))
		var wg sync.WaitGroup
		for i, apiURL := range apiURLs {
			wg.Add(1)
			go func(idx int, url string) {
				defer wg.Done()
				regionName := store.APIURLToRegion(url)

				tok, ok := creds.Tokens[url]
				if !ok {
					results[idx] = dskRegionResult{Region: regionName,
						Err: fmt.Errorf("no token — run: bo login --regions %s", regionName)}
					return
				}
				client := cf.NewClient(url, tok.AccessToken)
				client.SetTokenRefresher(makeTokenRefresher(url, tok.AccessToken))

				orgs, err := client.ListOrganizations(ctx)
				if err != nil {
					results[idx] = dskRegionResult{Region: regionName,
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
					results[idx] = dskRegionResult{Region: regionName}
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
					results[idx] = dskRegionResult{Region: regionName,
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
					results[idx] = dskRegionResult{Region: regionName}
					return
				}

				instances, err := client.ListServiceInstancesFullBySpacesAnyType(ctx, spaceGUIDs)
				if err != nil {
					results[idx] = dskRegionResult{Region: regionName,
						Err: fmt.Errorf("listing service instances: %w", err)}
					return
				}
				if len(instances) == 0 {
					results[idx] = dskRegionResult{Region: regionName}
					return
				}

				instanceByGUID := make(map[string]cf.ServiceInstanceFull, len(instances))
				instanceGUIDs := make([]string, len(instances))
				for i, inst := range instances {
					instanceGUIDs[i] = inst.GUID
					instanceByGUID[inst.GUID] = inst
				}

				bindings, err := client.ListServiceCredentialBindingsByInstances(ctx, instanceGUIDs)
				if err != nil {
					results[idx] = dskRegionResult{Region: regionName,
						Err: fmt.Errorf("listing service credential bindings: %w", err)}
					return
				}

				var matched []dskMatchedKey
				for _, b := range bindings {
					if b.Type != "key" {
						continue
					}
					if !matchesNegatablePattern(patternsCSV, b.Name) {
						continue
					}
					inst := instanceByGUID[b.Relationships.ServiceInstance.Data.GUID]
					info := spaceByGUID[inst.Relationships.Space.Data.GUID]
					matched = append(matched, dskMatchedKey{
						Client:       client,
						OrgName:      info.OrgName,
						SpaceName:    info.Name,
						InstanceGUID: inst.GUID,
						InstanceName: inst.Name,
						KeyGUID:      b.GUID,
						KeyName:      b.Name,
					})
				}

				results[idx] = dskRegionResult{Region: regionName, Keys: matched}
			}(i, apiURL)
		}
		wg.Wait()

		var allMatched []dskMatchedKey
		for _, r := range results {
			if r.Err != nil {
				fmt.Fprintf(os.Stderr, "warning: region %q: %v\n", r.Region, r.Err)
				continue
			}
			allMatched = append(allMatched, r.Keys...)
		}
		if len(allMatched) == 0 {
			return fmt.Errorf("no service keys matched — check --org/--orgs, --spaces, and the name patterns")
		}

		sort.Slice(allMatched, func(i, j int) bool {
			if allMatched[i].OrgName != allMatched[j].OrgName {
				return allMatched[i].OrgName < allMatched[j].OrgName
			}
			if allMatched[i].SpaceName != allMatched[j].SpaceName {
				return allMatched[i].SpaceName < allMatched[j].SpaceName
			}
			if allMatched[i].InstanceName != allMatched[j].InstanceName {
				return allMatched[i].InstanceName < allMatched[j].InstanceName
			}
			return allMatched[i].KeyName < allMatched[j].KeyName
		})

		out := cmd.OutOrStdout()
		fmt.Fprintln(out, "Service keys to delete:")
		if err := printDskPreview(out, allMatched); err != nil {
			return err
		}
		fmt.Fprintf(out, "\n%d service key(s) to delete.\n", len(allMatched))

		if !skipConfirm {
			fmt.Fprint(out, "\nThis cannot be undone. Proceed? [y/N] ")
			text, ok := readLine(ctx)
			if !ok || strings.ToLower(text) != "y" {
				fmt.Fprintln(out, "Aborted.")
				return nil
			}
		}

		// Phase 3: delete, bounded concurrency across the whole batch.
		var (
			execWg              sync.WaitGroup
			mu                  sync.Mutex
			deletedCount, fails int
		)
		sem := make(chan struct{}, dskConcurrency)
		for _, k := range allMatched {
			execWg.Add(1)
			sem <- struct{}{}
			go func(k dskMatchedKey) {
				defer execWg.Done()
				defer func() { <-sem }()

				if err := k.Client.DeleteServiceCredentialBinding(ctx, k.KeyGUID); err != nil {
					mu.Lock()
					fails++
					mu.Unlock()
					fmt.Fprintf(os.Stderr, "  ! ERROR deleting key %s for %s/%s/%s: %v\n",
						k.KeyName, k.OrgName, k.SpaceName, k.InstanceName, err)
					return
				}

				mu.Lock()
				deletedCount++
				mu.Unlock()
				fmt.Fprintf(out, "  - deleted: %s/%s/%s/%s\n", k.OrgName, k.SpaceName, k.InstanceName, k.KeyName)
			}(k)
		}
		execWg.Wait()

		fmt.Fprintf(out, "\nDone: %d deleted, %d failed.\n", deletedCount, fails)
		return nil
	},
}

// printDskPreview renders the matched keys as a TOON document nested
// org -> space -> service_instance -> service_keys.
func printDskPreview(w io.Writer, matched []dskMatchedKey) error {
	var doc dskPreviewDoc
	var curOrg *dskPreviewOrg
	var curSpace *dskPreviewSpace
	var curInstance *dskPreviewInstance

	flushInstance := func() {
		if curInstance != nil && curSpace != nil {
			curSpace.ServiceInstances = append(curSpace.ServiceInstances, *curInstance)
		}
		curInstance = nil
	}
	flushSpace := func() {
		flushInstance()
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

	for _, k := range matched {
		if curOrg == nil || curOrg.OrgName != k.OrgName {
			flushOrg()
			curOrg = &dskPreviewOrg{OrgName: k.OrgName}
		}
		if curSpace == nil || curSpace.SpaceName != k.SpaceName {
			flushSpace()
			curSpace = &dskPreviewSpace{SpaceName: k.SpaceName}
		}
		if curInstance == nil || curInstance.InstanceGUID != k.InstanceGUID {
			flushInstance()
			curInstance = &dskPreviewInstance{InstanceGUID: k.InstanceGUID, InstanceName: k.InstanceName}
		}
		curInstance.ServiceKeys = append(curInstance.ServiceKeys, dskPreviewKey{
			KeyGUID: k.KeyGUID,
			KeyName: k.KeyName,
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

func init() {
	deleteServiceKeysCmd.GroupID = "cf-org"
	rootCmd.AddCommand(deleteServiceKeysCmd)
	deleteServiceKeysCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	deleteServiceKeysCmd.Flags().String("org", "", "Org GUID to target; only this org will be targeted")
	deleteServiceKeysCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	deleteServiceKeysCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,org_id,org_name)")
	deleteServiceKeysCmd.Flags().String("spaces", "", "Only include these comma-separated, exact (case-insensitive) space names; default is every accessible space in scope")
	deleteServiceKeysCmd.Flags().String("space", "", "Equivalent to --spaces with a single name (combined with --spaces if both are given)")
	deleteServiceKeysCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
}
