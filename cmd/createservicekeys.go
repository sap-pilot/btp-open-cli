package cmd

import (
	"context"
	"encoding/json"
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

// cskConcurrency bounds how many CF API calls (existing-binding lookups, and
// later create/update calls) run at once across the whole matched set.
const cskConcurrency = 8

// ── matched instances (phase 1) ─────────────────────────────────────────────

// cskMatchedInstance is one managed service instance that passed org/space/
// name-pattern filtering, annotated with the org/space names and the
// already-authenticated client for its region.
type cskMatchedInstance struct {
	Client       *cf.Client
	OrgName      string
	SpaceName    string
	InstanceGUID string
	InstanceName string
	Label        string
	Plan         string
	Tags         []string
}

type cskRegionResult struct {
	Region    string
	Instances []cskMatchedInstance
	Err       error
}

// ── planned items (phase 2) ─────────────────────────────────────────────────

// cskPlannedItem is one service key to create (or update, if a key with the
// same name already exists on that instance).
type cskPlannedItem struct {
	Client              *cf.Client
	OrgName             string
	SpaceName           string
	InstanceGUID        string
	InstanceName        string
	Label               string
	Plan                string
	Tags                []string
	ExistingBindingGUID string // non-empty if a key with this name already exists
}

// ── TOON preview document ────────────────────────────────────────────────────

type cskPreviewInstance struct {
	InstanceGUID   string `toon:"instance_guid"`
	InstanceName   string `toon:"instance_name"`
	Label          string `toon:"label"`
	Plan           string `toon:"plan"`
	ServiceKeyName string `toon:"service_key_name"`
	Status         string `toon:"status"` // "create" or "update"
}

type cskPreviewSpace struct {
	SpaceName        string               `toon:"space_name"`
	ServiceInstances []cskPreviewInstance `toon:"service_instances"`
}

type cskPreviewOrg struct {
	OrgName string            `toon:"org_name"`
	Spaces  []cskPreviewSpace `toon:"spaces"`
}

type cskPreviewDoc struct {
	Orgs []cskPreviewOrg `toon:"orgs"`
}

// ── output record ────────────────────────────────────────────────────────────

// cskOutputRecord is one created/updated service key, written to --output
// (or stdout) as a flat JSON array once the run completes.
type cskOutputRecord struct {
	Org          string                 `json:"org"`
	Space        string                 `json:"space"`
	InstanceName string                 `json:"instance_name"`
	InstanceGUID string                 `json:"instance_guid"`
	Label        string                 `json:"label"`
	Plan         string                 `json:"plan"`
	Tags         []string               `json:"tags,omitempty"`
	BindingGUID  string                 `json:"binding_guid"`
	BindingName  string                 `json:"binding_name"`
	Credentials  map[string]interface{} `json:"credentials"`
}

var createServiceKeysCmd = &cobra.Command{
	Use:   "create-service-keys <service_key_name>",
	Short: "Create (or update) service keys for matching service instances across orgs",
	Long: `Create a service key of the given name for every managed service instance
that matches --service-instances, across target orgs and spaces.

If neither --org nor --orgs is given, the default org scope selected via
'bo orgs' is used. --spaces narrows to specific space names (comma-separated,
exact, case-insensitive) within each target org; if omitted, every accessible
space in scope is searched. --service-instances/-s narrows to specific
service instance names: comma-separated patterns, e.g. '*-hdi,*-db' — each is
a glob (via filepath.Match) if it contains * ? [, otherwise a case-insensitive
substring. If omitted, every managed service instance in scope matches.

Without -y, a preview in TOON format (org -> space -> service_instances) is
shown first, listing each matched instance and whether the given key name
would be created or updated there. If any matching instance already has a
key with that name, you're asked whether to [u]pdate existing (delete and
recreate), [c]reate new keys only (skip the ones that already exist), or
[a]bort — the same choice can be made upfront with --create-new-keys-only
(equivalent to answering 'c'). If -y is given without --create-new-keys-only
and existing keys are found, 'u' (update) is chosen automatically with no
prompt. If there are no naming conflicts at all, a plain "Proceed? [y/N]"
confirmation is shown instead, skipped by -y.

Each created/updated key's credentials are fetched and written as a flat
JSON array to --output/-o (or stdout if omitted):
[{org, space, instance_name, instance_guid, label, plan, tags, binding_guid,
binding_name, credentials}].

⚠️ This outputs live secrets (the service key's credentials) in plain JSON —
prefer --output to a file over printing to a shared terminal.

If --regions is omitted the regions from the last login are used.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		serviceKeyName := strings.TrimSpace(args[0])
		if serviceKeyName == "" {
			return fmt.Errorf("service_key_name cannot be empty")
		}

		regionsFlag, _ := cmd.Flags().GetString("regions")
		orgsFile, _ := cmd.Flags().GetString("orgs")
		excludeOrgsFile, _ := cmd.Flags().GetString("excludeOrgs")
		orgGUID, _ := cmd.Flags().GetString("org")
		spacesFlag, _ := cmd.Flags().GetString("spaces")
		instancesFlag, _ := cmd.Flags().GetString("service-instances")
		outputFile, _ := cmd.Flags().GetString("output")
		skipConfirm, _ := cmd.Flags().GetBool("yes")
		createNewOnlyFlag, _ := cmd.Flags().GetBool("create-new-keys-only")

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
		// managed service instances), in parallel across regions.
		results := make([]cskRegionResult, len(apiURLs))
		var wg sync.WaitGroup
		for i, apiURL := range apiURLs {
			wg.Add(1)
			go func(idx int, url string) {
				defer wg.Done()
				regionName := store.APIURLToRegion(url)

				tok, ok := creds.Tokens[url]
				if !ok {
					results[idx] = cskRegionResult{Region: regionName,
						Err: fmt.Errorf("no token — run: bo login --regions %s", regionName)}
					return
				}
				client := cf.NewClient(url, tok.AccessToken)
				client.SetTokenRefresher(makeTokenRefresher(url, tok.AccessToken))

				orgs, err := client.ListOrganizations(ctx)
				if err != nil {
					results[idx] = cskRegionResult{Region: regionName,
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
					results[idx] = cskRegionResult{Region: regionName}
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
					results[idx] = cskRegionResult{Region: regionName,
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
					results[idx] = cskRegionResult{Region: regionName}
					return
				}

				instances, err := client.ListServiceInstancesFullBySpaces(ctx, spaceGUIDs)
				if err != nil {
					results[idx] = cskRegionResult{Region: regionName,
						Err: fmt.Errorf("listing service instances: %w", err)}
					return
				}

				var matched []cf.ServiceInstanceFull
				planGUIDSet := make(map[string]struct{})
				for _, inst := range instances {
					if !scMatchesAppPattern(instancesFlag, inst.Name) {
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
					results[idx] = cskRegionResult{Region: regionName,
						Err: fmt.Errorf("fetching service plan details: %w", err)}
					return
				}

				out := make([]cskMatchedInstance, 0, len(matched))
				for _, inst := range matched {
					info := spaceByGUID[inst.Relationships.Space.Data.GUID]
					pd := planDetails[inst.Relationships.ServicePlan.Data.GUID]
					out = append(out, cskMatchedInstance{
						Client:       client,
						OrgName:      info.OrgName,
						SpaceName:    info.Name,
						InstanceGUID: inst.GUID,
						InstanceName: inst.Name,
						Label:        pd.ServiceName,
						Plan:         pd.Name,
						Tags:         inst.Tags,
					})
				}

				results[idx] = cskRegionResult{Region: regionName, Instances: out}
			}(i, apiURL)
		}
		wg.Wait()

		var allMatched []cskMatchedInstance
		for _, r := range results {
			if r.Err != nil {
				fmt.Fprintf(os.Stderr, "warning: region %q: %v\n", r.Region, r.Err)
				continue
			}
			allMatched = append(allMatched, r.Instances...)
		}
		if len(allMatched) == 0 {
			return fmt.Errorf("no service instances matched — check --org/--orgs, --spaces, and --service-instances")
		}

		// Phase 1.5: check for an existing key of this name on each matched
		// instance, bounded concurrency across the whole matched set.
		planned := make([]cskPlannedItem, len(allMatched))
		{
			var checkWg sync.WaitGroup
			sem := make(chan struct{}, cskConcurrency)
			for i, m := range allMatched {
				checkWg.Add(1)
				sem <- struct{}{}
				go func(i int, m cskMatchedInstance) {
					defer checkWg.Done()
					defer func() { <-sem }()

					item := cskPlannedItem{
						Client:       m.Client,
						OrgName:      m.OrgName,
						SpaceName:    m.SpaceName,
						InstanceGUID: m.InstanceGUID,
						InstanceName: m.InstanceName,
						Label:        m.Label,
						Plan:         m.Plan,
						Tags:         m.Tags,
					}
					existing, err := m.Client.FindServiceCredentialBinding(ctx, serviceKeyName, m.InstanceGUID)
					if err != nil {
						fmt.Fprintf(os.Stderr, "warning: %s/%s/%s: checking for existing key: %v\n",
							m.OrgName, m.SpaceName, m.InstanceName, err)
					} else if existing != nil {
						item.ExistingBindingGUID = existing.GUID
					}
					planned[i] = item
				}(i, m)
			}
			checkWg.Wait()
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
		fmt.Fprintln(out, "Service keys to create/update:")
		if err := printCskPreview(out, planned, serviceKeyName); err != nil {
			return err
		}

		conflictCount := 0
		for _, p := range planned {
			if p.ExistingBindingGUID != "" {
				conflictCount++
			}
		}
		fmt.Fprintf(out, "\n%d to create, %d to update (already exist).\n", len(planned)-conflictCount, conflictCount)

		var createNewOnly bool
		switch {
		case createNewOnlyFlag:
			createNewOnly = true
		case conflictCount == 0 && !skipConfirm:
			fmt.Fprint(out, "\nProceed? [y/N] ")
			text, ok := readLine(ctx)
			if !ok || strings.ToLower(text) != "y" {
				fmt.Fprintln(out, "Aborted.")
				return nil
			}
		case conflictCount > 0 && skipConfirm:
			createNewOnly = false // -y without --create-new-keys-only defaults to update
		case conflictCount > 0 && !skipConfirm:
			var aborted bool
			createNewOnly, aborted = promptUpdateCreateAbort(ctx, out, serviceKeyName, conflictCount)
			if aborted {
				fmt.Fprintln(out, "Aborted.")
				return nil
			}
		}

		toExecute := planned
		if createNewOnly {
			var newOnly []cskPlannedItem
			for _, p := range planned {
				if p.ExistingBindingGUID == "" {
					newOnly = append(newOnly, p)
				}
			}
			toExecute = newOnly
		}
		if len(toExecute) == 0 {
			fmt.Fprintln(out, "Nothing to do.")
			return nil
		}

		// Phase 3: create/update, bounded concurrency across the whole batch.
		var (
			execWg                            sync.WaitGroup
			mu                                sync.Mutex
			records                           []cskOutputRecord
			createdCount, updatedCount, fails int
		)
		sem := make(chan struct{}, cskConcurrency)
		for _, p := range toExecute {
			execWg.Add(1)
			sem <- struct{}{}
			go func(p cskPlannedItem) {
				defer execWg.Done()
				defer func() { <-sem }()

				isUpdate := p.ExistingBindingGUID != ""
				if isUpdate {
					if err := p.Client.DeleteServiceCredentialBinding(ctx, p.ExistingBindingGUID); err != nil {
						mu.Lock()
						fails++
						mu.Unlock()
						fmt.Fprintf(os.Stderr, "  ! ERROR deleting existing key for %s/%s/%s: %v\n",
							p.OrgName, p.SpaceName, p.InstanceName, err)
						return
					}
				}

				bindingGUID, err := p.Client.CreateServiceCredentialBinding(ctx, serviceKeyName, p.InstanceGUID)
				if err != nil {
					mu.Lock()
					fails++
					mu.Unlock()
					fmt.Fprintf(os.Stderr, "  ! ERROR creating key for %s/%s/%s: %v\n",
						p.OrgName, p.SpaceName, p.InstanceName, err)
					return
				}

				credentials, err := p.Client.GetServiceCredentialDetails(ctx, bindingGUID)
				if err != nil {
					mu.Lock()
					fails++
					mu.Unlock()
					fmt.Fprintf(os.Stderr, "  ! ERROR fetching credentials for %s/%s/%s: %v\n",
						p.OrgName, p.SpaceName, p.InstanceName, err)
					return
				}

				record := cskOutputRecord{
					Org:          p.OrgName,
					Space:        p.SpaceName,
					InstanceName: p.InstanceName,
					InstanceGUID: p.InstanceGUID,
					Label:        p.Label,
					Plan:         p.Plan,
					Tags:         p.Tags,
					BindingGUID:  bindingGUID,
					BindingName:  serviceKeyName,
					Credentials:  credentials,
				}

				mu.Lock()
				records = append(records, record)
				if isUpdate {
					updatedCount++
				} else {
					createdCount++
				}
				mu.Unlock()

				verb := "created"
				if isUpdate {
					verb = "updated"
				}
				fmt.Fprintf(out, "  + %s: %s/%s/%s\n", verb, p.OrgName, p.SpaceName, p.InstanceName)
			}(p)
		}
		execWg.Wait()

		fmt.Fprintf(out, "\nDone: %d created, %d updated, %d failed.\n", createdCount, updatedCount, fails)

		jsonOut, closeOut, err := resolveOutputWriter(outputFile)
		if err != nil {
			return err
		}
		defer closeOut()

		if records == nil {
			records = []cskOutputRecord{}
		}
		enc := json.NewEncoder(jsonOut)
		enc.SetIndent("", "  ")
		if err := enc.Encode(records); err != nil {
			return fmt.Errorf("encoding JSON: %w", err)
		}
		return nil
	},
}

// printCskPreview renders the planned items as a TOON document nested
// org -> space -> service_instances.
func printCskPreview(w io.Writer, planned []cskPlannedItem, serviceKeyName string) error {
	var doc cskPreviewDoc
	var curOrg *cskPreviewOrg
	var curSpace *cskPreviewSpace

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
			curOrg = &cskPreviewOrg{OrgName: p.OrgName}
		}
		if curSpace == nil || curSpace.SpaceName != p.SpaceName {
			flushSpace()
			curSpace = &cskPreviewSpace{SpaceName: p.SpaceName}
		}
		status := "create"
		if p.ExistingBindingGUID != "" {
			status = "update"
		}
		curSpace.ServiceInstances = append(curSpace.ServiceInstances, cskPreviewInstance{
			InstanceGUID:   p.InstanceGUID,
			InstanceName:   p.InstanceName,
			Label:          p.Label,
			Plan:           p.Plan,
			ServiceKeyName: serviceKeyName,
			Status:         status,
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

// promptUpdateCreateAbort asks the user to resolve a naming conflict:
// [u]pdate existing keys (delete + recreate), [c]reate new keys only (skip
// the conflicting ones), or [a]bort the whole run. It loops on unrecognized
// input; a closed/cancelled stdin is treated as abort.
func promptUpdateCreateAbort(ctx context.Context, out io.Writer, keyName string, conflictCount int) (createNewOnly, aborted bool) {
	for {
		fmt.Fprintf(out, "\n%d service key(s) named %q already exist. [u]pdate existing, [c]reate new only, or [a]bort? ",
			conflictCount, keyName)
		text, ok := readLine(ctx)
		if !ok {
			return false, true
		}
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "u", "update":
			return false, false
		case "c", "create":
			return true, false
		case "a", "abort", "":
			return false, true
		default:
			fmt.Fprintln(out, "Please answer u, c, or a.")
		}
	}
}

func init() {
	createServiceKeysCmd.GroupID = "cf-org"
	rootCmd.AddCommand(createServiceKeysCmd)
	createServiceKeysCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	createServiceKeysCmd.Flags().String("org", "", "Org GUID to target; only this org will be targeted")
	createServiceKeysCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	createServiceKeysCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,org_id,org_name)")
	createServiceKeysCmd.Flags().String("spaces", "", "Only include these comma-separated, exact (case-insensitive) space names; default is every accessible space in scope")
	createServiceKeysCmd.Flags().StringP("service-instances", "s", "", "Only include service instances whose name matches any of these comma-separated patterns (glob, e.g. '*-hdi,*-db', or substring); default is every managed service instance in scope")
	createServiceKeysCmd.Flags().StringP("output", "o", "", "Write the created/updated key details (including credentials) as a JSON array to this file instead of stdout")
	createServiceKeysCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompts (defaults to 'update' for existing keys unless --create-new-keys-only is also given)")
	createServiceKeysCmd.Flags().BoolP("create-new-keys-only", "c", false, "When a key with this name already exists, skip it instead of updating it — answers the update/create/abort prompt as 'create new only' without asking")
}
