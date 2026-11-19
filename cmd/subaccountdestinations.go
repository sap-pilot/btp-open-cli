package cmd

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"

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

// sadLeadingCols are subaccount-destinations' own structural columns (see
// destFlatRow) — just org identity, since there's no space/instance concept
// at subaccount level.
var sadLeadingCols = []string{"org_id", "org_name"}

// flattenSadRows converts docs into one flat destFlatRow per destination.
func flattenSadRows(docs []sadOrgDoc) []destFlatRow {
	var rows []destFlatRow
	for _, doc := range docs {
		for _, d := range doc.Destinations {
			rows = append(rows, destFlatRow{OrgID: doc.OrgID, OrgName: doc.OrgName, Props: d})
		}
	}
	return rows
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
// If no destination service instance at all is found anywhere in the org
// and noPrompt is false, the user is prompted to create one and the search
// is retried once; noPrompt instead fails immediately. A found instance
// with no service key is instead governed by keyOption (see
// resolveDestInstanceClient) — independently of noPrompt. Callers must call
// cleanupDestClients(client) once they're done using the returned client.
func resolveOrgDestClient(
	ctx context.Context,
	cmd *cobra.Command,
	orgFlag string,
	creds *store.Credentials,
	apiURLs []string,
	noPrompt bool,
	keyOption destKeyOption,
) (orgGUID, orgName string, client sdDestClient, err error) {

	// ── step 1: find the org across regions ──────────────────────────────────
	// The client that successfully lists orgs is reused for all subsequent CF
	// calls. This avoids creating a second client from the stale in-memory token
	// (creds.Tokens) if the first client already triggered a token refresh.
	type foundOrg struct {
		guid string
		name string
	}
	var (
		found    *foundOrg
		cfClient *cf.Client
	)

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
				found = &foundOrg{guid: org.GUID, name: org.Name}
				cfClient = c
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

	// ── step 2: look up destination/lite plan (reuse the client with fresh token)
	destPlan, planErr := cfClient.FindServicePlan(ctx, "destination", "lite")
	if planErr != nil {
		return "", "", sdDestClient{}, fmt.Errorf("looking up destination service plan: %w", planErr)
	}
	if destPlan == nil {
		return "", "", sdDestClient{},
			fmt.Errorf("destination service plan 'lite' not found in region %s",
				store.APIURLToRegion(cfClient.BaseURL()))
	}

	// ── step 4: scan all spaces in the org for a usable instance ─────────────
	if creds.SpaceDestServices == nil {
		creds.SpaceDestServices = make(map[string]map[string]*store.DestInstanceCache)
	}

	tryFindClient := func() (sdDestClient, bool, error) {
		spaces, spacesErr := cfClient.ListOrganizationSpaces(ctx, found.guid)
		if spacesErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: listing spaces in org %q: %v\n", found.name, spacesErr)
			return sdDestClient{}, false, nil
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
				c, ok, resolveErr := resolveDestInstanceClient(ctx, cmd, cfClient, space.Name, inst, spaceCache, keyOption)
				if resolveErr != nil {
					return sdDestClient{}, false, resolveErr
				}
				if ok {
					return c, true, nil
				}
			}
		}
		return sdDestClient{}, false, nil
	}

	if c, ok, tryErr := tryFindClient(); tryErr != nil {
		return "", "", sdDestClient{}, tryErr
	} else if ok {
		saveDestCache(cmd, creds)
		return found.guid, found.name, c, nil
	}

	// No instance found — prompt or fail.
	if noPrompt {
		return "", "", sdDestClient{},
			fmt.Errorf("no destination service instance found in org %q — create one and run again", found.name)
	}
	for {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"\nWARNING: No destination service instance found in org %q\n"+
				"  Create one in any space, e.g.:\n"+
				"    cf create-service destination lite <instance-name>\n"+
				"    cf create-service-key <instance-name> bo-dest-key\n"+
				"  Then press Enter to retry, type 's' to skip this org, or Ctrl-C to abort.\n",
			found.name)
		retry, skip := promptRetryOrSkip(ctx)
		if !retry && !skip {
			return "", "", sdDestClient{}, errAborted
		}
		if skip {
			return "", "", sdDestClient{}, fmt.Errorf("no destination service instance available in org %q", found.name)
		}

		c, ok, tryErr := tryFindClient()
		if tryErr != nil {
			return "", "", sdDestClient{}, tryErr
		}
		if ok {
			saveDestCache(cmd, creds)
			return found.guid, found.name, c, nil
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: still no destination service instance found in org %q\n", found.name)
	}
}

// ── subaccount-destinations ───────────────────────────────────────────────────

var subaccountDestinationsCmd = &cobra.Command{
	Use:   "subaccount-destinations",
	Short: "List subaccount-level destinations via the destination service",
	Long: `Retrieves all subaccount-level destinations from the destination service using
any destination service instance found in the target org(s) (--org GUID or name).

Every destination property is included exactly as the destination service
API responds — nothing is redacted, including sensitive fields such as
Password, ClientSecret, and ProxyPassword; this is sensitive output, see
README.

Use --include/--exclude to narrow results: each accepts a comma-separated
list of keywords, matched against any destination property key or value —
as a glob pattern (e.g. API*PP) if a keyword contains * ? [, otherwise as a
case-insensitive substring.

--format csv gives a flat CSV, one row per destination: org_id and org_name
lead, followed by every property key that appears anywhere in the result
set (sorted alphabetically), so the column set reflects whatever
destinations were actually returned.

Use --fields <column,...> (comma-separated, in the order given — org_id,
org_name, or any destination property name, exactly as the API casing has
it, e.g. "Name" not "name") to limit output to just those columns instead —
switches json/toon to that same flat {"rows": [...]} view too. Omit it to
get every column (csv) or the normal nested json/toon structure below.

Use --sort-by <column,...> to sort rows by one or more of those same
columns (any name accepted, matched the same way), each optionally
suffixed with ":desc" for descending (":asc" is the default) — also
switches json/toon to the flat view. Omit it to keep the default order:
sorted by Name within each org.

If neither --org nor --orgs is given, the default org scope selected via
'bo orgs' is used. If no default scope has been set either, run 'bo orgs' to
pick one interactively, or pass --org/--orgs directly. With more than one
target org, --format json/toon returns a list of per-org results instead of
a single object.

--no-key-option <s/skip|c/create|t/transient|a/abort> governs what happens
when a destination service instance is found with no service key: skip
warns and moves on; create automatically creates a 'bo-dest-key' service key
on the instance and keeps it; transient does the same but deletes that key
again once it's no longer needed for this run; abort stops the whole
command the first time it happens. Omit this flag to be asked
s/skip/c/create/t/transient/a/abort interactively each time such an
instance is found instead, defaulting to skip on a blank answer — unless -y
is also given, in which case skip is used directly with no prompt.
--no-prompt is the separate, unrelated case of an org having no destination
service instance at all: with it, that fails immediately instead of
prompting to create one.

Use --output/-o to write the result to a file instead of stdout. If --format
is not explicitly given, it is auto-detected from --output's extension:
.json selects json, .csv selects csv, .toon selects toon — otherwise the
default toon applies. Pass --format explicitly to override the extension.

The access token is cached locally and reused until it expires or 'bo logoff' is run.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		orgFlag, _ := cmd.Flags().GetString("org")
		orgsFile, _ := cmd.Flags().GetString("orgs")
		regionsFlag, _ := cmd.Flags().GetString("regions")
		format, _ := cmd.Flags().GetString("format")
		includePattern, _ := cmd.Flags().GetString("include")
		excludePattern, _ := cmd.Flags().GetString("exclude")
		noPrompt, _ := cmd.Flags().GetBool("no-prompt")
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

		// All of this command's output goes through cmd.OutOrStdout(), so
		// redirecting it here also covers the final JSON/toon/CSV result.
		out, closeOut, err := resolveOutputWriter(outputFile)
		if err != nil {
			return err
		}
		defer closeOut()
		cmd.SetOut(out)

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		// Determine which orgs to target: --org (single, matched by GUID or
		// name substring), --orgs (a CSV of exact org refs), or the default
		// org scope selected via 'bo orgs'.
		orgTargets, err := resolveOrgTargets(creds, orgFlag, orgsFile)
		if err != nil {
			return err
		}

		var docs []sadOrgDoc
		for _, target := range orgTargets {
			orgGUID, orgName, destClient, resolveErr := resolveOrgDestClient(ctx, cmd, target, creds, apiURLs, noPrompt, keyOption)
			if resolveErr != nil {
				if errors.Is(resolveErr, errAborted) {
					return errAborted
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", resolveErr)
				continue
			}

			rawDests, fetchErr := destination.ListSubaccountDestinationsFull(ctx, destClient.URI, destClient.Token)
			cleanupDestClients(destClient)
			if fetchErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] listing subaccount destinations: %v\n", orgName, fetchErr)
				continue
			}

			var dests []map[string]string
			for _, raw := range rawDests {
				if includePattern != "" && !sdMatchesAnyKeyword(includePattern, raw) {
					continue
				}
				if excludePattern != "" && sdMatchesAnyKeyword(excludePattern, raw) {
					continue
				}
				dests = append(dests, raw)
			}
			sort.Slice(dests, func(i, j int) bool { return dests[i]["Name"] < dests[j]["Name"] })

			docs = append(docs, sadOrgDoc{
				OrgID:        orgGUID,
				OrgName:      orgName,
				Destinations: dests,
			})
		}
		if len(docs) == 0 {
			return fmt.Errorf("no destinations retrieved — no target org resolved to a usable destination service instance")
		}

		flatRowsFor := func() []destFlatRow {
			flatRows := flattenSadRows(docs)
			if len(sortKeys) > 0 {
				destSortRowsBy(flatRows, sortKeys)
			}
			return flatRows
		}

		switch strings.ToLower(format) {
		case "json":
			if len(fieldIDs) > 0 || len(sortKeys) > 0 {
				out, jsonErr := json.MarshalIndent(flatRowsDoc{Rows: destRowMaps(flatRowsFor(), fieldIDs, sadLeadingCols)}, "", "  ")
				if jsonErr != nil {
					return fmt.Errorf("encoding JSON: %w", jsonErr)
				}
				fmt.Fprintln(cmd.OutOrStdout(), string(out))
				return nil
			}
			var out []byte
			if len(docs) == 1 {
				out, err = json.MarshalIndent(docs[0], "", "  ")
			} else {
				out, err = json.MarshalIndent(docs, "", "  ")
			}
			if err != nil {
				return fmt.Errorf("encoding JSON: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(out))

		case "csv":
			w := csv.NewWriter(cmd.OutOrStdout())
			defer w.Flush()
			return destWriteCSV(w, flatRowsFor(), fieldIDs, sadLeadingCols)

		default: // toon
			if len(fieldIDs) > 0 || len(sortKeys) > 0 {
				out, toonErr := toonenc.Marshal(flatRowsDoc{Rows: destRowMaps(flatRowsFor(), fieldIDs, sadLeadingCols)}, toonenc.WithIndent(2))
				if toonErr != nil {
					return fmt.Errorf("encoding TOON: %w", toonErr)
				}
				fmt.Fprintln(cmd.OutOrStdout(), string(out))
				return nil
			}
			var out []byte
			if len(docs) == 1 {
				out, err = toonenc.Marshal(docs[0], toonenc.WithIndent(2))
			} else {
				out, err = toonenc.Marshal(struct {
					Orgs []sadOrgDoc `json:"orgs" toon:"orgs"`
				}{docs}, toonenc.WithIndent(2))
			}
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
in each target org (--org GUID or name).

The JSON file must be an array of destination objects, e.g.:
  [{"Name":"my-dest","Type":"HTTP","URL":"https://...","Authentication":"NoAuthentication"}]

If neither --org nor --orgs is given, the default org scope selected via
'bo orgs' is used. If no default scope has been set either, run 'bo orgs' to
pick one interactively, or pass --org/--orgs directly. The same destinations
file is applied to every target org — with more than one target org in
scope, double-check the scope (e.g. 'bo orgs' or --org) before running.

The access token is cached locally and reused until it expires or 'bo logoff' is run.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		orgFlag, _ := cmd.Flags().GetString("org")
		orgsFile, _ := cmd.Flags().GetString("orgs")
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

		orgTargets, err := resolveOrgTargets(creds, orgFlag, orgsFile)
		if err != nil {
			return err
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		var anyOK bool
		for _, target := range orgTargets {
			_, orgName, destClient, resolveErr := resolveOrgDestClient(ctx, cmd, target, creds, apiURLs, noPrompt, destKeyOptionFromNoPrompt(noPrompt))
			if resolveErr != nil {
				if errors.Is(resolveErr, errAborted) {
					return errAborted
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", resolveErr)
				continue
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Creating subaccount destinations in org %s (via instance: %s)...\n",
				orgName, destClient.InstanceName)
			items, postErr := destination.CreateSubaccountDestinations(ctx, destClient.URI, destClient.Token, rawBody)
			cleanupDestClients(destClient)
			if postErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "    ERROR: [%s] creating subaccount destinations: %v\n", orgName, postErr)
				continue
			}
			printActionResults(cmd, "created", names, items)
			anyOK = true
		}
		if !anyOK {
			return fmt.Errorf("no target org completed successfully")
		}
		return nil
	},
}

// ── update-subaccount-destinations ───────────────────────────────────────────

var updateSubaccountDestinationsCmd = &cobra.Command{
	Use:   "update-subaccount-destinations",
	Short: "Update subaccount-level destinations via the destination service",
	Long: `Reads destinations from a JSON file (--destinations) and PUTs them to the
subaccount-level destination endpoint using a destination service instance found
in each target org (--org GUID or name).

Existing destinations with the same Name are overwritten; others are left unchanged.

The JSON file must be an array of destination objects.

If neither --org nor --orgs is given, the default org scope selected via
'bo orgs' is used. If no default scope has been set either, run 'bo orgs' to
pick one interactively, or pass --org/--orgs directly. The same destinations
file is applied to every target org — with more than one target org in
scope, double-check the scope (e.g. 'bo orgs' or --org) before running.

The access token is cached locally and reused until it expires or 'bo logoff' is run.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		orgFlag, _ := cmd.Flags().GetString("org")
		orgsFile, _ := cmd.Flags().GetString("orgs")
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

		orgTargets, err := resolveOrgTargets(creds, orgFlag, orgsFile)
		if err != nil {
			return err
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		var anyOK bool
		for _, target := range orgTargets {
			_, orgName, destClient, resolveErr := resolveOrgDestClient(ctx, cmd, target, creds, apiURLs, noPrompt, destKeyOptionFromNoPrompt(noPrompt))
			if resolveErr != nil {
				if errors.Is(resolveErr, errAborted) {
					return errAborted
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", resolveErr)
				continue
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Updating subaccount destinations in org %s (via instance: %s)...\n",
				orgName, destClient.InstanceName)
			items, putErr := destination.UpdateSubaccountDestinations(ctx, destClient.URI, destClient.Token, rawBody)
			cleanupDestClients(destClient)
			if putErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "    ERROR: [%s] updating subaccount destinations: %v\n", orgName, putErr)
				continue
			}
			printActionResults(cmd, "updated", names, items)
			anyOK = true
		}
		if !anyOK {
			return fmt.Errorf("no target org completed successfully")
		}
		return nil
	},
}

// ── delete-subaccount-destinations ───────────────────────────────────────────

var deleteSubaccountDestinationsCmd = &cobra.Command{
	Use:   "delete-subaccount-destinations",
	Short: "Delete subaccount-level destinations via the destination service",
	Long: `Reads destination names from a JSON file (--destinations) and deletes each
matching destination from the subaccount-level endpoint using a destination
service instance found in each target org (--org GUID or name).

The JSON file must be an array of destination objects; only the "Name" field is
used. Non-existent destinations are silently ignored (idempotent).

If neither --org nor --orgs is given, the default org scope selected via
'bo orgs' is used. If no default scope has been set either, run 'bo orgs' to
pick one interactively, or pass --org/--orgs directly. The same destination
names are deleted from every target org — with more than one target org in
scope, double-check the scope (e.g. 'bo orgs' or --org) before running.

The access token is cached locally and reused until it expires or 'bo logoff' is run.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		orgFlag, _ := cmd.Flags().GetString("org")
		orgsFile, _ := cmd.Flags().GetString("orgs")
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

		orgTargets, err := resolveOrgTargets(creds, orgFlag, orgsFile)
		if err != nil {
			return err
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		var anyOK bool
		for _, target := range orgTargets {
			_, orgName, destClient, resolveErr := resolveOrgDestClient(ctx, cmd, target, creds, apiURLs, noPrompt, destKeyOptionFromNoPrompt(noPrompt))
			if resolveErr != nil {
				if errors.Is(resolveErr, errAborted) {
					return errAborted
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", resolveErr)
				continue
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Deleting %d subaccount destination(s) in org %s (via instance: %s)...\n",
				len(names), orgName, destClient.InstanceName)
			for _, name := range names {
				deleted, delErr := destination.DeleteSubaccountDestination(ctx, destClient.URI, destClient.Token, name)
				if delErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "    ERROR: %s — %v\n", name, delErr)
				} else if deleted {
					fmt.Fprintf(cmd.OutOrStdout(), "    deleted: %s\n", name)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "    not found: %s\n", name)
				}
			}
			cleanupDestClients(destClient)
			anyOK = true
		}
		if !anyOK {
			return fmt.Errorf("no target org completed successfully")
		}
		return nil
	},
}

// ── registration ──────────────────────────────────────────────────────────────

func init() {
	// subaccount-destinations
	subaccountDestinationsCmd.Flags().String("org", "", "Org GUID or name substring to target")
	subaccountDestinationsCmd.Flags().String("orgs", "", "Path to CSV of orgs to target (columns: region,org_id,org_name)")
	subaccountDestinationsCmd.Flags().String("regions", "", "Comma-separated CF regions to search (default: last login regions)")
	subaccountDestinationsCmd.Flags().String("format", "toon", "Output format: toon (default), json, or csv; auto-detected from --output's extension (.json, .csv, or .toon) when --format is not explicitly given")
	subaccountDestinationsCmd.Flags().String("include", "", "Only include destinations where any property contains any of these comma-separated keywords (substring, or glob if a keyword has * ? [)")
	subaccountDestinationsCmd.Flags().String("exclude", "", "Exclude destinations where any property contains any of these comma-separated keywords (substring, or glob if a keyword has * ? [)")
	subaccountDestinationsCmd.Flags().Bool("no-prompt", false, "Fail immediately (instead of prompting) when an org has no destination service instance at all")
	subaccountDestinationsCmd.Flags().String("no-key-option", "", "What to do with a destination service instance found with no service key: s/skip (default), c/create (auto-create one, kept), t/transient (create, use, then delete it), or a/abort; omit to be asked interactively per instance (unless -y), s/skip if -y is given")
	subaccountDestinationsCmd.Flags().BoolP("yes", "y", false, "Don't ask interactively about a missing destination service key — default to s/skip instead, unless --no-key-option is also given")
	subaccountDestinationsCmd.Flags().StringP("output", "o", "", "Write output to this file instead of stdout")
	subaccountDestinationsCmd.Flags().String("fields", "", "Comma-separated column names to limit output to, in the order given (org_id, org_name, or any destination property name exactly as the API casing has it); switches json/toon to a flat {rows:[...]} view too")
	subaccountDestinationsCmd.Flags().String("sort-by", "", "Comma-separated column names to sort by, each optionally suffixed with \":desc\" (default \":asc\"); switches json/toon to a flat {rows:[...]} view too")
	subaccountDestinationsCmd.GroupID = "destination"
	rootCmd.AddCommand(subaccountDestinationsCmd)

	// create-subaccount-destinations
	createSubaccountDestinationsCmd.Flags().String("org", "", "Org GUID or name substring to target")
	createSubaccountDestinationsCmd.Flags().String("orgs", "", "Path to CSV of orgs to target (columns: region,org_id,org_name)")
	createSubaccountDestinationsCmd.Flags().String("destinations", "", "Path to JSON file containing destinations array (required)")
	createSubaccountDestinationsCmd.Flags().String("regions", "", "Comma-separated CF regions to search (default: last login regions)")
	createSubaccountDestinationsCmd.Flags().Bool("no-prompt", false, "Skip interactive prompts — fail if no service instance or key found")
	_ = createSubaccountDestinationsCmd.MarkFlagRequired("destinations")
	createSubaccountDestinationsCmd.GroupID = "destination"
	rootCmd.AddCommand(createSubaccountDestinationsCmd)

	// update-subaccount-destinations
	updateSubaccountDestinationsCmd.Flags().String("org", "", "Org GUID or name substring to target")
	updateSubaccountDestinationsCmd.Flags().String("orgs", "", "Path to CSV of orgs to target (columns: region,org_id,org_name)")
	updateSubaccountDestinationsCmd.Flags().String("destinations", "", "Path to JSON file containing destinations array (required)")
	updateSubaccountDestinationsCmd.Flags().String("regions", "", "Comma-separated CF regions to search (default: last login regions)")
	updateSubaccountDestinationsCmd.Flags().Bool("no-prompt", false, "Skip interactive prompts — fail if no service instance or key found")
	_ = updateSubaccountDestinationsCmd.MarkFlagRequired("destinations")
	updateSubaccountDestinationsCmd.GroupID = "destination"
	rootCmd.AddCommand(updateSubaccountDestinationsCmd)

	// delete-subaccount-destinations
	deleteSubaccountDestinationsCmd.Flags().String("org", "", "Org GUID or name substring to target")
	deleteSubaccountDestinationsCmd.Flags().String("orgs", "", "Path to CSV of orgs to target (columns: region,org_id,org_name)")
	deleteSubaccountDestinationsCmd.Flags().String("destinations", "", "Path to JSON file — only \"Name\" field is used (required)")
	deleteSubaccountDestinationsCmd.Flags().String("regions", "", "Comma-separated CF regions to search (default: last login regions)")
	deleteSubaccountDestinationsCmd.Flags().Bool("no-prompt", false, "Skip interactive prompts — fail if no service instance or key found")
	_ = deleteSubaccountDestinationsCmd.MarkFlagRequired("destinations")
	deleteSubaccountDestinationsCmd.GroupID = "destination"
	rootCmd.AddCommand(deleteSubaccountDestinationsCmd)
}
