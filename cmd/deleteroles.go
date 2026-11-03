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

var deleteRolesCmd = &cobra.Command{
	Use:   "delete-roles [role_name_pattern]",
	Short: "Delete XSUAA roles by name pattern, or from an exported roles file",
	Long: `Delete roles from the XSUAA (Authorization and Trust Management) apiaccess
service, either by discovering them across orgs (the default) or by reading
an exported roles file via --roles <file>.

Default mode (no --roles): <role_name_pattern> is required — a comma-separated
list of patterns, e.g. 'custom-*,temp-*' or '*' (matches every role name).
Each pattern is a glob (via filepath.Match) if it contains * ? [, otherwise a
case-insensitive substring; a pattern prefixed with '!' reverses it, and a
role is selected if any pattern — negated or not — matches it. Roles are
discovered the same way as 'bo roles', across --org/--orgs/--excludeOrgs or
the default org scope selected via 'bo orgs'.

--roles <file> mode: <file> is a JSON or TOON document produced by 'bo roles'
(or hand-authored with the same regions -> orgs -> roles shape). Roles are
matched by name from the (filtered) file; --org/--orgs/--excludeOrgs/
--regions are ignored. <role_name_pattern> becomes optional in this mode
(default '*', every role in the file) and still further narrows the file's
roles by name.

The orgs actually targeted depend on whether a default org scope is
currently selected via 'bo orgs': if one is, every role in the file is
deleted from those org(s) instead of the file's own — discarding the
file's own org assignment entirely, so the file can double as a role
template to apply against any subaccount just by changing your 'bo orgs'
selection. If no default org scope is selected, each role's own org
(exactly as specified in the file) is used. Either way, each role's
roleTemplateAppId is automatically re-resolved against the target org's
own role templates, matching by app name (ignoring the subaccount-specific
suffix after '!') and roleTemplateName.

⚠️ This is destructive and cannot be undone.

Without -y, a TOON preview of all roles to be deleted is shown and
confirmation is required before any changes are made.`,
	Args: optionalPatternArgsIfFlagSet("roles"),
	RunE: func(cmd *cobra.Command, args []string) error {
		rolesFile, _ := cmd.Flags().GetString("roles")
		skipConfirm, _ := cmd.Flags().GetBool("yes")

		pattern := "*"
		if len(args) > 0 {
			pattern = strings.TrimSpace(args[0])
			if pattern == "" {
				return fmt.Errorf("role_name_pattern cannot be empty")
			}
		}

		creds, err := store.Load()
		if err != nil {
			return fmt.Errorf("not logged in — run: bo login --regions <region>")
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		var targets []roleTarget
		var scopeNote string
		if rolesFile != "" {
			entries, err := parseRolesFile(rolesFile)
			if err != nil {
				return fmt.Errorf("invalid --roles file: %w", err)
			}
			filtered := filterRoleEntriesByPattern(entries, pattern)
			if len(filtered) == 0 {
				return fmt.Errorf("no roles matched — check the --roles file and the name pattern")
			}

			destOrgs := resolveFileOrgRetarget(creds)
			filtered = retargetEntries(filtered, destOrgs, func(e roFileEntry, region, orgID, orgName string) roFileEntry {
				e.Region, e.OrgID, e.OrgName = region, orgID, orgName
				return e
			})
			if destOrgs != nil {
				scopeNote = orgScopeNote(destOrgs, "--roles file")
			}

			targets, err = resolveRoleTargets(ctx, creds, filtered)
			if err != nil {
				return err
			}
		} else {
			targets, err = discoverRoleTargets(cmd, ctx, creds, pattern)
			if err != nil {
				return err
			}
		}

		if len(targets) == 0 {
			return fmt.Errorf("no roles matched — check --org/--orgs, --excludeOrgs, and the name pattern")
		}
		sortRoleTargets(targets)

		return applyRolesPreviewAndRun(ctx, targets,
			"Roles to be deleted:", "This cannot be undone. Proceed? [y/N] ", "Deleting roles...", "-", scopeNote,
			skipConfirm,
			func(ctx context.Context, apiURL, token string, role xsuaa.Role) error {
				return xsuaa.DeleteRole(ctx, apiURL, token, role.RoleTemplateAppID, role.RoleTemplateName, role.Name)
			})
	},
}

// discoverRoleTargets finds roles matching pattern across orgs in scope,
// via --org/--orgs/--excludeOrgs/--regions or the default org scope from
// 'bo orgs' — the same discovery flow as 'bo roles'.
func discoverRoleTargets(cmd *cobra.Command, ctx context.Context, creds *store.Credentials, pattern string) ([]roleTarget, error) {
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
		w     xsuaaOrgClient
		roles []xsuaa.Role
		err   error
	}
	results := make([]orgResult, len(clients))
	var wg sync.WaitGroup
	for i, w := range clients {
		wg.Add(1)
		go func(idx int, w xsuaaOrgClient) {
			defer wg.Done()
			roles, err := xsuaa.ListRoles(ctx, w.APIURL, w.Token)
			results[idx] = orgResult{w: w, roles: roles, err: err}
		}(i, w)
	}
	wg.Wait()

	var targets []roleTarget
	for _, r := range results {
		if r.err != nil {
			fmt.Fprintf(os.Stderr, "[%s] %s: %v\n", r.w.RegionName, r.w.OrgName, r.err)
			continue
		}
		for _, role := range r.roles {
			if !matchesNegatablePattern(pattern, role.Name) {
				continue
			}
			targets = append(targets, roleTarget{
				RegionName: r.w.RegionName,
				OrgGUID:    r.w.OrgGUID,
				OrgName:    r.w.OrgName,
				APIURL:     r.w.APIURL,
				Token:      r.w.Token,
				Role:       role,
			})
		}
	}
	return targets, nil
}

func init() {
	deleteRolesCmd.GroupID = "xsuaa"
	rootCmd.AddCommand(deleteRolesCmd)
	deleteRolesCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	deleteRolesCmd.Flags().String("org", "", "Org name or GUID to target (case-insensitive substring match on name, exact on GUID)")
	deleteRolesCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	deleteRolesCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,org_id,org_name)")
	deleteRolesCmd.Flags().String("roles", "", "Path to a roles JSON/TOON file produced by 'bo roles'; bypasses --org/--orgs/--excludeOrgs/--regions, targeting the roles it lists within whatever org(s) are selected via 'bo orgs' (or, with no default org scope selected, the orgs the file itself specifies)")
	deleteRolesCmd.Flags().Bool("no-prompt", false, "Skip interactive prompts — orgs with no service instance or key are silently skipped (discovery mode only)")
	deleteRolesCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
}
