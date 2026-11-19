package cmd

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	toonenc "github.com/toon-format/toon-go"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/destination"
	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

// ── output types ──────────────────────────────────────────────────────────────

// sdSpaceDoc is one space's instance-level destinations, flattened across
// every destination service instance found in it — each destination's own
// map carries its originating "destination_service_id"/
// "destination_service_name" (see sdTagDestInstance), so that grouping is
// not lost by the flattening.
type sdSpaceDoc struct {
	SpaceID      string              `json:"space_id"      toon:"space_id"`
	SpaceName    string              `json:"space_name"    toon:"space_name"`
	Destinations []map[string]string `json:"destinations"  toon:"destinations"`
}

// sdOrgDoc is one org's spaces (each with its own flattened destinations),
// the result shape for 'bo space-destinations' once --space is omitted and
// every accessible space is in scope — or, with --space given, a single org
// with a single space.
type sdOrgDoc struct {
	OrgID   string       `json:"org_id"   toon:"org_id"`
	OrgName string       `json:"org_name" toon:"org_name"`
	Spaces  []sdSpaceDoc `json:"spaces"   toon:"spaces"`
}

// sdLeadingCols are space-destinations' own structural columns (see
// destFlatRow) — org and space identity; the owning destination service
// instance (destination_service_id/destination_service_name) is already
// baked into each destination's own properties by sdTagDestInstance, so it
// isn't a separate structural column here.
var sdLeadingCols = []string{"org_id", "org_name", "space_id", "space_name"}

// flattenSdRows converts docs into one flat destFlatRow per destination.
func flattenSdRows(docs []sdOrgDoc) []destFlatRow {
	var rows []destFlatRow
	for _, org := range docs {
		for _, sp := range org.Spaces {
			for _, d := range sp.Destinations {
				rows = append(rows, destFlatRow{
					OrgID: org.OrgID, OrgName: org.OrgName, SpaceID: sp.SpaceID, SpaceName: sp.SpaceName,
					Props: d,
				})
			}
		}
	}
	return rows
}

// sdTagDestInstance returns a copy of dest with "destination_service_id"
// and "destination_service_name" set to instGUID/instName, so a destination
// still carries which destination service instance it came from after
// sdSpaceDoc.Destinations flattens away the per-instance grouping.
func sdTagDestInstance(dest map[string]string, instGUID, instName string) map[string]string {
	tagged := make(map[string]string, len(dest)+2)
	for k, v := range dest {
		tagged[k] = v
	}
	tagged["destination_service_id"] = instGUID
	tagged["destination_service_name"] = instName
	return tagged
}

// sdMatchesFilter reports whether a destination matches the filter. The filter
// is tested case-insensitively against every property value (and key). When
// the filter contains glob metacharacters (* ? [) filepath.Match is used;
// otherwise a substring match is performed.
func sdMatchesFilter(dest map[string]string, filter string) bool {
	if filter == "" {
		return true
	}
	isGlob := strings.ContainsAny(filter, "*?[")
	fl := strings.ToLower(filter)
	for k, v := range dest {
		kl, vl := strings.ToLower(k), strings.ToLower(v)
		if isGlob {
			if m, _ := filepath.Match(fl, vl); m {
				return true
			}
			if m, _ := filepath.Match(fl, kl); m {
				return true
			}
		} else {
			if strings.Contains(vl, fl) || strings.Contains(kl, fl) {
				return true
			}
		}
	}
	return false
}

// sdMatchesAnyKeyword reports whether any comma-separated keyword in pattern
// matches dest, per sdMatchesFilter's rules (substring, or glob when the
// keyword contains * ? [). Returns false when pattern is empty.
func sdMatchesAnyKeyword(pattern string, dest map[string]string) bool {
	if pattern == "" {
		return false
	}
	for _, kw := range splitCSV(pattern) {
		if sdMatchesFilter(dest, kw) {
			return true
		}
	}
	return false
}

// ── shared setup ──────────────────────────────────────────────────────────────

// sdDestClient is a ready-to-use destination service client (token refreshed).
// Cleanup is non-nil only when resolveDestInstanceClient created a temporary
// service key for this client under destKeyOptionTransient — see
// cleanupDestClients.
type sdDestClient struct {
	InstanceGUID string
	InstanceName string
	URI          string
	Token        string
	Cleanup      func()
}

// resolveSpaceDestClients finds the target space across regions, discovers all
// destination service instances in it, loads/fetches their credentials (per
// keyOption when a service key is missing — see resolveDestInstanceClient),
// refreshes tokens and persists the cache. It returns the space name and one
// client per instance. Callers must call cleanupDestClients(clients...) once
// they're done using the returned clients.
func resolveSpaceDestClients(
	ctx context.Context,
	cmd *cobra.Command,
	spaceGUID string,
	creds *store.Credentials,
	apiURLs []string,
	keyOption destKeyOption,
) (spaceName string, clients []sdDestClient, err error) {

	// ── find the space and its CF client ─────────────────────────────────────
	// The client that successfully finds the space is reused for all subsequent
	// CF calls. This avoids creating a second client from the stale in-memory
	// token (creds.Tokens) when the first client already triggered a refresh.
	var (
		spaceObj *cf.Space
		cfClient *cf.Client
	)
	for _, apiURL := range apiURLs {
		tok, ok := creds.Tokens[apiURL]
		if !ok {
			continue
		}
		c := cf.NewClient(apiURL, tok.AccessToken)
		c.SetTokenRefresher(makeTokenRefresher(apiURL, tok.AccessToken))
		s, findErr := c.FindSpaceByGUID(ctx, spaceGUID)
		if findErr != nil || s == nil {
			continue
		}
		spaceObj = s
		cfClient = c
		break
	}
	if spaceObj == nil {
		return "", nil, fmt.Errorf("space %q not found in any active region — check --regions", spaceGUID)
	}
	spaceName = spaceObj.Name

	clients, err = resolveSpaceDestClientsFor(ctx, cmd, cfClient, spaceGUID, spaceName, creds, keyOption)
	saveDestCache(cmd, creds)
	if err == nil && len(clients) == 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: no destination service instances found in space %s (%s)\n", spaceName, spaceGUID)
	}
	return spaceName, clients, err
}

// resolveSpaceDestClientsFor is resolveSpaceDestClients' second half, for a
// caller that has already found the space's own CF client (e.g. while
// scanning every space in an org, where the same client is reused across
// many spaces instead of re-searching for each one across every region) —
// it looks up the destination/lite plan and delegates to
// resolveSpaceDestClientsForPlan. Unlike resolveSpaceDestClients, it does
// NOT persist the token cache itself; callers that invoke it directly
// (rather than through resolveSpaceDestClients) must call saveDestCache
// once after they're done, rather than once per space.
func resolveSpaceDestClientsFor(ctx context.Context, cmd *cobra.Command, cfClient *cf.Client, spaceGUID, spaceName string, creds *store.Credentials, keyOption destKeyOption) ([]sdDestClient, error) {
	destPlan, planErr := cfClient.FindServicePlan(ctx, "destination", "lite")
	if planErr != nil {
		return nil, fmt.Errorf("looking up destination service plan: %w", planErr)
	}
	if destPlan == nil {
		return nil, fmt.Errorf("destination service plan 'lite' not found in region %s", store.APIURLToRegion(cfClient.BaseURL()))
	}
	return resolveSpaceDestClientsForPlan(ctx, cmd, cfClient, spaceGUID, spaceName, destPlan.GUID, creds, keyOption)
}

// resolveSpaceDestClientsForPlan is resolveSpaceDestClientsFor's core, for a
// caller that has also already resolved the destination/lite plan GUID once
// (e.g. per region, rather than refetching it for every space scanned in
// that region). Discovers destination service instances in the space and
// resolves/caches an OAuth token for each (per keyOption when a service key
// is missing — see resolveDestInstanceClient). Does not persist the cache —
// see resolveSpaceDestClientsFor.
func resolveSpaceDestClientsForPlan(ctx context.Context, cmd *cobra.Command, cfClient *cf.Client, spaceGUID, spaceName, destPlanGUID string, creds *store.Credentials, keyOption destKeyOption) ([]sdDestClient, error) {
	// ── find destination service instances in the space ───────────────────────
	instances, instErr := cfClient.ListServiceInstancesInSpace(ctx, spaceGUID, destPlanGUID)
	if instErr != nil {
		return nil, fmt.Errorf("listing destination instances in space: %w", instErr)
	}
	if len(instances) == 0 {
		return nil, nil
	}

	// ── ensure cache map exists ───────────────────────────────────────────────
	if creds.SpaceDestServices == nil {
		creds.SpaceDestServices = make(map[string]map[string]*store.DestInstanceCache)
	}
	if creds.SpaceDestServices[spaceGUID] == nil {
		creds.SpaceDestServices[spaceGUID] = make(map[string]*store.DestInstanceCache)
	}
	spaceCache := creds.SpaceDestServices[spaceGUID]
	var clients []sdDestClient

	// ── per-instance: refresh token if needed, fetching key from CF each time ─
	// Client ID and client secret are intentionally NOT cached locally.
	// They are fetched from CF on demand whenever a new token is needed and
	// discarded immediately — only the token (+ tokenURL + URI) is persisted.
	for _, inst := range instances {
		c, ok, resolveErr := resolveDestInstanceClient(ctx, cmd, cfClient, spaceName, inst, spaceCache, keyOption)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if ok {
			clients = append(clients, c)
		}
	}

	creds.SpaceDestServices[spaceGUID] = spaceCache
	return clients, nil
}

// saveDestCache reloads credentials from disk before saving so that any CF
// token refresh written by the tokenRefresher (which operates on its own
// store.Load copy) is not overwritten by the stale in-memory creds pointer.
// Only SpaceDestServices is taken from creds; everything else comes from disk.
func saveDestCache(cmd *cobra.Command, creds *store.Credentials) {
	fresh, err := store.Load()
	if err != nil {
		// Disk read failed — fall back to saving what we have.
		fresh = creds
	} else {
		fresh.SpaceDestServices = creds.SpaceDestServices
	}
	if saveErr := store.Save(fresh); saveErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: saving destination credentials: %v\n", saveErr)
	}
}

// loadDestinationsJSON reads the destinations JSON file and returns the raw
// message (validated as a JSON array). Also returns the Names for delete use.
func loadDestinationsJSON(path string) (json.RawMessage, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading destinations file: %w", err)
	}
	// validate JSON
	var raw []map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, fmt.Errorf("parsing destinations JSON (expected array): %w", err)
	}
	names := make([]string, 0, len(raw))
	for _, d := range raw {
		if n, ok := d["Name"].(string); ok && n != "" {
			names = append(names, n)
		}
	}
	return json.RawMessage(data), names, nil
}

// printActionResults prints per-item create/update results.
// action should be "created" or "updated".
// When items is non-empty (bulk API response with per-item status), each
// successful item prints "    {action}: {name}" and failures print to stderr.
// When items is empty (simple 201/200 with no body), it falls back to printing
// "    {action}: {name}" for every name in the names slice.
func printActionResults(cmd *cobra.Command, action string, names []string, items []destination.BulkResponseItem) {
	if len(items) > 0 {
		for _, it := range items {
			// The destination service may omit the per-item "status" field (leaving
			// it as 0). The reliable error indicator is "cause": if it is non-empty
			// the item failed; if it is empty and status is 0 or 2xx, it succeeded.
			isError := it.Cause != "" || (it.Status != 0 && (it.Status < 200 || it.Status >= 300))
			if !isError {
				fmt.Fprintf(cmd.OutOrStdout(), "    %s: %s\n", action, it.Name)
			} else {
				if it.Status != 0 {
					if it.Cause != "" {
						fmt.Fprintf(cmd.ErrOrStderr(), "    ERROR(%d): %s — %s\n", it.Status, it.Name, it.Cause)
					} else {
						fmt.Fprintf(cmd.ErrOrStderr(), "    ERROR(%d): %s\n", it.Status, it.Name)
					}
				} else {
					fmt.Fprintf(cmd.ErrOrStderr(), "    ERROR: %s — %s\n", it.Name, it.Cause)
				}
			}
		}
	} else {
		for _, name := range names {
			fmt.Fprintf(cmd.OutOrStdout(), "    %s: %s\n", action, name)
		}
	}
}

// ── space-destinations ────────────────────────────────────────────────────────

// resolveSpaceOrgInfo looks up the org GUID and name that spaceGUID belongs
// to, searching across apiURLs the same way resolveSpaceDestClients finds
// the space itself.
func resolveSpaceOrgInfo(ctx context.Context, creds *store.Credentials, apiURLs []string, spaceGUID string) (orgGUID, orgName string, err error) {
	for _, apiURL := range apiURLs {
		tok, ok := creds.Tokens[apiURL]
		if !ok {
			continue
		}
		c := cf.NewClient(apiURL, tok.AccessToken)
		c.SetTokenRefresher(makeTokenRefresher(apiURL, tok.AccessToken))
		s, findErr := c.FindSpaceByGUID(ctx, spaceGUID)
		if findErr != nil || s == nil {
			continue
		}
		orgGUID = s.Relationships.Organization.Data.GUID
		if orgs, orgErr := c.ListOrganizations(ctx); orgErr == nil {
			for _, o := range orgs {
				if o.GUID == orgGUID {
					orgName = o.Name
					break
				}
			}
		}
		return orgGUID, orgName, nil
	}
	return "", "", fmt.Errorf("space %q not found in any active region — check --regions", spaceGUID)
}

// sdRegionOrgsSpaces holds one region's orgs/spaces plus the destination/lite
// plan GUID found in that region (empty if the plan isn't offered there),
// gathered once up front so fetchAllSpaceDestinations doesn't re-resolve the
// plan or re-search for each space individually.
type sdRegionOrgsSpaces struct {
	Region   string
	Client   *cf.Client
	Orgs     []cf.Organization
	Spaces   []cf.Space
	PlanGUID string
	Err      error
}

// resolveAllOrgsSpacesForDest lists every org and space across apiURLs, in
// parallel per region, along with that region's destination/lite service
// plan GUID.
func resolveAllOrgsSpacesForDest(ctx context.Context, creds *store.Credentials, apiURLs []string) []sdRegionOrgsSpaces {
	results := make([]sdRegionOrgsSpaces, len(apiURLs))
	var wg sync.WaitGroup
	for i, apiURL := range apiURLs {
		wg.Add(1)
		go func(idx int, url string) {
			defer wg.Done()
			regionName := store.APIURLToRegion(url)

			tok, ok := creds.Tokens[url]
			if !ok {
				results[idx] = sdRegionOrgsSpaces{Region: regionName,
					Err: fmt.Errorf("no token — run: bo login --regions %s", regionName)}
				return
			}
			client := cf.NewClient(url, tok.AccessToken)
			client.SetTokenRefresher(makeTokenRefresher(url, tok.AccessToken))

			orgs, err := client.ListOrganizations(ctx)
			if err != nil {
				results[idx] = sdRegionOrgsSpaces{Region: regionName, Client: client,
					Err: fmt.Errorf("listing orgs: %w", err)}
				return
			}
			if len(orgs) == 0 {
				results[idx] = sdRegionOrgsSpaces{Region: regionName, Client: client}
				return
			}

			orgGUIDs := make([]string, len(orgs))
			for i, o := range orgs {
				orgGUIDs[i] = o.GUID
			}
			spaces, err := client.ListSpacesByOrgs(ctx, orgGUIDs)
			if err != nil {
				results[idx] = sdRegionOrgsSpaces{Region: regionName, Client: client,
					Err: fmt.Errorf("listing spaces: %w", err)}
				return
			}

			var planGUID string
			if plan, planErr := client.FindServicePlan(ctx, "destination", "lite"); planErr != nil {
				fmt.Fprintf(os.Stderr, "warning: [%s] looking up destination service plan: %v\n", regionName, planErr)
			} else if plan != nil {
				planGUID = plan.GUID
			}

			results[idx] = sdRegionOrgsSpaces{
				Region: regionName, Client: client,
				Orgs: orgs, Spaces: spaces, PlanGUID: planGUID,
			}
		}(i, apiURL)
	}
	wg.Wait()
	return results
}

// fetchAllSpaceDestinations is the --space-omitted discovery path: every
// accessible org/space is scanned for destination service instances and
// their instance-level destinations, flattened per space (see sdSpaceDoc).
// Orgs/spaces with no matching destinations are omitted from the result
// entirely, unlike the single --space path which always returns its one
// target space even when empty.
func fetchAllSpaceDestinations(ctx context.Context, cmd *cobra.Command, creds *store.Credentials, apiURLs []string, includePattern, excludePattern string, keyOption destKeyOption) ([]sdOrgDoc, error) {
	regionResults := resolveAllOrgsSpacesForDest(ctx, creds, apiURLs)

	var docs []sdOrgDoc
	for _, rr := range regionResults {
		if rr.Err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] %v\n", rr.Region, rr.Err)
			continue
		}
		if rr.Client == nil || rr.PlanGUID == "" || len(rr.Spaces) == 0 {
			continue
		}

		spacesByOrg := make(map[string][]cf.Space)
		for _, sp := range rr.Spaces {
			orgGUID := sp.Relationships.Organization.Data.GUID
			spacesByOrg[orgGUID] = append(spacesByOrg[orgGUID], sp)
		}

		for _, org := range rr.Orgs {
			var spaceDocs []sdSpaceDoc
			for _, sp := range spacesByOrg[org.GUID] {
				clients, resolveErr := resolveSpaceDestClientsForPlan(ctx, cmd, rr.Client, sp.GUID, sp.Name, rr.PlanGUID, creds, keyOption)
				if resolveErr != nil {
					if resolveErr == errAborted {
						saveDestCache(cmd, creds)
						return docs, errAborted
					}
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s/%s] %v\n", org.Name, sp.Name, resolveErr)
					continue
				}

				var dests []map[string]string
				for _, c := range clients {
					rawDests, fetchErr := destination.ListInstanceDestinationsFull(ctx, c.URI, c.Token)
					if fetchErr != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s/%s] instance %q: listing destinations: %v\n",
							org.Name, sp.Name, c.InstanceName, fetchErr)
						continue
					}
					for _, raw := range rawDests {
						if includePattern != "" && !sdMatchesAnyKeyword(includePattern, raw) {
							continue
						}
						if excludePattern != "" && sdMatchesAnyKeyword(excludePattern, raw) {
							continue
						}
						dests = append(dests, sdTagDestInstance(raw, c.InstanceGUID, c.InstanceName))
					}
				}
				cleanupDestClients(clients...)
				if len(dests) == 0 {
					continue
				}
				sort.Slice(dests, func(i, j int) bool { return dests[i]["Name"] < dests[j]["Name"] })
				spaceDocs = append(spaceDocs, sdSpaceDoc{SpaceID: sp.GUID, SpaceName: sp.Name, Destinations: dests})
			}
			if len(spaceDocs) == 0 {
				continue
			}
			docs = append(docs, sdOrgDoc{OrgID: org.GUID, OrgName: org.Name, Spaces: spaceDocs})
		}
	}

	saveDestCache(cmd, creds)
	return docs, nil
}

// ── rendering ────────────────────────────────────────────────────────────────

// Single-org results marshal as a single object (matching the pre---output
// shape); more than one — or zero — marshal as a list, same convention as
// subaccount-destinations.

func renderSpaceDestinationsJSON(w io.Writer, docs []sdOrgDoc, fieldIDs []string, sortKeys []destSortKey) error {
	if len(fieldIDs) > 0 || len(sortKeys) > 0 {
		flatRows := flattenSdRows(docs)
		if len(sortKeys) > 0 {
			destSortRowsBy(flatRows, sortKeys)
		}
		out, err := json.MarshalIndent(flatRowsDoc{Rows: destRowMaps(flatRows, fieldIDs, sdLeadingCols)}, "", "  ")
		if err != nil {
			return fmt.Errorf("encoding JSON: %w", err)
		}
		fmt.Fprintln(w, string(out))
		return nil
	}
	var out []byte
	var err error
	if len(docs) == 1 {
		out, err = json.MarshalIndent(docs[0], "", "  ")
	} else {
		out, err = json.MarshalIndent(docs, "", "  ")
	}
	if err != nil {
		return fmt.Errorf("encoding JSON: %w", err)
	}
	fmt.Fprintln(w, string(out))
	return nil
}

func renderSpaceDestinationsToon(w io.Writer, docs []sdOrgDoc, fieldIDs []string, sortKeys []destSortKey) error {
	if len(fieldIDs) > 0 || len(sortKeys) > 0 {
		flatRows := flattenSdRows(docs)
		if len(sortKeys) > 0 {
			destSortRowsBy(flatRows, sortKeys)
		}
		out, err := toonenc.Marshal(flatRowsDoc{Rows: destRowMaps(flatRows, fieldIDs, sdLeadingCols)}, toonenc.WithIndent(2))
		if err != nil {
			return fmt.Errorf("encoding TOON: %w", err)
		}
		fmt.Fprintln(w, string(out))
		return nil
	}
	var out []byte
	var err error
	if len(docs) == 1 {
		out, err = toonenc.Marshal(docs[0], toonenc.WithIndent(2))
	} else {
		out, err = toonenc.Marshal(struct {
			Orgs []sdOrgDoc `json:"orgs" toon:"orgs"`
		}{docs}, toonenc.WithIndent(2))
	}
	if err != nil {
		return fmt.Errorf("encoding TOON: %w", err)
	}
	fmt.Fprintln(w, string(out))
	return nil
}

func renderSpaceDestinationsCSV(w io.Writer, docs []sdOrgDoc, fieldIDs []string, sortKeys []destSortKey) error {
	flatRows := flattenSdRows(docs)
	if len(sortKeys) > 0 {
		destSortRowsBy(flatRows, sortKeys)
	}
	cw := csv.NewWriter(w)
	defer cw.Flush()
	return destWriteCSV(cw, flatRows, fieldIDs, sdLeadingCols)
}

// ── command ───────────────────────────────────────────────────────────────────

var spaceDestinationsCmd = &cobra.Command{
	Use:   "space-destinations",
	Short: "List instance-level destinations across destination service instances in CF spaces",
	Long: `Retrieves all instance-level destinations from every destination service instance
found in the given CF space (--space GUID).

If --space is omitted, every accessible space (across every org in scope) is
scanned instead — destinations are then grouped as
org_id/org_name -> spaces[space_id/space_name] -> destinations, each
destination carrying its own "destination_service_id"/
"destination_service_name" (the destination service instance it was found
in). Orgs/spaces with no matching destinations are left out of this
all-spaces result entirely; with --space given, that one target space is
always included, even if empty. With more than one org in the result,
--format json/toon returns a list of per-org results instead of a single
object.

Every destination property is included exactly as the destination service
API responds — nothing is redacted, including sensitive fields such as
Password, ClientSecret, and ProxyPassword; this is sensitive output, see
README.

Use --include/--exclude to narrow results: each accepts a comma-separated
list of keywords, matched against any destination property key or value —
as a glob pattern (e.g. API*PP) if a keyword contains * ? [, otherwise as a
case-insensitive substring.

--format csv gives a flat CSV, one row per destination: org_id, org_name,
space_id, and space_name lead, followed by every property key that appears
anywhere in the result set (sorted alphabetically, and already including
destination_service_id/destination_service_name — see below), so the
column set reflects whatever destinations were actually returned.

Use --fields <column,...> (comma-separated, in the order given — org_id,
org_name, space_id, space_name, or any destination property name, exactly
as the API casing has it, e.g. "Name" not "name") to limit output to just
those columns instead — switches json/toon to that same flat
{"rows": [...]} view too. Omit it to get every column (csv) or the normal
nested json/toon structure below.

Use --sort-by <column,...> to sort rows by one or more of those same
columns (any name accepted, matched the same way), each optionally
suffixed with ":desc" for descending (":asc" is the default) — also
switches json/toon to the flat view. Omit it to keep the default order:
sorted by Name within each space.

--no-key-option <s/skip|c/create|t/transient|a/abort> governs what happens
when a destination service instance is found with no service key: skip
warns and moves on; create automatically creates a 'bo-dest-key' service key
on the instance and keeps it; transient does the same but deletes that key
again once it's no longer needed for this run; abort stops the whole
command the first time it happens. Omit this flag to be asked
s/skip/c/create/t/transient/a/abort interactively each time such an
instance is found instead, defaulting to skip on a blank answer — unless -y
is also given, in which case skip is used directly with no prompt.

Use --output/-o to write the result to a file instead of stdout. If --format
is not explicitly given, it is auto-detected from --output's extension:
.json selects json, .csv selects csv, .toon selects toon — otherwise the
default toon applies. Pass --format explicitly to override the extension.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		spaceGUID, _ := cmd.Flags().GetString("space")
		regionsFlag, _ := cmd.Flags().GetString("regions")
		format, _ := cmd.Flags().GetString("format")
		includePattern, _ := cmd.Flags().GetString("include")
		excludePattern, _ := cmd.Flags().GetString("exclude")
		skipConfirm, _ := cmd.Flags().GetBool("yes")
		keyOptionFlag, _ := cmd.Flags().GetString("no-key-option")
		keyOption, err := normalizeDestKeyOption(keyOptionFlag)
		if err != nil {
			return err
		}
		if !cmd.Flags().Changed("no-key-option") && !skipConfirm {
			keyOption = destKeyOptionPrompt
		}
		outputFile, _ := cmd.Flags().GetString("output")
		if !cmd.Flags().Changed("format") {
			if auto := autoFormatFromOutputExt(outputFile, map[string]string{"json": ".json", "csv": ".csv", "toon": ".toon"}); auto != "" {
				format = auto
			}
		}
		fieldsFlag, _ := cmd.Flags().GetString("fields")
		fieldIDs := destParseFields(fieldsFlag)
		sortByFlag, _ := cmd.Flags().GetString("sort-by")
		sortKeys, err := destParseSortBy(sortByFlag, "--sort-by")
		if err != nil {
			return err
		}

		creds, err := store.Load()
		if err != nil {
			return fmt.Errorf("not logged in — run: bo login --regions <region>")
		}
		apiURLs := activeAPIURLs(creds, regionsFlag)
		if len(apiURLs) == 0 {
			return fmt.Errorf("no regions configured — run: bo login --regions <region1,region2>")
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		var docs []sdOrgDoc
		if spaceGUID != "" {
			orgGUID, orgName, orgErr := resolveSpaceOrgInfo(ctx, creds, apiURLs, spaceGUID)
			if orgErr != nil {
				return orgErr
			}
			spaceName, clients, resolveErr := resolveSpaceDestClients(ctx, cmd, spaceGUID, creds, apiURLs, keyOption)
			if resolveErr != nil {
				return resolveErr
			}

			var dests []map[string]string
			for _, c := range clients {
				rawDests, fetchErr := destination.ListInstanceDestinationsFull(ctx, c.URI, c.Token)
				if fetchErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] listing instance destinations: %v\n", c.InstanceName, fetchErr)
					continue
				}
				for _, raw := range rawDests {
					if includePattern != "" && !sdMatchesAnyKeyword(includePattern, raw) {
						continue
					}
					if excludePattern != "" && sdMatchesAnyKeyword(excludePattern, raw) {
						continue
					}
					dests = append(dests, sdTagDestInstance(raw, c.InstanceGUID, c.InstanceName))
				}
			}
			cleanupDestClients(clients...)
			sort.Slice(dests, func(i, j int) bool { return dests[i]["Name"] < dests[j]["Name"] })

			docs = []sdOrgDoc{{
				OrgID:   orgGUID,
				OrgName: orgName,
				Spaces: []sdSpaceDoc{{
					SpaceID:      spaceGUID,
					SpaceName:    spaceName,
					Destinations: dests,
				}},
			}}
		} else {
			docs, err = fetchAllSpaceDestinations(ctx, cmd, creds, apiURLs, includePattern, excludePattern, keyOption)
			if err != nil {
				return err
			}
		}

		out, closeOut, err := resolveOutputWriter(outputFile)
		if err != nil {
			return err
		}
		defer closeOut()

		switch strings.ToLower(format) {
		case "json":
			return renderSpaceDestinationsJSON(out, docs, fieldIDs, sortKeys)
		case "csv":
			return renderSpaceDestinationsCSV(out, docs, fieldIDs, sortKeys)
		default:
			return renderSpaceDestinationsToon(out, docs, fieldIDs, sortKeys)
		}
	},
}

// ── create-space-destinations ─────────────────────────────────────────────────

var createSpaceDestinationsCmd = &cobra.Command{
	Use:   "create-space-destinations",
	Short: "Create instance-level destinations in all destination service instances of a CF space",
	Long: `Reads destinations from a JSON file (--destinations) and POSTs them to every
destination service instance found in the given CF space (--space GUID).

The JSON file must be an array of destination objects, e.g.:
  [{"Name":"my-dest","Type":"HTTP","URL":"https://...","Authentication":"NoAuthentication"}]

A destination service instance found with no service key prompts
interactively (s/skip, c/create, t/transient, or a/abort — see
'bo space-destinations --help').`,
	RunE: func(cmd *cobra.Command, args []string) error {
		spaceGUID, _ := cmd.Flags().GetString("space")
		destFile, _ := cmd.Flags().GetString("destinations")
		regionsFlag, _ := cmd.Flags().GetString("regions")

		rawBody, names, err := loadDestinationsJSON(destFile)
		if err != nil {
			return err
		}

		creds, err := store.Load()
		if err != nil {
			return fmt.Errorf("not logged in — run: bo login --regions <region>")
		}
		apiURLs := activeAPIURLs(creds, regionsFlag)
		if len(apiURLs) == 0 {
			return fmt.Errorf("no regions configured — run: bo login --regions <region1,region2>")
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		spaceName, clients, err := resolveSpaceDestClients(ctx, cmd, spaceGUID, creds, apiURLs, destKeyOptionPrompt)
		if err != nil {
			return err
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Creating destinations in space %s (%s)...\n", spaceName, spaceGUID)
		for _, c := range clients {
			fmt.Fprintf(cmd.OutOrStdout(), "  → %s (%s)\n", c.InstanceName, c.InstanceGUID)
			items, postErr := destination.CreateInstanceDestinations(ctx, c.URI, c.Token, rawBody)
			if postErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "    ERROR: %v\n", postErr)
				continue
			}
			printActionResults(cmd, "created", names, items)
		}
		cleanupDestClients(clients...)
		return nil
	},
}

// ── update-space-destinations ─────────────────────────────────────────────────

var updateSpaceDestinationsCmd = &cobra.Command{
	Use:   "update-space-destinations",
	Short: "Update instance-level destinations in all destination service instances of a CF space",
	Long: `Reads destinations from a JSON file (--destinations) and PUTs them to every
destination service instance found in the given CF space (--space GUID).

The JSON file must be an array of destination objects. Existing destinations with
the same Name are overwritten; others are left unchanged.

A destination service instance found with no service key prompts
interactively (s/skip, c/create, t/transient, or a/abort — see
'bo space-destinations --help').`,
	RunE: func(cmd *cobra.Command, args []string) error {
		spaceGUID, _ := cmd.Flags().GetString("space")
		destFile, _ := cmd.Flags().GetString("destinations")
		regionsFlag, _ := cmd.Flags().GetString("regions")

		rawBody, names, err := loadDestinationsJSON(destFile)
		if err != nil {
			return err
		}

		creds, err := store.Load()
		if err != nil {
			return fmt.Errorf("not logged in — run: bo login --regions <region>")
		}
		apiURLs := activeAPIURLs(creds, regionsFlag)
		if len(apiURLs) == 0 {
			return fmt.Errorf("no regions configured — run: bo login --regions <region1,region2>")
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		spaceName, clients, err := resolveSpaceDestClients(ctx, cmd, spaceGUID, creds, apiURLs, destKeyOptionPrompt)
		if err != nil {
			return err
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Updating destinations in space %s (%s)...\n", spaceName, spaceGUID)
		for _, c := range clients {
			fmt.Fprintf(cmd.OutOrStdout(), "  → %s (%s)\n", c.InstanceName, c.InstanceGUID)
			items, putErr := destination.UpdateInstanceDestinations(ctx, c.URI, c.Token, rawBody)
			if putErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "    ERROR: %v\n", putErr)
				continue
			}
			printActionResults(cmd, "updated", names, items)
		}
		cleanupDestClients(clients...)
		return nil
	},
}

// ── delete-space-destinations ─────────────────────────────────────────────────

var deleteSpaceDestinationsCmd = &cobra.Command{
	Use:   "delete-space-destinations",
	Short: "Delete named destinations from all destination service instances of a CF space",
	Long: `Reads destination names from a JSON file (--destinations) and deletes each
matching destination from every destination service instance found in the given
CF space (--space GUID).

The JSON file must be an array of destination objects; only the "Name" field is
used. Non-existent destinations are silently ignored (idempotent).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		spaceGUID, _ := cmd.Flags().GetString("space")
		destFile, _ := cmd.Flags().GetString("destinations")
		regionsFlag, _ := cmd.Flags().GetString("regions")

		_, names, err := loadDestinationsJSON(destFile)
		if err != nil {
			return err
		}
		if len(names) == 0 {
			return fmt.Errorf("no destination names found in %s (each entry must have a \"Name\" field)", destFile)
		}

		creds, err := store.Load()
		if err != nil {
			return fmt.Errorf("not logged in — run: bo login --regions <region>")
		}
		apiURLs := activeAPIURLs(creds, regionsFlag)
		if len(apiURLs) == 0 {
			return fmt.Errorf("no regions configured — run: bo login --regions <region1,region2>")
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		spaceName, clients, err := resolveSpaceDestClients(ctx, cmd, spaceGUID, creds, apiURLs, destKeyOptionPrompt)
		if err != nil {
			return err
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Deleting %d destination(s) from space %s (%s)...\n",
			len(names), spaceName, spaceGUID)
		for _, c := range clients {
			fmt.Fprintf(cmd.OutOrStdout(), "  → %s (%s)\n", c.InstanceName, c.InstanceGUID)
			for _, name := range names {
				deleted, delErr := destination.DeleteInstanceDestination(ctx, c.URI, c.Token, name)
				if delErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "    ERROR: %s — %v\n", name, delErr)
				} else if deleted {
					fmt.Fprintf(cmd.OutOrStdout(), "    deleted: %s\n", name)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "    not found: %s\n", name)
				}
			}
		}
		cleanupDestClients(clients...)
		return nil
	},
}

// ── helpers ───────────────────────────────────────────────────────────────────

// activeAPIURLs resolves the list of CF API URLs from the regions flag or the
// stored active URL list.
func activeAPIURLs(creds *store.Credentials, regionsFlag string) []string {
	if regionsFlag != "" {
		var urls []string
		for _, r := range splitCSV(regionsFlag) {
			urls = append(urls, store.RegionToAPIURL(r))
		}
		return urls
	}
	return creds.ActiveAPIURLs
}

// ── registration ──────────────────────────────────────────────────────────────

func init() {
	// space-destinations
	spaceDestinationsCmd.Flags().String("space", "", "CF space GUID; if omitted, every accessible space is scanned instead")
	spaceDestinationsCmd.Flags().String("regions", "", "Comma-separated CF regions to search (default: last login regions)")
	spaceDestinationsCmd.Flags().String("format", "toon", "Output format: toon (default), json, or csv; auto-detected from --output's extension (.json, .csv, or .toon) when --format is not explicitly given")
	spaceDestinationsCmd.Flags().String("include", "", "Only include destinations where any property contains any of these comma-separated keywords (substring, or glob if a keyword has * ? [)")
	spaceDestinationsCmd.Flags().String("exclude", "", "Exclude destinations where any property contains any of these comma-separated keywords (substring, or glob if a keyword has * ? [)")
	spaceDestinationsCmd.Flags().String("no-key-option", "", "What to do with a destination service instance found with no service key: s/skip (default), c/create (auto-create one, kept), t/transient (create, use, then delete it), or a/abort; omit to be asked interactively per instance (unless -y), s/skip if -y is given")
	spaceDestinationsCmd.Flags().BoolP("yes", "y", false, "Don't ask interactively about a missing destination service key — default to s/skip instead, unless --no-key-option is also given")
	spaceDestinationsCmd.Flags().StringP("output", "o", "", "Write output to this file instead of stdout")
	spaceDestinationsCmd.Flags().String("fields", "", "Comma-separated column names to limit output to, in the order given (org_id, org_name, space_id, space_name, or any destination property name exactly as the API casing has it); switches json/toon to a flat {rows:[...]} view too")
	spaceDestinationsCmd.Flags().String("sort-by", "", "Comma-separated column names to sort by, each optionally suffixed with \":desc\" (default \":asc\"); switches json/toon to a flat {rows:[...]} view too")
	spaceDestinationsCmd.GroupID = "destination"
	rootCmd.AddCommand(spaceDestinationsCmd)

	// create-space-destinations
	createSpaceDestinationsCmd.Flags().String("space", "", "CF space GUID (required)")
	createSpaceDestinationsCmd.Flags().String("destinations", "", "Path to JSON file containing destinations array (required)")
	createSpaceDestinationsCmd.Flags().String("regions", "", "Comma-separated CF regions to search (default: last login regions)")
	_ = createSpaceDestinationsCmd.MarkFlagRequired("space")
	_ = createSpaceDestinationsCmd.MarkFlagRequired("destinations")
	createSpaceDestinationsCmd.GroupID = "destination"
	rootCmd.AddCommand(createSpaceDestinationsCmd)

	// update-space-destinations
	updateSpaceDestinationsCmd.Flags().String("space", "", "CF space GUID (required)")
	updateSpaceDestinationsCmd.Flags().String("destinations", "", "Path to JSON file containing destinations array (required)")
	updateSpaceDestinationsCmd.Flags().String("regions", "", "Comma-separated CF regions to search (default: last login regions)")
	_ = updateSpaceDestinationsCmd.MarkFlagRequired("space")
	_ = updateSpaceDestinationsCmd.MarkFlagRequired("destinations")
	updateSpaceDestinationsCmd.GroupID = "destination"
	rootCmd.AddCommand(updateSpaceDestinationsCmd)

	// delete-space-destinations
	deleteSpaceDestinationsCmd.Flags().String("space", "", "CF space GUID (required)")
	deleteSpaceDestinationsCmd.Flags().String("destinations", "", "Path to JSON file — only \"Name\" field is used (required)")
	deleteSpaceDestinationsCmd.Flags().String("regions", "", "Comma-separated CF regions to search (default: last login regions)")
	_ = deleteSpaceDestinationsCmd.MarkFlagRequired("space")
	_ = deleteSpaceDestinationsCmd.MarkFlagRequired("destinations")
	deleteSpaceDestinationsCmd.GroupID = "destination"
	rootCmd.AddCommand(deleteSpaceDestinationsCmd)
}
