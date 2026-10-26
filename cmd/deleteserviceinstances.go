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

// dsiConcurrency bounds how many delete API calls run at once across the
// whole matched set.
const dsiConcurrency = 8

// ── matched instances (phase 1) ─────────────────────────────────────────────

// dsiMatchedInstance is one service instance (managed or user-provided) that
// passed org/space/name-pattern filtering, annotated with the org/space
// names and the already-authenticated client for its region.
type dsiMatchedInstance struct {
	Client       *cf.Client
	OrgName      string
	SpaceName    string
	InstanceGUID string
	InstanceName string
	Type         string
	Label        string
	Plan         string
}

type dsiRegionResult struct {
	Region    string
	Instances []dsiMatchedInstance
	Err       error
}

// ── planned items (phase 2) ──────────────────────────────────────────────────

// dsiPlannedItem is one service instance to delete, along with every
// existing service credential binding (key or app binding) on it that must
// be deleted first.
type dsiPlannedItem struct {
	Client       *cf.Client
	OrgName      string
	SpaceName    string
	InstanceGUID string
	InstanceName string
	Type         string
	Label        string
	Plan         string
	BindingGUIDs []string
}

// ── TOON preview document ────────────────────────────────────────────────────

type dsiPreviewInstance struct {
	InstanceGUID     string `toon:"instance_guid"`
	InstanceName     string `toon:"instance_name"`
	Type             string `toon:"type"`
	Label            string `toon:"label"`
	Plan             string `toon:"plan"`
	BindingsToDelete int    `toon:"bindings_to_delete"`
}

type dsiPreviewSpace struct {
	SpaceName        string               `toon:"space_name"`
	ServiceInstances []dsiPreviewInstance `toon:"service_instances"`
}

type dsiPreviewOrg struct {
	OrgName string            `toon:"org_name"`
	Spaces  []dsiPreviewSpace `toon:"spaces"`
}

type dsiPreviewDoc struct {
	Orgs []dsiPreviewOrg `toon:"orgs"`
}

var deleteServiceInstancesCmd = &cobra.Command{
	Use:   "delete-service-instances <service_instance_name_patterns>",
	Short: "Delete service instances (and their bindings/keys) matching name patterns, across orgs",
	Long: `Delete every service instance (managed or user-provided) whose name matches
<service_instance_name_patterns> — a comma-separated list of patterns, e.g.
'*-ups,tmp-*': each is a glob (via filepath.Match) if it contains * ? [,
otherwise a case-insensitive substring.

If neither --org nor --orgs is given, the default org scope selected via
'bo orgs' is used. --spaces/--space narrows to specific space names
(comma-separated, exact, case-insensitive) within each target org; if
omitted, every accessible space in scope is searched.

⚠️ This is destructive and cannot be undone: every service credential
binding and service key on a matched instance is deleted first (CF refuses
to delete a service instance that still has any), and then the instance
itself is deleted.

Without -y, every matched instance is listed in a TOON preview (org -> space
-> service_instances), including how many existing bindings/keys will be
deleted along with it, and a "Proceed? [y/N]" confirmation is required —
declining aborts the whole command without deleting anything. -y skips this
confirmation.

If --regions is omitted the regions from the last login are used.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		patternsCSV := strings.TrimSpace(args[0])
		if patternsCSV == "" {
			return fmt.Errorf("service_instance_name_patterns cannot be empty")
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

		// Phase 1: resolve matching service instances (orgs -> spaces ->
		// service instances, managed or user-provided), in parallel across
		// regions.
		results := make([]dsiRegionResult, len(apiURLs))
		var wg sync.WaitGroup
		for i, apiURL := range apiURLs {
			wg.Add(1)
			go func(idx int, url string) {
				defer wg.Done()
				regionName := store.APIURLToRegion(url)

				tok, ok := creds.Tokens[url]
				if !ok {
					results[idx] = dsiRegionResult{Region: regionName,
						Err: fmt.Errorf("no token — run: bo login --regions %s", regionName)}
					return
				}
				client := cf.NewClient(url, tok.AccessToken)
				client.SetTokenRefresher(makeTokenRefresher(url, tok.AccessToken))

				orgs, err := client.ListOrganizations(ctx)
				if err != nil {
					results[idx] = dsiRegionResult{Region: regionName,
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
					results[idx] = dsiRegionResult{Region: regionName}
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
					results[idx] = dsiRegionResult{Region: regionName,
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
					results[idx] = dsiRegionResult{Region: regionName}
					return
				}

				instances, err := client.ListServiceInstancesFullBySpacesAnyType(ctx, spaceGUIDs)
				if err != nil {
					results[idx] = dsiRegionResult{Region: regionName,
						Err: fmt.Errorf("listing service instances: %w", err)}
					return
				}

				var matched []cf.ServiceInstanceFull
				planGUIDSet := make(map[string]struct{})
				for _, inst := range instances {
					if !scMatchesAppPattern(patternsCSV, inst.Name) {
						continue
					}
					matched = append(matched, inst)
					if g := inst.Relationships.ServicePlan.Data.GUID; g != "" {
						planGUIDSet[g] = struct{}{}
					}
				}

				planGUIDs := make([]string, 0, len(planGUIDSet))
				for g := range planGUIDSet {
					planGUIDs = append(planGUIDs, g)
				}
				planDetails, err := client.ListServicePlanDetails(ctx, planGUIDs)
				if err != nil {
					results[idx] = dsiRegionResult{Region: regionName,
						Err: fmt.Errorf("fetching service plan details: %w", err)}
					return
				}

				out := make([]dsiMatchedInstance, 0, len(matched))
				for _, inst := range matched {
					info := spaceByGUID[inst.Relationships.Space.Data.GUID]
					pd := planDetails[inst.Relationships.ServicePlan.Data.GUID]
					out = append(out, dsiMatchedInstance{
						Client:       client,
						OrgName:      info.OrgName,
						SpaceName:    info.Name,
						InstanceGUID: inst.GUID,
						InstanceName: inst.Name,
						Type:         inst.Type,
						Label:        pd.ServiceName,
						Plan:         pd.Name,
					})
				}

				results[idx] = dsiRegionResult{Region: regionName, Instances: out}
			}(i, apiURL)
		}
		wg.Wait()

		var allMatched []dsiMatchedInstance
		for _, r := range results {
			if r.Err != nil {
				fmt.Fprintf(os.Stderr, "warning: region %q: %v\n", r.Region, r.Err)
				continue
			}
			allMatched = append(allMatched, r.Instances...)
		}
		if len(allMatched) == 0 {
			return fmt.Errorf("no service instances matched — check --org/--orgs, --spaces, and the name patterns")
		}

		// Phase 1.5: look up every existing service credential binding/key
		// on each matched instance, batched per region client.
		byClient := make(map[*cf.Client][]string)
		for _, m := range allMatched {
			byClient[m.Client] = append(byClient[m.Client], m.InstanceGUID)
		}
		bindingsByInstance := make(map[string][]string)
		{
			var lookupWg sync.WaitGroup
			var lookupMu sync.Mutex
			for client, guids := range byClient {
				lookupWg.Add(1)
				go func(client *cf.Client, guids []string) {
					defer lookupWg.Done()
					bindings, err := client.ListServiceCredentialBindingsByInstances(ctx, guids)
					if err != nil {
						fmt.Fprintf(os.Stderr, "warning: listing existing bindings/keys: %v\n", err)
						return
					}
					lookupMu.Lock()
					for _, b := range bindings {
						instGUID := b.Relationships.ServiceInstance.Data.GUID
						bindingsByInstance[instGUID] = append(bindingsByInstance[instGUID], b.GUID)
					}
					lookupMu.Unlock()
				}(client, guids)
			}
			lookupWg.Wait()
		}

		planned := make([]dsiPlannedItem, len(allMatched))
		for i, m := range allMatched {
			planned[i] = dsiPlannedItem{
				Client:       m.Client,
				OrgName:      m.OrgName,
				SpaceName:    m.SpaceName,
				InstanceGUID: m.InstanceGUID,
				InstanceName: m.InstanceName,
				Type:         m.Type,
				Label:        m.Label,
				Plan:         m.Plan,
				BindingGUIDs: bindingsByInstance[m.InstanceGUID],
			}
		}

		sort.Slice(planned, func(i, j int) bool {
			if planned[i].OrgName != planned[j].OrgName {
				return planned[i].OrgName < planned[j].OrgName
			}
			if planned[i].SpaceName != planned[j].SpaceName {
				return planned[i].SpaceName < planned[j].SpaceName
			}
			return planned[i].InstanceName < planned[j].InstanceName
		})

		out := cmd.OutOrStdout()
		fmt.Fprintln(out, "Service instances to delete:")
		if err := printDsiPreview(out, planned); err != nil {
			return err
		}

		totalBindings := 0
		for _, p := range planned {
			totalBindings += len(p.BindingGUIDs)
		}
		fmt.Fprintf(out, "\n%d service instance(s) to delete, %d existing binding(s)/key(s) to delete along with them.\n",
			len(planned), totalBindings)

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
		sem := make(chan struct{}, dsiConcurrency)
		for _, p := range planned {
			execWg.Add(1)
			sem <- struct{}{}
			go func(p dsiPlannedItem) {
				defer execWg.Done()
				defer func() { <-sem }()

				for _, bindingGUID := range p.BindingGUIDs {
					if err := p.Client.DeleteServiceCredentialBinding(ctx, bindingGUID); err != nil {
						mu.Lock()
						fails++
						mu.Unlock()
						fmt.Fprintf(os.Stderr, "  ! ERROR deleting binding %s for %s/%s/%s: %v\n",
							bindingGUID, p.OrgName, p.SpaceName, p.InstanceName, err)
						return
					}
				}

				if err := p.Client.DeleteServiceInstance(ctx, p.InstanceGUID); err != nil {
					mu.Lock()
					fails++
					mu.Unlock()
					fmt.Fprintf(os.Stderr, "  ! ERROR deleting %s/%s/%s: %v\n",
						p.OrgName, p.SpaceName, p.InstanceName, err)
					return
				}

				mu.Lock()
				deletedCount++
				mu.Unlock()
				fmt.Fprintf(out, "  - deleted: %s/%s/%s (%d binding(s)/key(s) removed)\n",
					p.OrgName, p.SpaceName, p.InstanceName, len(p.BindingGUIDs))
			}(p)
		}
		execWg.Wait()

		fmt.Fprintf(out, "\nDone: %d deleted, %d failed.\n", deletedCount, fails)
		return nil
	},
}

// printDsiPreview renders the planned items as a TOON document nested
// org -> space -> service_instances.
func printDsiPreview(w io.Writer, planned []dsiPlannedItem) error {
	var doc dsiPreviewDoc
	var curOrg *dsiPreviewOrg
	var curSpace *dsiPreviewSpace

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

	for _, p := range planned {
		if curOrg == nil || curOrg.OrgName != p.OrgName {
			flushOrg()
			curOrg = &dsiPreviewOrg{OrgName: p.OrgName}
		}
		if curSpace == nil || curSpace.SpaceName != p.SpaceName {
			flushSpace()
			curSpace = &dsiPreviewSpace{SpaceName: p.SpaceName}
		}
		curSpace.ServiceInstances = append(curSpace.ServiceInstances, dsiPreviewInstance{
			InstanceGUID:     p.InstanceGUID,
			InstanceName:     p.InstanceName,
			Type:             p.Type,
			Label:            p.Label,
			Plan:             p.Plan,
			BindingsToDelete: len(p.BindingGUIDs),
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
	deleteServiceInstancesCmd.GroupID = "cf-org"
	rootCmd.AddCommand(deleteServiceInstancesCmd)
	deleteServiceInstancesCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	deleteServiceInstancesCmd.Flags().String("org", "", "Org GUID to target; only this org will be targeted")
	deleteServiceInstancesCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	deleteServiceInstancesCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,org_id,org_name)")
	deleteServiceInstancesCmd.Flags().String("spaces", "", "Only include these comma-separated, exact (case-insensitive) space names; default is every accessible space in scope")
	deleteServiceInstancesCmd.Flags().String("space", "", "Equivalent to --spaces with a single name (combined with --spaces if both are given)")
	deleteServiceInstancesCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
}
