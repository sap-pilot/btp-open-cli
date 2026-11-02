package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"
	"btp-open-cli/internal/xsuaa"

	"github.com/spf13/cobra"
)

var deleteRoleCollectionsCmd = &cobra.Command{
	Use:   "delete-role-collections [role_collection_name_pattern]",
	Short: "Delete XSUAA role collections by name pattern, or from an exported role-collections file",
	Long: `Delete role collections from the XSUAA (Authorization and Trust Management)
apiaccess service, either by discovering them across orgs (the default) or
by reading an exported role-collections file via --role-collections <file>.

Default mode (no --role-collections): <role_collection_name_pattern> is
required — a comma-separated list of patterns, e.g. 'Custom_*,temp-*' or '*'
(matches every name). Each pattern is a glob (via filepath.Match) if it
contains * ? [, otherwise a case-insensitive substring; a pattern prefixed
with '!' reverses it, and a role collection is selected if any pattern —
negated or not — matches it. Role collections are discovered the same way
as 'bo role-collections', across --org/--orgs/--excludeOrgs or the default
org scope selected via 'bo orgs'.

--role-collections <file> mode: <file> is a JSON or TOON document produced
by 'bo role-collections' (or hand-authored with the same
regions -> orgs -> roleCollections shape). Only the orgs and role
collections listed in the file are targeted — --org/--orgs/--excludeOrgs,
--regions, and the default org scope are all ignored.
<role_collection_name_pattern> becomes optional in this mode (default '*',
every role collection in the file) and still further narrows the file's
role collections by name.

⚠️ This is destructive and cannot be undone.

Without -y, a TOON preview of all role collections to be deleted is shown
and confirmation is required before any changes are made.`,
	Args: optionalPatternArgsIfFlagSet("role-collections"),
	RunE: func(cmd *cobra.Command, args []string) error {
		rcFile, _ := cmd.Flags().GetString("role-collections")
		skipConfirm, _ := cmd.Flags().GetBool("yes")

		pattern := "*"
		if len(args) > 0 {
			pattern = strings.TrimSpace(args[0])
			if pattern == "" {
				return fmt.Errorf("role_collection_name_pattern cannot be empty")
			}
		}

		creds, err := store.Load()
		if err != nil {
			return fmt.Errorf("not logged in — run: bo login --regions <region>")
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		var targets []rcTarget
		if rcFile != "" {
			entries, err := parseRoleCollectionsFile(rcFile)
			if err != nil {
				return fmt.Errorf("invalid --role-collections file: %w", err)
			}
			var filtered []rcFileEntry
			for _, e := range entries {
				if matchesNegatablePattern(pattern, e.RC.Name) {
					filtered = append(filtered, e)
				}
			}
			if len(filtered) == 0 {
				return fmt.Errorf("no role collections matched — check the --role-collections file and the name pattern")
			}
			targets, err = resolveRCTargets(ctx, creds, filtered)
			if err != nil {
				return err
			}
		} else {
			targets, err = discoverRCTargets(cmd, ctx, creds, pattern)
			if err != nil {
				return err
			}
		}

		if len(targets) == 0 {
			return fmt.Errorf("no role collections matched — check --org/--orgs, --excludeOrgs, and the name pattern")
		}
		sortRCTargets(targets)

		return applyRCPreviewAndRun(ctx, targets,
			"Role collections to be deleted:", "This cannot be undone. Proceed? [y/N] ", "Deleting role collections...", "-",
			skipConfirm,
			func(ctx context.Context, apiURL, token string, rc xsuaa.RoleCollection) error {
				return xsuaa.DeleteRoleCollection(ctx, apiURL, token, rc.Name)
			})
	},
}

// discoverRCTargets finds role collections matching pattern across orgs in
// scope, via --org/--orgs/--excludeOrgs/--regions or the default org scope
// from 'bo orgs' — the same discovery flow as 'bo role-collections'.
func discoverRCTargets(cmd *cobra.Command, ctx context.Context, creds *store.Credentials, pattern string) ([]rcTarget, error) {
	regionsFlag, _ := cmd.Flags().GetString("regions")
	orgsFile, _ := cmd.Flags().GetString("orgs")
	excludeOrgsFile, _ := cmd.Flags().GetString("excludeOrgs")
	orgFilterArg, _ := cmd.Flags().GetString("org")
	noPrompt, _ := cmd.Flags().GetBool("no-prompt")

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
	var err error
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

	if orgFilterArg != "" {
		fl := strings.ToLower(orgFilterArg)
		for _, apiURL := range apiURLs {
			tok, ok := creds.Tokens[apiURL]
			if !ok {
				continue
			}
			regionName := store.APIURLToRegion(apiURL)
			cfClient := cf.NewClient(apiURL, tok.AccessToken)
			cfClient.SetTokenRefresher(makeTokenRefresher(apiURL, tok.AccessToken))
			orgs, listErr := cfClient.ListOrganizations(ctx)
			if listErr != nil {
				fmt.Fprintf(os.Stderr, "[%s] error listing orgs: %v\n", regionName, listErr)
				continue
			}
			for _, org := range orgs {
				if strings.EqualFold(org.GUID, orgFilterArg) || strings.Contains(strings.ToLower(org.Name), fl) {
					includeOrgs = append(includeOrgs, cosOrgRef{ID: org.GUID})
				}
			}
		}
		if len(includeOrgs) == 0 {
			return nil, fmt.Errorf("org %q not found in any accessible region", orgFilterArg)
		}
	} else if orgsFile == "" {
		includeOrgs, err = resolveDefaultOrgScope(creds)
		if err != nil {
			return nil, err
		}
	}

	clients, _, err := resolveXsuaaClients(ctx, apiURLs, creds, includeOrgs, excludeOrgs, noPrompt)
	if err != nil {
		return nil, err
	}

	type orgResult struct {
		w   xsuaaOrgClient
		rcs []xsuaa.RoleCollection
		err error
	}
	results := make([]orgResult, len(clients))
	var wg sync.WaitGroup
	for i, w := range clients {
		wg.Add(1)
		go func(idx int, w xsuaaOrgClient) {
			defer wg.Done()
			rcs, err := xsuaa.ListRoleCollections(ctx, w.APIURL, w.Token)
			results[idx] = orgResult{w: w, rcs: rcs, err: err}
		}(i, w)
	}
	wg.Wait()

	var targets []rcTarget
	for _, r := range results {
		if r.err != nil {
			fmt.Fprintf(os.Stderr, "[%s] %s: %v\n", r.w.RegionName, r.w.OrgName, r.err)
			continue
		}
		for _, rc := range r.rcs {
			if !matchesNegatablePattern(pattern, rc.Name) {
				continue
			}
			targets = append(targets, rcTarget{
				RegionName: r.w.RegionName,
				OrgGUID:    r.w.OrgGUID,
				OrgName:    r.w.OrgName,
				APIURL:     r.w.APIURL,
				Token:      r.w.Token,
				RC:         rc,
			})
		}
	}
	return targets, nil
}

func init() {
	deleteRoleCollectionsCmd.GroupID = "xsuaa"
	rootCmd.AddCommand(deleteRoleCollectionsCmd)
	deleteRoleCollectionsCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	deleteRoleCollectionsCmd.Flags().String("org", "", "Org name or GUID to target (case-insensitive substring match on name, exact on GUID)")
	deleteRoleCollectionsCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	deleteRoleCollectionsCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,org_id,org_name)")
	deleteRoleCollectionsCmd.Flags().String("role-collections", "", "Path to a role-collections JSON/TOON file produced by 'bo role-collections'; bypasses --org/--orgs/--excludeOrgs/--regions and the 'bo orgs' default scope, targeting exactly the orgs/role collections it lists")
	deleteRoleCollectionsCmd.Flags().Bool("no-prompt", false, "Skip interactive prompts — orgs with no service instance or key are silently skipped (discovery mode only)")
	deleteRoleCollectionsCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
}
