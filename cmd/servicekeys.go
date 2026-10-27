package cmd

import (
	"encoding/csv"
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

// skConcurrency bounds how many CF API calls (credential-detail lookups when
// --full is given) run at once across the whole matched set.
const skConcurrency = 8

// ── output types ──────────────────────────────────────────────────────────────

type skOutKey struct {
	GUID               string                 `json:"key_guid"             toon:"key_guid"`
	Name               string                 `json:"key_name"             toon:"key_name"`
	CreatedAt          string                 `json:"created_at"           toon:"created_at"`
	UpdatedAt          string                 `json:"updated_at"           toon:"updated_at"`
	LastOperationType  string                 `json:"last_operation_type"  toon:"last_operation_type"`
	LastOperationState string                 `json:"last_operation_state" toon:"last_operation_state"`
	Credentials        map[string]interface{} `json:"credentials,omitempty" toon:"credentials,omitempty"`
}

type skOutInstance struct {
	GUID        string     `json:"instance_guid" toon:"instance_guid"`
	Name        string     `json:"instance_name" toon:"instance_name"`
	Label       string     `json:"label"         toon:"label"`
	Plan        string     `json:"plan"          toon:"plan"`
	ServiceKeys []skOutKey `json:"service_keys"  toon:"service_keys"`
}

type skOutSpace struct {
	ID               string          `json:"space_id"          toon:"space_id"`
	Name             string          `json:"space_name"        toon:"space_name"`
	ServiceInstances []skOutInstance `json:"service_instances" toon:"service_instances"`
}

type skOutOrg struct {
	ID     string       `json:"org_id"   toon:"org_id"`
	Name   string       `json:"org_name" toon:"org_name"`
	Spaces []skOutSpace `json:"spaces"   toon:"spaces"`
}

type skOutRegion struct {
	ID   string     `json:"region" toon:"region"`
	Orgs []skOutOrg `json:"orgs"   toon:"orgs"`
}

type skOutDoc struct {
	Regions []skOutRegion `json:"regions" toon:"regions"`
}

// ── matched keys (phase 1) ───────────────────────────────────────────────────

// skMatchedKey is one service key that passed org/space/instance/key-pattern
// filtering, annotated with every level of its hierarchy and the
// already-authenticated client for its region.
type skMatchedKey struct {
	Client             *cf.Client
	Region             string
	OrgID              string
	OrgName            string
	SpaceID            string
	SpaceName          string
	InstanceGUID       string
	InstanceName       string
	Label              string
	Plan               string
	KeyGUID            string
	KeyName            string
	CreatedAt          string
	UpdatedAt          string
	LastOperationType  string
	LastOperationState string
	Credentials        map[string]interface{}
}

type skRegionResult struct {
	Region string
	Keys   []skMatchedKey
	Err    error
}

// ── command ───────────────────────────────────────────────────────────────────

var serviceKeysCmd = &cobra.Command{
	Use:   "service-keys",
	Short: "List service keys across orgs and spaces",
	Long: `List every service key (service credential binding of type "key") across
one or more regions, organizations, and spaces, nested as
region -> org -> space -> service_instance -> service_keys.

If neither --org nor --orgs is given, the default org scope selected via
'bo orgs' is used. --spaces/--space narrows to specific space names
(comma-separated, exact, case-insensitive) within each target org; if
omitted, every accessible space in scope is searched. --service-instances
narrows to specific service instance names and --service-keys narrows to
specific service key names: each is a comma-separated list of patterns,
e.g. '*-hdi,*-db' — a glob (via filepath.Match) if it contains * ? [,
otherwise a case-insensitive substring; omit either to match everything at
that level.

Without --full, only service key metadata is included (name, guid,
created_at, updated_at, last_operation_type, last_operation_state) — not
the "created by" user, which the CF API does not expose on this resource.
With --full, each key's credentials are also fetched and included — this
outputs live secrets, so prefer --output to a file over printing to a
shared terminal.

Output formats (--format):
  toon  Token-Oriented Object Notation — compact, human-readable (default)
  json  JSON document
  csv   Flat CSV rows, one per service key (credentials as a JSON column with --full)

Use --output/-o to write the result to a file instead of stdout.

If --regions is omitted the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		orgsFile, _ := cmd.Flags().GetString("orgs")
		excludeOrgsFile, _ := cmd.Flags().GetString("excludeOrgs")
		orgGUID, _ := cmd.Flags().GetString("org")
		spacesFlag, _ := cmd.Flags().GetString("spaces")
		spaceFlag, _ := cmd.Flags().GetString("space")
		instancesFlag, _ := cmd.Flags().GetString("service-instances")
		keysFlag, _ := cmd.Flags().GetString("service-keys")
		fullFlag, _ := cmd.Flags().GetBool("full")
		outputFile, _ := cmd.Flags().GetString("output")
		format, _ := cmd.Flags().GetString("format")

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
		results := make([]skRegionResult, len(apiURLs))
		var wg sync.WaitGroup
		for i, apiURL := range apiURLs {
			wg.Add(1)
			go func(idx int, url string) {
				defer wg.Done()
				regionName := store.APIURLToRegion(url)

				tok, ok := creds.Tokens[url]
				if !ok {
					results[idx] = skRegionResult{Region: regionName,
						Err: fmt.Errorf("no token — run: bo login --regions %s", regionName)}
					return
				}
				client := cf.NewClient(url, tok.AccessToken)
				client.SetTokenRefresher(makeTokenRefresher(url, tok.AccessToken))

				orgs, err := client.ListOrganizations(ctx)
				if err != nil {
					results[idx] = skRegionResult{Region: regionName,
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
					results[idx] = skRegionResult{Region: regionName}
					return
				}

				orgByGUID := make(map[string]cf.Organization, len(filteredOrgs))
				orgGUIDs := make([]string, len(filteredOrgs))
				for i, o := range filteredOrgs {
					orgGUIDs[i] = o.GUID
					orgByGUID[o.GUID] = o
				}

				spaces, err := client.ListSpacesByOrgs(ctx, orgGUIDs)
				if err != nil {
					results[idx] = skRegionResult{Region: regionName,
						Err: fmt.Errorf("listing spaces: %w", err)}
					return
				}

				type spaceInfo struct {
					GUID, Name string
					OrgGUID    string
					OrgName    string
				}
				spaceByGUID := make(map[string]spaceInfo)
				var spaceGUIDs []string
				for _, sp := range spaces {
					if !scMatchesSpaceName(spacesFlag, sp.Name) {
						continue
					}
					orgGUID := sp.Relationships.Organization.Data.GUID
					spaceGUIDs = append(spaceGUIDs, sp.GUID)
					spaceByGUID[sp.GUID] = spaceInfo{
						GUID:    sp.GUID,
						Name:    sp.Name,
						OrgGUID: orgGUID,
						OrgName: orgByGUID[orgGUID].Name,
					}
				}
				if len(spaceGUIDs) == 0 {
					results[idx] = skRegionResult{Region: regionName}
					return
				}

				instances, err := client.ListServiceInstancesFullBySpacesAnyType(ctx, spaceGUIDs)
				if err != nil {
					results[idx] = skRegionResult{Region: regionName,
						Err: fmt.Errorf("listing service instances: %w", err)}
					return
				}

				var filteredInstances []cf.ServiceInstanceFull
				planGUIDSet := make(map[string]struct{})
				for _, inst := range instances {
					if !scMatchesAppPattern(instancesFlag, inst.Name) {
						continue
					}
					filteredInstances = append(filteredInstances, inst)
					if g := inst.Relationships.ServicePlan.Data.GUID; g != "" {
						planGUIDSet[g] = struct{}{}
					}
				}
				if len(filteredInstances) == 0 {
					results[idx] = skRegionResult{Region: regionName}
					return
				}

				planGUIDs := make([]string, 0, len(planGUIDSet))
				for g := range planGUIDSet {
					planGUIDs = append(planGUIDs, g)
				}
				planDetails, err := client.ListServicePlanDetails(ctx, planGUIDs)
				if err != nil {
					results[idx] = skRegionResult{Region: regionName,
						Err: fmt.Errorf("fetching service plan details: %w", err)}
					return
				}

				instanceByGUID := make(map[string]cf.ServiceInstanceFull, len(filteredInstances))
				instanceGUIDs := make([]string, len(filteredInstances))
				for i, inst := range filteredInstances {
					instanceGUIDs[i] = inst.GUID
					instanceByGUID[inst.GUID] = inst
				}

				bindings, err := client.ListServiceCredentialBindingsByInstances(ctx, instanceGUIDs)
				if err != nil {
					results[idx] = skRegionResult{Region: regionName,
						Err: fmt.Errorf("listing service credential bindings: %w", err)}
					return
				}

				var matched []skMatchedKey
				for _, b := range bindings {
					if b.Type != "key" {
						continue
					}
					if !scMatchesAppPattern(keysFlag, b.Name) {
						continue
					}
					inst := instanceByGUID[b.Relationships.ServiceInstance.Data.GUID]
					info := spaceByGUID[inst.Relationships.Space.Data.GUID]
					pd := planDetails[inst.Relationships.ServicePlan.Data.GUID]
					matched = append(matched, skMatchedKey{
						Client:             client,
						Region:             regionName,
						OrgID:              info.OrgGUID,
						OrgName:            info.OrgName,
						SpaceID:            info.GUID,
						SpaceName:          info.Name,
						InstanceGUID:       inst.GUID,
						InstanceName:       inst.Name,
						Label:              pd.ServiceName,
						Plan:               pd.Name,
						KeyGUID:            b.GUID,
						KeyName:            b.Name,
						CreatedAt:          b.CreatedAt,
						UpdatedAt:          b.UpdatedAt,
						LastOperationType:  b.LastOperation.Type,
						LastOperationState: b.LastOperation.State,
					})
				}

				results[idx] = skRegionResult{Region: regionName, Keys: matched}
			}(i, apiURL)
		}
		wg.Wait()

		var allMatched []skMatchedKey
		for _, r := range results {
			if r.Err != nil {
				fmt.Fprintf(os.Stderr, "warning: region %q: %v\n", r.Region, r.Err)
				continue
			}
			allMatched = append(allMatched, r.Keys...)
		}

		// Phase 2: with --full, fetch each matched key's credentials, bounded
		// concurrency across the whole matched set.
		if fullFlag && len(allMatched) > 0 {
			var credWg sync.WaitGroup
			sem := make(chan struct{}, skConcurrency)
			for i := range allMatched {
				credWg.Add(1)
				sem <- struct{}{}
				go func(i int) {
					defer credWg.Done()
					defer func() { <-sem }()

					k := &allMatched[i]
					credentials, err := k.Client.GetServiceCredentialDetails(ctx, k.KeyGUID)
					if err != nil {
						fmt.Fprintf(os.Stderr, "warning: %s/%s/%s/%s: fetching credentials: %v\n",
							k.OrgName, k.SpaceName, k.InstanceName, k.KeyName, err)
						return
					}
					k.Credentials = credentials
				}(i)
			}
			credWg.Wait()
		}

		sort.Slice(allMatched, func(i, j int) bool {
			if allMatched[i].Region != allMatched[j].Region {
				return allMatched[i].Region < allMatched[j].Region
			}
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

		out, closeOut, err := resolveOutputWriter(outputFile)
		if err != nil {
			return err
		}
		defer closeOut()

		switch strings.ToLower(format) {
		case "json":
			return writeServiceKeysJSON(out, allMatched)
		case "csv":
			return writeServiceKeysCSV(out, allMatched, fullFlag)
		default:
			return writeServiceKeysToon(out, allMatched)
		}
	},
}

// ── output builder ───────────────────────────────────────────────────────────

func buildServiceKeysDoc(matched []skMatchedKey) skOutDoc {
	var doc skOutDoc
	var curRegion *skOutRegion
	var curOrg *skOutOrg
	var curSpace *skOutSpace
	var curInstance *skOutInstance

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
		if curOrg != nil && curRegion != nil {
			curRegion.Orgs = append(curRegion.Orgs, *curOrg)
		}
		curOrg = nil
	}
	flushRegion := func() {
		flushOrg()
		if curRegion != nil {
			doc.Regions = append(doc.Regions, *curRegion)
		}
		curRegion = nil
	}

	for _, k := range matched {
		if curRegion == nil || curRegion.ID != k.Region {
			flushRegion()
			curRegion = &skOutRegion{ID: k.Region}
		}
		if curOrg == nil || curOrg.ID != k.OrgID {
			flushOrg()
			curOrg = &skOutOrg{ID: k.OrgID, Name: k.OrgName}
		}
		if curSpace == nil || curSpace.ID != k.SpaceID {
			flushSpace()
			curSpace = &skOutSpace{ID: k.SpaceID, Name: k.SpaceName}
		}
		if curInstance == nil || curInstance.GUID != k.InstanceGUID {
			flushInstance()
			curInstance = &skOutInstance{GUID: k.InstanceGUID, Name: k.InstanceName, Label: k.Label, Plan: k.Plan}
		}
		curInstance.ServiceKeys = append(curInstance.ServiceKeys, skOutKey{
			GUID:               k.KeyGUID,
			Name:               k.KeyName,
			CreatedAt:          k.CreatedAt,
			UpdatedAt:          k.UpdatedAt,
			LastOperationType:  k.LastOperationType,
			LastOperationState: k.LastOperationState,
			Credentials:        k.Credentials,
		})
	}
	flushRegion()

	return doc
}

func writeServiceKeysToon(w io.Writer, matched []skMatchedKey) error {
	doc := buildServiceKeysDoc(matched)
	out, err := toonenc.Marshal(doc, toonenc.WithIndent(2))
	if err != nil {
		return fmt.Errorf("encoding TOON: %w", err)
	}
	if _, err := w.Write(out); err != nil {
		return err
	}
	_, err = fmt.Fprintln(w)
	return err
}

func writeServiceKeysJSON(w io.Writer, matched []skMatchedKey) error {
	doc := buildServiceKeysDoc(matched)
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding JSON: %w", err)
	}
	fmt.Fprintln(w, string(out))
	return nil
}

func writeServiceKeysCSV(w io.Writer, matched []skMatchedKey, full bool) error {
	csvW := csv.NewWriter(w)
	defer csvW.Flush()

	header := []string{
		"region", "org_id", "org_name", "space_id", "space_name",
		"instance_guid", "instance_name", "label", "plan",
		"key_guid", "key_name", "created_at", "updated_at",
		"last_operation_type", "last_operation_state",
	}
	if full {
		header = append(header, "credentials")
	}
	if err := csvW.Write(header); err != nil {
		return err
	}

	for _, k := range matched {
		row := []string{
			k.Region, k.OrgID, k.OrgName, k.SpaceID, k.SpaceName,
			k.InstanceGUID, k.InstanceName, k.Label, k.Plan,
			k.KeyGUID, k.KeyName, k.CreatedAt, k.UpdatedAt,
			k.LastOperationType, k.LastOperationState,
		}
		if full {
			credJSON := ""
			if k.Credentials != nil {
				b, err := json.Marshal(k.Credentials)
				if err != nil {
					return fmt.Errorf("encoding credentials for %s/%s/%s/%s: %w",
						k.OrgName, k.SpaceName, k.InstanceName, k.KeyName, err)
				}
				credJSON = string(b)
			}
			row = append(row, credJSON)
		}
		if err := csvW.Write(row); err != nil {
			return err
		}
	}
	return nil
}

func init() {
	serviceKeysCmd.GroupID = "cf-org"
	rootCmd.AddCommand(serviceKeysCmd)
	serviceKeysCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	serviceKeysCmd.Flags().String("org", "", "Org GUID to target; only this org will be listed")
	serviceKeysCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	serviceKeysCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,org_id,org_name)")
	serviceKeysCmd.Flags().String("spaces", "", "Only include these comma-separated, exact (case-insensitive) space names; default is every accessible space in scope")
	serviceKeysCmd.Flags().String("space", "", "Equivalent to --spaces with a single name (combined with --spaces if both are given)")
	serviceKeysCmd.Flags().String("service-instances", "", "Only include service instances whose name matches any of these comma-separated patterns (glob, e.g. '*-hdi,*-db', or substring); default is every service instance in scope")
	serviceKeysCmd.Flags().String("service-keys", "", "Only include service keys whose name matches any of these comma-separated patterns (glob, e.g. 'btp*,temp*', or substring); default is every service key in scope")
	serviceKeysCmd.Flags().Bool("full", false, "Also fetch and include each service key's credentials (live secrets)")
	serviceKeysCmd.Flags().StringP("output", "o", "", "Write output to this file instead of stdout")
	serviceKeysCmd.Flags().String("format", "toon", "Output format: toon (default), json, or csv")
}
