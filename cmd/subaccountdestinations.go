package cmd

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
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

type sadOrgDoc struct {
	OrgID        string              `json:"org_id"       toon:"org_id"`
	OrgName      string              `json:"org_name"     toon:"org_name"`
	Destinations []map[string]string `json:"destinations" toon:"destinations"`
}

// ── shared setup ──────────────────────────────────────────────────────────────

// resolveOrgDestClient finds the target org across all active regions by GUID
// (exact) or name (case-insensitive substring), then scans all spaces in that
// org for any destination/lite service instance that has a service key. The
// first usable instance is returned as an sdDestClient with a valid token.
//
// Token caching follows the same rules as resolveSpaceDestClients:
//   - Only the access token, tokenURL and URI are persisted in SpaceDestServices.
//   - Service key credentials (clientId, clientSecret) are fetched from CF on
//     demand and discarded immediately — they are never written to disk.
//
// If no instance or service key is found and noPrompt is false, the user is
// prompted to create the required resource and the search is retried once.
func resolveOrgDestClient(
	ctx context.Context,
	cmd *cobra.Command,
	orgFlag string,
	creds *store.Credentials,
	apiURLs []string,
	noPrompt bool,
) (orgGUID, orgName string, client sdDestClient, err error) {

	// ── step 1: find the org across regions ──────────────────────────────────
	type foundOrg struct {
		guid       string
		name       string
		regionURL  string
	}
	var found *foundOrg

	fl := strings.ToLower(orgFlag)
	for _, apiURL := range apiURLs {
		tok, ok := creds.Tokens[apiURL]
		if !ok {
			continue
		}
		c := cf.NewClient(apiURL, tok.AccessToken)
		c.SetTokenRefresher(makeTokenRefresher(apiURL, tok.AccessToken))
		orgs, listErr := c.ListOrganizations(ctx)
		if listErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] listing orgs: %v\n", store.APIURLToRegion(apiURL), listErr)
			continue
		}
		for _, org := range orgs {
			if strings.EqualFold(org.GUID, orgFlag) || strings.Contains(strings.ToLower(org.Name), fl) {
				found = &foundOrg{guid: org.GUID, name: org.Name, regionURL: apiURL}
				break
			}
		}
		if found != nil {
			break
		}
	}
	if found == nil {
		return "", "", sdDestClient{}, fmt.Errorf("org %q not found in any accessible region", orgFlag)
	}

	// ── step 2: build CF client for the org's region ─────────────────────────
	tok := creds.Tokens[found.regionURL]
	cfClient := cf.NewClient(found.regionURL, tok.AccessToken)
	cfClient.SetTokenRefresher(makeTokenRefresher(found.regionURL, tok.AccessToken))

	// ── step 3: look up destination/lite plan ────────────────────────────────
	destPlan, planErr := cfClient.FindServicePlan(ctx, "destination", "lite")
	if planErr != nil {
		return "", "", sdDestClient{}, fmt.Errorf("looking up destination service plan: %w", planErr)
	}
	if destPlan == nil {
		return "", "", sdDestClient{},
			fmt.Errorf("destination service plan 'lite' not found in region %s",
				store.APIURLToRegion(found.regionURL))
	}

	// ── step 4: scan all spaces in the org for a usable instance ─────────────
	if creds.SpaceDestServices == nil {
		creds.SpaceDestServices = make(map[string]map[string]*store.DestInstanceCache)
	}

	tryFindClient := func() (sdDestClient, bool) {
		spaces, spacesErr := cfClient.ListOrganizationSpaces(ctx, found.guid)
		if spacesErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: listing spaces in org %q: %v\n", found.name, spacesErr)
			return sdDestClient{}, false
		}

		for _, space := range spaces {
			instances, instErr := cfClient.ListServiceInstancesInSpace(ctx, space.GUID, destPlan.GUID)
			if instErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: listing destination instances in space %q: %v\n", space.Name, instErr)
				continue
			}

			if creds.SpaceDestServices[space.GUID] == nil {
				creds.SpaceDestServices[space.GUID] = make(map[string]*store.DestInstanceCache)
			}
			spaceCache := creds.SpaceDestServices[space.GUID]

			for _, inst := range instances {
				cached := spaceCache[inst.GUID]
				needToken := cached == nil || cached.AccessToken == "" ||
					time.Now().Add(60*time.Second).After(cached.TokenExpiry)

				if needToken {
					// Fetch service key from CF on demand — credentials are NOT cached.
					key, keyErr := cfClient.FindAnyServiceCredentialBinding(ctx, inst.GUID)
					if keyErr != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: finding service key for %q: %v\n", inst.Name, keyErr)
						continue
					}
					if key == nil {
						if noPrompt {
							fmt.Fprintf(cmd.ErrOrStderr(),
								"warning: no service key for destination instance %q in space %q — skipping\n",
								inst.Name, space.Name)
						} else {
							fmt.Fprintf(cmd.ErrOrStderr(),
								"\nWARNING: No service key found for destination instance %q (space: %s)\n"+
									"  Create one manually, e.g. via CF CLI:\n"+
									"    cf create-service-key %s bo-dest-key\n"+
									"  Then press Enter to retry, or Ctrl-C to skip.\n",
								inst.Name, space.Name, inst.Name)
							if _, ok := readLine(ctx); ok {
								key, keyErr = cfClient.FindAnyServiceCredentialBinding(ctx, inst.GUID)
								if keyErr != nil || key == nil {
									fmt.Fprintf(cmd.ErrOrStderr(), "warning: still no service key for %q — skipping\n", inst.Name)
									continue
								}
							} else {
								continue
							}
						}
						if key == nil {
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

					// Obtain token — credentials are discarded immediately after.
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
					continue
				}
				return sdDestClient{
					InstanceGUID: inst.GUID,
					InstanceName: cached.InstanceName,
					URI:          cached.URI,
					Token:        cached.AccessToken,
				}, true
			}
		}
		return sdDestClient{}, false
	}

	if c, ok := tryFindClient(); ok {
		if saveErr := store.Save(creds); saveErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: saving destination credentials: %v\n", saveErr)
		}
		return found.guid, found.name, c, nil
	}

	// No instance found — prompt or fail.
	if noPrompt {
		return "", "", sdDestClient{},
			fmt.Errorf("no destination service instance found in org %q — create one and run again", found.name)
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"\nWARNING: No destination service instance found in org %q\n"+
			"  Create one in any space, e.g.:\n"+
			"    cf create-service destination lite <instance-name>\n"+
			"    cf create-service-key <instance-name> bo-dest-key\n"+
			"  Then press Enter to retry, or Ctrl-C to abort.\n",
		found.name)
	if _, ok := readLine(ctx); !ok {
		return "", "", sdDestClient{}, fmt.Errorf("no destination service instance available in org %q", found.name)
	}

	// Retry once.
	if c, ok := tryFindClient(); ok {
		if saveErr := store.Save(creds); saveErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: saving destination credentials: %v\n", saveErr)
		}
		return found.guid, found.name, c, nil
	}

	if saveErr := store.Save(creds); saveErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: saving destination credentials: %v\n", saveErr)
	}
	return "", "", sdDestClient{},
		fmt.Errorf("no destination service instance found in org %q after retry — aborting", found.name)
}

// ── subaccount-destinations ───────────────────────────────────────────────────

var subaccountDestinationsCmd = &cobra.Command{
	Use:   "subaccount-destinations",
	Short: "List subaccount-level destinations via the destination service",
	Long: `Retrieves all subaccount-level destinations from the destination service using
any destination service instance found in the target org (--org GUID or name).

Without --full: only Name, URL, and sap-client are included per destination.
With --full: all non-sensitive destination properties are returned as a flat object.

Use --filter to narrow results by substring or glob pattern matched against
any destination property (e.g. MDG, API*PP).

Use --format csv (without --full) to get a flat CSV with columns:
  org_name,destination_name,destination_url,destination_sap_client

The access token is cached locally and reused until it expires or 'bo logoff' is run.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		orgFlag, _ := cmd.Flags().GetString("org")
		regionsFlag, _ := cmd.Flags().GetString("regions")
		format, _ := cmd.Flags().GetString("format")
		full, _ := cmd.Flags().GetBool("full")
		filter, _ := cmd.Flags().GetString("filter")
		noPrompt, _ := cmd.Flags().GetBool("no-prompt")

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

		orgGUID, orgName, destClient, err := resolveOrgDestClient(ctx, cmd, orgFlag, creds, apiURLs, noPrompt)
		if err != nil {
			return err
		}

		// Fetch subaccount destinations.
		fetchFn := destination.ListSubaccountDestinations
		if full {
			fetchFn = destination.ListSubaccountDestinationsFull
		}

		rawDests, fetchErr := fetchFn(ctx, destClient.URI, destClient.Token)
		if fetchErr != nil {
			return fmt.Errorf("listing subaccount destinations: %w", fetchErr)
		}

		var dests []map[string]string
		for _, raw := range rawDests {
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

		doc := sadOrgDoc{
			OrgID:        orgGUID,
			OrgName:      orgName,
			Destinations: dests,
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
				"org_name", "destination_name", "destination_url", "destination_sap_client",
			}); err != nil {
				return err
			}
			for _, d := range doc.Destinations {
				if err := w.Write([]string{
					doc.OrgName, d["Name"], d["URL"], d["sap-client"],
				}); err != nil {
					return err
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

// ── create-subaccount-destinations ───────────────────────────────────────────

var createSubaccountDestinationsCmd = &cobra.Command{
	Use:   "create-subaccount-destinations",
	Short: "Create subaccount-level destinations via the destination service",
	Long: `Reads destinations from a JSON file (--destinations) and POSTs them to the
subaccount-level destination endpoint using a destination service instance found
in the target org (--org GUID or name).

The JSON file must be an array of destination objects, e.g.:
  [{"Name":"my-dest","Type":"HTTP","URL":"https://...","Authentication":"NoAuthentication"}]

The access token is cached locally and reused until it expires or 'bo logoff' is run.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		orgFlag, _ := cmd.Flags().GetString("org")
		destFile, _ := cmd.Flags().GetString("destinations")
		regionsFlag, _ := cmd.Flags().GetString("regions")
		noPrompt, _ := cmd.Flags().GetBool("no-prompt")

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

		_, orgName, destClient, err := resolveOrgDestClient(ctx, cmd, orgFlag, creds, apiURLs, noPrompt)
		if err != nil {
			return err
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Creating subaccount destinations in org %s (via instance: %s)...\n",
			orgName, destClient.InstanceName)
		items, postErr := destination.CreateSubaccountDestinations(ctx, destClient.URI, destClient.Token, rawBody)
		if postErr != nil {
			return fmt.Errorf("creating subaccount destinations: %w", postErr)
		}
		printActionResults(cmd, "created", names, items)
		return nil
	},
}

// ── update-subaccount-destinations ───────────────────────────────────────────

var updateSubaccountDestinationsCmd = &cobra.Command{
	Use:   "update-subaccount-destinations",
	Short: "Update subaccount-level destinations via the destination service",
	Long: `Reads destinations from a JSON file (--destinations) and PUTs them to the
subaccount-level destination endpoint using a destination service instance found
in the target org (--org GUID or name).

Existing destinations with the same Name are overwritten; others are left unchanged.

The JSON file must be an array of destination objects.

The access token is cached locally and reused until it expires or 'bo logoff' is run.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		orgFlag, _ := cmd.Flags().GetString("org")
		destFile, _ := cmd.Flags().GetString("destinations")
		regionsFlag, _ := cmd.Flags().GetString("regions")
		noPrompt, _ := cmd.Flags().GetBool("no-prompt")

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

		_, orgName, destClient, err := resolveOrgDestClient(ctx, cmd, orgFlag, creds, apiURLs, noPrompt)
		if err != nil {
			return err
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Updating subaccount destinations in org %s (via instance: %s)...\n",
			orgName, destClient.InstanceName)
		items, putErr := destination.UpdateSubaccountDestinations(ctx, destClient.URI, destClient.Token, rawBody)
		if putErr != nil {
			return fmt.Errorf("updating subaccount destinations: %w", putErr)
		}
		printActionResults(cmd, "updated", names, items)
		return nil
	},
}

// ── delete-subaccount-destinations ───────────────────────────────────────────

var deleteSubaccountDestinationsCmd = &cobra.Command{
	Use:   "delete-subaccount-destinations",
	Short: "Delete subaccount-level destinations via the destination service",
	Long: `Reads destination names from a JSON file (--destinations) and deletes each
matching destination from the subaccount-level endpoint using a destination
service instance found in the target org (--org GUID or name).

The JSON file must be an array of destination objects; only the "Name" field is
used. Non-existent destinations are silently ignored (idempotent).

The access token is cached locally and reused until it expires or 'bo logoff' is run.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		orgFlag, _ := cmd.Flags().GetString("org")
		destFile, _ := cmd.Flags().GetString("destinations")
		regionsFlag, _ := cmd.Flags().GetString("regions")
		noPrompt, _ := cmd.Flags().GetBool("no-prompt")

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

		_, orgName, destClient, err := resolveOrgDestClient(ctx, cmd, orgFlag, creds, apiURLs, noPrompt)
		if err != nil {
			return err
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Deleting %d subaccount destination(s) in org %s (via instance: %s)...\n",
			len(names), orgName, destClient.InstanceName)
		for _, name := range names {
			delErr := destination.DeleteSubaccountDestination(ctx, destClient.URI, destClient.Token, name)
			if delErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "  ERROR deleting %q: %v\n", name, delErr)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "  deleted: %s\n", name)
			}
		}
		return nil
	},
}

// ── registration ──────────────────────────────────────────────────────────────

func init() {
	// subaccount-destinations
	subaccountDestinationsCmd.Flags().String("org", "", "Org GUID or name substring to target (required)")
	subaccountDestinationsCmd.Flags().String("regions", "", "Comma-separated CF regions to search (default: last login regions)")
	subaccountDestinationsCmd.Flags().String("format", "toon", "Output format: toon (default), json, or csv (csv only without --full)")
	subaccountDestinationsCmd.Flags().Bool("full", false, "Include all destination properties as a flat object (default: Name, URL, sap-client only)")
	subaccountDestinationsCmd.Flags().String("filter", "", "Case-insensitive substring or glob pattern matched against any destination property")
	subaccountDestinationsCmd.Flags().Bool("no-prompt", false, "Skip interactive prompts — skip instances with no service key")
	_ = subaccountDestinationsCmd.MarkFlagRequired("org")
	rootCmd.AddCommand(subaccountDestinationsCmd)

	// create-subaccount-destinations
	createSubaccountDestinationsCmd.Flags().String("org", "", "Org GUID or name substring to target (required)")
	createSubaccountDestinationsCmd.Flags().String("destinations", "", "Path to JSON file containing destinations array (required)")
	createSubaccountDestinationsCmd.Flags().String("regions", "", "Comma-separated CF regions to search (default: last login regions)")
	createSubaccountDestinationsCmd.Flags().Bool("no-prompt", false, "Skip interactive prompts — fail if no service instance or key found")
	_ = createSubaccountDestinationsCmd.MarkFlagRequired("org")
	_ = createSubaccountDestinationsCmd.MarkFlagRequired("destinations")
	rootCmd.AddCommand(createSubaccountDestinationsCmd)

	// update-subaccount-destinations
	updateSubaccountDestinationsCmd.Flags().String("org", "", "Org GUID or name substring to target (required)")
	updateSubaccountDestinationsCmd.Flags().String("destinations", "", "Path to JSON file containing destinations array (required)")
	updateSubaccountDestinationsCmd.Flags().String("regions", "", "Comma-separated CF regions to search (default: last login regions)")
	updateSubaccountDestinationsCmd.Flags().Bool("no-prompt", false, "Skip interactive prompts — fail if no service instance or key found")
	_ = updateSubaccountDestinationsCmd.MarkFlagRequired("org")
	_ = updateSubaccountDestinationsCmd.MarkFlagRequired("destinations")
	rootCmd.AddCommand(updateSubaccountDestinationsCmd)

	// delete-subaccount-destinations
	deleteSubaccountDestinationsCmd.Flags().String("org", "", "Org GUID or name substring to target (required)")
	deleteSubaccountDestinationsCmd.Flags().String("destinations", "", "Path to JSON file — only \"Name\" field is used (required)")
	deleteSubaccountDestinationsCmd.Flags().String("regions", "", "Comma-separated CF regions to search (default: last login regions)")
	deleteSubaccountDestinationsCmd.Flags().Bool("no-prompt", false, "Skip interactive prompts — fail if no service instance or key found")
	_ = deleteSubaccountDestinationsCmd.MarkFlagRequired("org")
	_ = deleteSubaccountDestinationsCmd.MarkFlagRequired("destinations")
	rootCmd.AddCommand(deleteSubaccountDestinationsCmd)
}
