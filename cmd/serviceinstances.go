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

// siConcurrency bounds how many CF API calls (credential lookups when --full
// is given) run at once across the whole matched set.
const siConcurrency = 8

// ── output types ──────────────────────────────────────────────────────────────

type siOutInstance struct {
	GUID               string                 `json:"instance_guid"         toon:"instance_guid"`
	Name               string                 `json:"instance_name"         toon:"instance_name"`
	Type               string                 `json:"type"                  toon:"type"`
	Label              string                 `json:"label"                 toon:"label"`
	Plan               string                 `json:"plan"                  toon:"plan"`
	Tags               []string               `json:"tags,omitempty"        toon:"tags,omitempty"`
	CreatedAt          string                 `json:"created_at"            toon:"created_at"`
	UpdatedAt          string                 `json:"updated_at"            toon:"updated_at"`
	LastOperationType  string                 `json:"last_operation_type"   toon:"last_operation_type"`
	LastOperationState string                 `json:"last_operation_state"  toon:"last_operation_state"`
	Credentials        map[string]interface{} `json:"credentials,omitempty" toon:"credentials,omitempty"`
}

type siOutSpace struct {
	ID               string          `json:"space_id"          toon:"space_id"`
	Name             string          `json:"space_name"        toon:"space_name"`
	ServiceInstances []siOutInstance `json:"service_instances" toon:"service_instances"`
}

type siOutOrg struct {
	ID     string       `json:"org_id"   toon:"org_id"`
	Name   string       `json:"org_name" toon:"org_name"`
	Spaces []siOutSpace `json:"spaces"   toon:"spaces"`
}

type siOutRegion struct {
	ID   string     `json:"region" toon:"region"`
	Orgs []siOutOrg `json:"orgs"   toon:"orgs"`
}

type siOutDoc struct {
	Regions []siOutRegion `json:"regions" toon:"regions"`
}

// ── matched instances (phase 1) ─────────────────────────────────────────────

// siMatchedInstance is one service instance that passed org/space/
// name-pattern filtering, annotated with every level of its hierarchy and
// the already-authenticated client for its region.
type siMatchedInstance struct {
	Client             *cf.Client
	Region             string
	OrgID              string
	OrgName            string
	SpaceID            string
	SpaceName          string
	InstanceGUID       string
	InstanceName       string
	Type               string
	Label              string
	Plan               string
	Tags               []string
	CreatedAt          string
	UpdatedAt          string
	LastOperationType  string
	LastOperationState string
	Credentials        map[string]interface{}
}

type siRegionResult struct {
	Region    string
	Instances []siMatchedInstance
	Err       error
}

// ── command ───────────────────────────────────────────────────────────────────

var serviceInstancesCmd = &cobra.Command{
	Use:   "service-instances",
	Short: "List service instances across orgs and spaces",
	Long: `List every service instance (managed or user-provided) across one or more
regions, organizations, and spaces, nested as
region -> org -> space -> service_instances.

If neither --org nor --orgs is given, the default org scope selected via
'bo orgs' is used. --spaces/--space narrows to specific space names
(comma-separated, exact, case-insensitive) within each target org; if
omitted, every accessible space in scope is searched. --service-instances
narrows to specific service instance names: a comma-separated list of
patterns, e.g. '*-hdi,*-db' — a glob (via filepath.Match) if it contains
* ? [, otherwise a case-insensitive substring; omit it to match every
service instance in scope.

Without --full, only service instance metadata is included (name, guid,
type, label, plan, tags, created_at, updated_at, last_operation_type,
last_operation_state) — not the "created by" user, which the CF API does
not expose on this resource. With --full, each user-provided instance's
credentials are also fetched and included (managed instances have no
credentials of their own — only their service keys do, see
'bo service-keys' — so this field is left empty for those). This outputs
live secrets, so prefer --output to a file over printing to a shared
terminal.

Output formats (--format):
  toon  Token-Oriented Object Notation — compact, human-readable (default)
  json  JSON document
  csv   Flat CSV rows, one per service instance (credentials as a JSON column with --full)

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

		// Phase 1: resolve matching service instances (orgs -> spaces ->
		// service instances, managed or user-provided), in parallel across
		// regions.
		results := make([]siRegionResult, len(apiURLs))
		var wg sync.WaitGroup
		for i, apiURL := range apiURLs {
			wg.Add(1)
			go func(idx int, url string) {
				defer wg.Done()
				regionName := store.APIURLToRegion(url)

				tok, ok := creds.Tokens[url]
				if !ok {
					results[idx] = siRegionResult{Region: regionName,
						Err: fmt.Errorf("no token — run: bo login --regions %s", regionName)}
					return
				}
				client := cf.NewClient(url, tok.AccessToken)
				client.SetTokenRefresher(makeTokenRefresher(url, tok.AccessToken))

				orgs, err := client.ListOrganizations(ctx)
				if err != nil {
					results[idx] = siRegionResult{Region: regionName,
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
					results[idx] = siRegionResult{Region: regionName}
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
					results[idx] = siRegionResult{Region: regionName,
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
					results[idx] = siRegionResult{Region: regionName}
					return
				}

				instances, err := client.ListServiceInstancesFullBySpacesAnyType(ctx, spaceGUIDs)
				if err != nil {
					results[idx] = siRegionResult{Region: regionName,
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
					results[idx] = siRegionResult{Region: regionName}
					return
				}

				planGUIDs := make([]string, 0, len(planGUIDSet))
				for g := range planGUIDSet {
					planGUIDs = append(planGUIDs, g)
				}
				planDetails, err := client.ListServicePlanDetails(ctx, planGUIDs)
				if err != nil {
					results[idx] = siRegionResult{Region: regionName,
						Err: fmt.Errorf("fetching service plan details: %w", err)}
					return
				}

				var matched []siMatchedInstance
				for _, inst := range filteredInstances {
					info := spaceByGUID[inst.Relationships.Space.Data.GUID]
					pd := planDetails[inst.Relationships.ServicePlan.Data.GUID]
					matched = append(matched, siMatchedInstance{
						Client:             client,
						Region:             regionName,
						OrgID:              info.OrgGUID,
						OrgName:            info.OrgName,
						SpaceID:            info.GUID,
						SpaceName:          info.Name,
						InstanceGUID:       inst.GUID,
						InstanceName:       inst.Name,
						Type:               inst.Type,
						Label:              pd.ServiceName,
						Plan:               pd.Name,
						Tags:               inst.Tags,
						CreatedAt:          inst.CreatedAt,
						UpdatedAt:          inst.UpdatedAt,
						LastOperationType:  inst.LastOperation.Type,
						LastOperationState: inst.LastOperation.State,
					})
				}

				results[idx] = siRegionResult{Region: regionName, Instances: matched}
			}(i, apiURL)
		}
		wg.Wait()

		var allMatched []siMatchedInstance
		for _, r := range results {
			if r.Err != nil {
				fmt.Fprintf(os.Stderr, "warning: region %q: %v\n", r.Region, r.Err)
				continue
			}
			allMatched = append(allMatched, r.Instances...)
		}

		// Phase 2: with --full, fetch each matched user-provided instance's
		// credentials, bounded concurrency across the whole matched set.
		// Managed instances have no credentials of their own and are
		// silently left as-is.
		if fullFlag && len(allMatched) > 0 {
			var credWg sync.WaitGroup
			sem := make(chan struct{}, siConcurrency)
			for i := range allMatched {
				if allMatched[i].Type != "user-provided" {
					continue
				}
				credWg.Add(1)
				sem <- struct{}{}
				go func(i int) {
					defer credWg.Done()
					defer func() { <-sem }()

					inst := &allMatched[i]
					credentials, err := inst.Client.GetServiceInstanceCredentials(ctx, inst.InstanceGUID)
					if err != nil {
						fmt.Fprintf(os.Stderr, "warning: %s/%s/%s: fetching credentials: %v\n",
							inst.OrgName, inst.SpaceName, inst.InstanceName, err)
						return
					}
					inst.Credentials = credentials
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
			return allMatched[i].InstanceName < allMatched[j].InstanceName
		})

		out, closeOut, err := resolveOutputWriter(outputFile)
		if err != nil {
			return err
		}
		defer closeOut()

		switch strings.ToLower(format) {
		case "json":
			return writeServiceInstancesJSON(out, allMatched)
		case "csv":
			return writeServiceInstancesCSV(out, allMatched, fullFlag)
		default:
			return writeServiceInstancesToon(out, allMatched)
		}
	},
}

// ── output builder ───────────────────────────────────────────────────────────

func buildServiceInstancesDoc(matched []siMatchedInstance) siOutDoc {
	var doc siOutDoc
	var curRegion *siOutRegion
	var curOrg *siOutOrg
	var curSpace *siOutSpace

	flushSpace := func() {
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

	for _, inst := range matched {
		if curRegion == nil || curRegion.ID != inst.Region {
			flushRegion()
			curRegion = &siOutRegion{ID: inst.Region}
		}
		if curOrg == nil || curOrg.ID != inst.OrgID {
			flushOrg()
			curOrg = &siOutOrg{ID: inst.OrgID, Name: inst.OrgName}
		}
		if curSpace == nil || curSpace.ID != inst.SpaceID {
			flushSpace()
			curSpace = &siOutSpace{ID: inst.SpaceID, Name: inst.SpaceName}
		}
		curSpace.ServiceInstances = append(curSpace.ServiceInstances, siOutInstance{
			GUID:               inst.InstanceGUID,
			Name:               inst.InstanceName,
			Type:               inst.Type,
			Label:              inst.Label,
			Plan:               inst.Plan,
			Tags:               inst.Tags,
			CreatedAt:          inst.CreatedAt,
			UpdatedAt:          inst.UpdatedAt,
			LastOperationType:  inst.LastOperationType,
			LastOperationState: inst.LastOperationState,
			Credentials:        inst.Credentials,
		})
	}
	flushRegion()

	return doc
}

func writeServiceInstancesToon(w io.Writer, matched []siMatchedInstance) error {
	doc := buildServiceInstancesDoc(matched)
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

func writeServiceInstancesJSON(w io.Writer, matched []siMatchedInstance) error {
	doc := buildServiceInstancesDoc(matched)
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding JSON: %w", err)
	}
	fmt.Fprintln(w, string(out))
	return nil
}

func writeServiceInstancesCSV(w io.Writer, matched []siMatchedInstance, full bool) error {
	csvW := csv.NewWriter(w)
	defer csvW.Flush()

	header := []string{
		"region", "org_id", "org_name", "space_id", "space_name",
		"instance_guid", "instance_name", "type", "label", "plan", "tags",
		"created_at", "updated_at", "last_operation_type", "last_operation_state",
	}
	if full {
		header = append(header, "credentials")
	}
	if err := csvW.Write(header); err != nil {
		return err
	}

	for _, inst := range matched {
		row := []string{
			inst.Region, inst.OrgID, inst.OrgName, inst.SpaceID, inst.SpaceName,
			inst.InstanceGUID, inst.InstanceName, inst.Type, inst.Label, inst.Plan,
			strings.Join(inst.Tags, ";"),
			inst.CreatedAt, inst.UpdatedAt, inst.LastOperationType, inst.LastOperationState,
		}
		if full {
			credJSON := ""
			if inst.Credentials != nil {
				b, err := json.Marshal(inst.Credentials)
				if err != nil {
					return fmt.Errorf("encoding credentials for %s/%s/%s: %w",
						inst.OrgName, inst.SpaceName, inst.InstanceName, err)
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
	serviceInstancesCmd.GroupID = "cf-org"
	rootCmd.AddCommand(serviceInstancesCmd)
	serviceInstancesCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	serviceInstancesCmd.Flags().String("org", "", "Org GUID to target; only this org will be listed")
	serviceInstancesCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	serviceInstancesCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,org_id,org_name)")
	serviceInstancesCmd.Flags().String("spaces", "", "Only include these comma-separated, exact (case-insensitive) space names; default is every accessible space in scope")
	serviceInstancesCmd.Flags().String("space", "", "Equivalent to --spaces with a single name (combined with --spaces if both are given)")
	serviceInstancesCmd.Flags().String("service-instances", "", "Only include service instances whose name matches any of these comma-separated patterns (glob, e.g. '*-hdi,*-db', or substring); default is every service instance in scope")
	serviceInstancesCmd.Flags().Bool("full", false, "Also fetch and include each user-provided instance's credentials (live secrets); managed instances have none of their own")
	serviceInstancesCmd.Flags().StringP("output", "o", "", "Write output to this file instead of stdout")
	serviceInstancesCmd.Flags().String("format", "toon", "Output format: toon (default), json, or csv")
}
