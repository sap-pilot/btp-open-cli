package cmd

import (
	"context"
	"fmt"
	"strings"

	"btp-open-cli/internal/xsuaa"

	"github.com/spf13/cobra"
)

var createRoleCollectionsCmd = &cobra.Command{
	Use:   "create-role-collections [role_collection_name_pattern]",
	Short: "Create XSUAA role collections from an exported role-collections file",
	Long: `Create role collections (with their role references) in the XSUAA
(Authorization and Trust Management) apiaccess service, read from
--role-collections <file> — a JSON or TOON document produced by
'bo role-collections' (or hand-authored with the same
regions -> orgs -> roleCollections shape; its "roles" field, if present, is
ignored).

<role_collection_name_pattern> is optional — a comma-separated list of
patterns, e.g. 'Custom_*,temp-*' or '*' (the default when omitted: every
role collection in the file). Each pattern is a glob (via filepath.Match) if
it contains * ? [, otherwise a case-insensitive substring; a pattern
prefixed with '!' reverses it, and a role collection is selected if any
pattern — negated or not — matches it. Only role collections in
--role-collections matching the pattern are created; the rest of the file
is ignored.

For each org referenced in the (filtered) file an XSUAA token is resolved
the same way as 'bo role-collections'/'bo users' (via the xsuaa/apiaccess
service key) — only the orgs that actually appear are touched.

The orgs actually targeted depend on whether a default org scope is
currently selected via 'bo orgs': if one is, every role collection in the
file is created within those org(s) instead of the file's own —
discarding the file's own org assignment entirely, so the file can double
as a template to apply against any subaccount just by changing your
'bo orgs' selection. If no default org scope is selected, each role
collection's own org (exactly as specified in the file) is used. Either
way, each role reference's roleTemplateAppId is automatically re-resolved
against an existing role in the target org, matching by app name (ignoring
the subaccount-specific suffix after '!'), roleTemplateName, and role_name
— since a role collection can only reference a role that already exists
there (create it first with 'bo create-roles' if needed).

If a role collection with the same name already exists (HTTP 409), creation
is skipped for it.

Without -y, a TOON preview of all role collections to be created is shown
and confirmation is required before any changes are made.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		pattern := "*"
		if len(args) > 0 {
			pattern = strings.TrimSpace(args[0])
			if pattern == "" {
				return fmt.Errorf("role_collection_name_pattern cannot be empty")
			}
		}
		return runRoleCollectionsFileApply(cmd, pattern,
			"Role collections to be created:",
			"Proceed with role collection creation? [y/N] ",
			"Creating role collections...",
			"+",
			func(ctx context.Context, apiURL, token string, rc xsuaa.RoleCollection) error {
				return xsuaa.CreateRoleCollection(ctx, apiURL, token, rc)
			})
	},
}

func init() {
	createRoleCollectionsCmd.GroupID = "xsuaa"
	rootCmd.AddCommand(createRoleCollectionsCmd)
	createRoleCollectionsCmd.Flags().String("role-collections", "", "Path to a role-collections JSON/TOON file produced by 'bo role-collections' (required)")
	createRoleCollectionsCmd.MarkFlagRequired("role-collections") //nolint:errcheck
	createRoleCollectionsCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
}
