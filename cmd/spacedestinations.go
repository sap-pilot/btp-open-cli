package cmd

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	toonenc "github.com/toon-format/toon-go"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/destination"
	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

// ── output types ──────────────────────────────────────────────────────────────

type sdDestSvcInstance struct {
	ID           string              `json:"destination_service_id"   toon:"destination_service_id"`
	Name         string              `json:"destination_service_name" toon:"destination_service_name"`
	Destinations []map[string]string `json:"destinations"             toon:"destinations"`
}

type sdSpaceDoc struct {
	SpaceID   string              `json:"space_id"                      toon:"space_id"`
	SpaceName string              `json:"space_name"                    toon:"space_name"`
	Instances []sdDestSvcInstance `json:"destination_service_instances" toon:"destination_service_instances"`
}

// sdMinimalDest returns a copy of the destination map containing only the
// Name, URL, and sap-client properties.
func sdMinimalDest(raw map[string]string) map[string]string {
	m := make(map[string]string, 3)
	for _, k := range []string{"Name", "URL", "sap-client"} {
		if v, ok := raw[k]; ok {
			m[k] = v
		}
	}
	return m
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

// ── shared setup ──────────────────────────────────────────────────────────────

// sdDestClient is a ready-to-use destination service client (token refreshed).
type sdDestClient struct {
	InstanceGUID string
	InstanceName string
	URI          string
	Token        string
}

// resolveSpaceDestClients finds the target space across regions, discovers all
// destination service instances in it, loads/fetches their credentials (with
// an interactive prompt when no service key exists), refreshes tokens and
// persists the cache. It returns the space name and one client per instance.
func resolveSpaceDestClients(
	ctx context.Context,
	cmd *cobra.Command,
	spaceGUID string,
	creds *store.Credentials,
	apiURLs []string,
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

	destPlan, planErr := cfClient.FindServicePlan(ctx, "destination", "lite")
	if planErr != nil {
		return spaceName, nil, fmt.Errorf("looking up destination service plan: %w", planErr)
	}
	if destPlan == nil {
		return spaceName, nil, fmt.Errorf("destination service plan 'lite' not found in region %s", store.APIURLToRegion(cfClient.BaseURL()))
	}

	// ── find destination service instances in the space ───────────────────────
	instances, instErr := cfClient.ListServiceInstancesInSpace(ctx, spaceGUID, destPlan.GUID)
	if instErr != nil {
		return spaceName, nil, fmt.Errorf("listing destination instances in space: %w", instErr)
	}
	if len(instances) == 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: no destination service instances found in space %s (%s)\n", spaceName, spaceGUID)
		return spaceName, nil, nil
	}

	// ── ensure cache map exists ───────────────────────────────────────────────
	if creds.SpaceDestServices == nil {
		creds.SpaceDestServices = make(map[string]map[string]*store.DestInstanceCache)
	}
	if creds.SpaceDestServices[spaceGUID] == nil {
		creds.SpaceDestServices[spaceGUID] = make(map[string]*store.DestInstanceCache)
	}
	spaceCache := creds.SpaceDestServices[spaceGUID]

	// ── per-instance: refresh token if needed, fetching key from CF each time ─
	// Client ID and client secret are intentionally NOT cached locally.
	// They are fetched from CF on demand whenever a new token is needed and
	// discarded immediately — only the token (+ tokenURL + URI) is persisted.
	for _, inst := range instances {
		cached := spaceCache[inst.GUID]
		needToken := cached == nil || cached.AccessToken == "" ||
			time.Now().Add(60*time.Second).After(cached.TokenExpiry)

		if needToken {
			// Always fetch the service key from CF to get credentials.
			key, keyErr := cfClient.FindAnyServiceCredentialBinding(ctx, inst.GUID)
			if keyErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: finding service key for %q: %v\n", inst.Name, keyErr)
				continue
			}
			if key == nil {
				// No key — warn and offer an interactive prompt to create one.
				fmt.Fprintf(cmd.ErrOrStderr(),
					"\nWARNING: No service key found for destination service instance %q (%s)\n"+
						"  Create one manually, e.g. via CF CLI:\n"+
						"    cf create-service-key %s bo-dest-key\n"+
						"  Then press Enter to retry, or Ctrl-C to skip this instance.\n",
					inst.Name, inst.GUID, inst.Name)
				_, ok := readLine(ctx)
				if !ok {
					continue
				}
				key, keyErr = cfClient.FindAnyServiceCredentialBinding(ctx, inst.GUID)
				if keyErr != nil || key == nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: still no service key for %q — skipping\n", inst.Name)
					continue
				}
			}

			details, detailErr := cfClient.GetServiceCredentialDetails(ctx, key.GUID)
			if detailErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: fetching key details for %q: %v\n", inst.Name, detailErr)
				continue
			}
			clientID, _ := details["clientid"].(string)
			clientSecret, _ := details["clientsecret"].(string)
			tokenURL, _ := details["url"].(string)
			uri, _ := details["uri"].(string)
			if clientID == "" || clientSecret == "" || tokenURL == "" || uri == "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: incomplete credentials in service key for %q\n", inst.Name)
				continue
			}

			// Get a new token — credentials are discarded after this call.
			newToken, expiry, tokErr := destination.GetAccessToken(ctx, tokenURL, clientID, clientSecret)
			if tokErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: token for destination instance %q: %v\n", inst.Name, tokErr)
				continue
			}

			// Persist only: instanceName, tokenURL, URI, accessToken, tokenExpiry.
			if cached == nil {
				cached = &store.DestInstanceCache{}
				spaceCache[inst.GUID] = cached
			}
			cached.InstanceName = inst.Name
			cached.TokenURL = tokenURL
			cached.URI = uri
			cached.AccessToken = newToken
			cached.TokenExpiry = expiry
			// clientID and clientSecret intentionally NOT stored.
		}

		if cached == nil || cached.URI == "" {
			continue // token fetch must have failed above
		}

		clients = append(clients, sdDestClient{
			InstanceGUID: inst.GUID,
			InstanceName: cached.InstanceName,
			URI:          cached.URI,
			Token:        cached.AccessToken,
		})
	}

	// ── persist updated cache ─────────────────────────────────────────────────
	creds.SpaceDestServices[spaceGUID] = spaceCache
	if saveErr := store.Save(creds); saveErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: saving destination credentials: %v\n", saveErr)
	}

	return spaceName, clients, nil
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

var spaceDestinationsCmd = &cobra.Command{
	Use:   "space-destinations",
	Short: "List instance-level destinations across all destination service instances in a CF space",
	Long: `Retrieves all instance-level destinations from every destination service instance
found in the given CF space (identified by --space GUID).

Without --full: only Name, URL, and sap-client are included per destination.
With --full: all destination properties are returned as a flat object exactly as
the destination service API responds — nothing is redacted, including sensitive
fields such as Password, ClientSecret, and ProxyPassword.

Use --filter to narrow results by substring or glob pattern matched against
any destination property (e.g. MDG, API*PP).

Use --format csv (without --full) to get a flat CSV with columns:
  space_name,destination_service_name,destination_name,destination_url,destination_sap_client`,
	RunE: func(cmd *cobra.Command, args []string) error {
		spaceGUID, _ := cmd.Flags().GetString("space")
		regionsFlag, _ := cmd.Flags().GetString("regions")
		format, _ := cmd.Flags().GetString("format")
		full, _ := cmd.Flags().GetBool("full")
		filter, _ := cmd.Flags().GetString("filter")

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

		spaceName, clients, err := resolveSpaceDestClients(ctx, cmd, spaceGUID, creds, apiURLs)
		if err != nil {
			return err
		}

		// Fetch instance destinations from each client.
		// For --full, include sensitive properties (password, clientSecret, etc.).
		fetchFn := destination.ListInstanceDestinations
		if full {
			fetchFn = destination.ListInstanceDestinationsFull
		}

		var instDocs []sdDestSvcInstance
		for _, c := range clients {
			rawDests, fetchErr := fetchFn(ctx, c.URI, c.Token)
			if fetchErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] listing instance destinations: %v\n", c.InstanceName, fetchErr)
				instDocs = append(instDocs, sdDestSvcInstance{ID: c.InstanceGUID, Name: c.InstanceName})
				continue
			}
			var dests []map[string]string
			for _, raw := range rawDests {
				// Apply --filter against the full property set (before any trimming).
				if !sdMatchesFilter(raw, filter) {
					continue
				}
				if full {
					dests = append(dests, raw)
				} else {
					dests = append(dests, sdMinimalDest(raw))
				}
			}
			sort.Slice(dests, func(i, j int) bool { return dests[i]["Name"] < dests[j]["Name"] })
			instDocs = append(instDocs, sdDestSvcInstance{
				ID:           c.InstanceGUID,
				Name:         c.InstanceName,
				Destinations: dests,
			})
		}

		doc := sdSpaceDoc{
			SpaceID:   spaceGUID,
			SpaceName: spaceName,
			Instances: instDocs,
		}

		switch strings.ToLower(format) {
		case "json":
			out, err := json.MarshalIndent(doc, "", "  ")
			if err != nil {
				return fmt.Errorf("encoding JSON: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(out))

		case "csv":
			if full {
				return fmt.Errorf("--format csv is not supported with --full; use --format json or toon instead")
			}
			w := csv.NewWriter(cmd.OutOrStdout())
			defer w.Flush()
			if err := w.Write([]string{
				"space_name", "destination_service_name",
				"destination_name", "destination_url", "destination_sap_client",
			}); err != nil {
				return err
			}
			for _, inst := range instDocs {
				for _, d := range inst.Destinations {
					if err := w.Write([]string{
						doc.SpaceName, inst.Name,
						d["Name"], d["URL"], d["sap-client"],
					}); err != nil {
						return err
					}
				}
			}

		default: // toon
			out, err := toonenc.Marshal(doc, toonenc.WithIndent(2))
			if err != nil {
				return fmt.Errorf("encoding TOON: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(out))
		}
		return nil
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

A warning and interactive prompt are shown when a destination service instance
has no service key; the user can create one and press Enter to retry.`,
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

		spaceName, clients, err := resolveSpaceDestClients(ctx, cmd, spaceGUID, creds, apiURLs)
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

A warning and interactive prompt are shown when a destination service instance
has no service key; the user can create one and press Enter to retry.`,
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

		spaceName, clients, err := resolveSpaceDestClients(ctx, cmd, spaceGUID, creds, apiURLs)
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

		spaceName, clients, err := resolveSpaceDestClients(ctx, cmd, spaceGUID, creds, apiURLs)
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
	spaceDestinationsCmd.Flags().String("space", "", "CF space GUID (required)")
	spaceDestinationsCmd.Flags().String("regions", "", "Comma-separated CF regions to search (default: last login regions)")
	spaceDestinationsCmd.Flags().String("format", "toon", "Output format: toon (default), json, or csv (csv only without --full)")
	spaceDestinationsCmd.Flags().Bool("full", false, "Return all destination properties as-is from the API, including sensitive fields such as Password and ClientSecret (default: Name, URL, sap-client only)")
	spaceDestinationsCmd.Flags().String("filter", "", "Case-insensitive substring or glob pattern (e.g. MDG or API*PP) matched against any destination property")
	_ = spaceDestinationsCmd.MarkFlagRequired("space")
	rootCmd.AddCommand(spaceDestinationsCmd)

	// create-space-destinations
	createSpaceDestinationsCmd.Flags().String("space", "", "CF space GUID (required)")
	createSpaceDestinationsCmd.Flags().String("destinations", "", "Path to JSON file containing destinations array (required)")
	createSpaceDestinationsCmd.Flags().String("regions", "", "Comma-separated CF regions to search (default: last login regions)")
	_ = createSpaceDestinationsCmd.MarkFlagRequired("space")
	_ = createSpaceDestinationsCmd.MarkFlagRequired("destinations")
	rootCmd.AddCommand(createSpaceDestinationsCmd)

	// update-space-destinations
	updateSpaceDestinationsCmd.Flags().String("space", "", "CF space GUID (required)")
	updateSpaceDestinationsCmd.Flags().String("destinations", "", "Path to JSON file containing destinations array (required)")
	updateSpaceDestinationsCmd.Flags().String("regions", "", "Comma-separated CF regions to search (default: last login regions)")
	_ = updateSpaceDestinationsCmd.MarkFlagRequired("space")
	_ = updateSpaceDestinationsCmd.MarkFlagRequired("destinations")
	rootCmd.AddCommand(updateSpaceDestinationsCmd)

	// delete-space-destinations
	deleteSpaceDestinationsCmd.Flags().String("space", "", "CF space GUID (required)")
	deleteSpaceDestinationsCmd.Flags().String("destinations", "", "Path to JSON file — only \"Name\" field is used (required)")
	deleteSpaceDestinationsCmd.Flags().String("regions", "", "Comma-separated CF regions to search (default: last login regions)")
	_ = deleteSpaceDestinationsCmd.MarkFlagRequired("space")
	_ = deleteSpaceDestinationsCmd.MarkFlagRequired("destinations")
	rootCmd.AddCommand(deleteSpaceDestinationsCmd)
}
