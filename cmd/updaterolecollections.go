package cmd

import (
	"context"
	"fmt"
	"strings"

	"btp-open-cli/internal/xsuaa"

	"github.com/spf13/cobra"
)

var updateRoleCollectionsCmd = &cobra.Command{
	Use:   "update-role-collections [role_collection_name_pattern]",
	Short: "Update XSUAA role collections from an exported role-collections file",
	Long: `Update role collections in the XSUAA (Authorization and Trust Management)
apiaccess service, read from --role-collections <file> — a JSON or TOON
document produced by 'bo role-collections' (or hand-authored with the same
regions -> orgs -> roleCollections shape; its "roles" field, if present, is
ignored). Each role collection is identified by name, and the update fully
replaces its description and roleReferences with what the file specifies —
a role reference present on the server but absent from the file's
roleReferences list is removed.

<role_collection_name_pattern> is optional — a comma-separated list of
patterns, e.g. 'Custom_*,temp-*' or '*' (the default when omitted: every
role collection in the file). Each pattern is a glob (via filepath.Match) if
it contains * ? [, otherwise a case-insensitive substring; a pattern
prefixed with '!' reverses it, and a role collection is selected if any
pattern — negated or not — matches it. Only role collections in
--role-collections matching the pattern are updated; the rest of the file
is ignored.

For each org referenced in the (filtered) file an XSUAA token is resolved
the same way as 'bo role-collections'/'bo users' (via the xsuaa/apiaccess
service key) — only the orgs that actually appear are touched.

The orgs actually targeted depend on whether a default org scope is
currently selected via 'bo orgs': if one is, every role collection in the
file is updated within those org(s) instead of the file's own —
discarding the file's own org assignment entirely. If no default org
scope is selected, each role collection's own org (exactly as specified in
the file) is used. Either way, each role reference's roleTemplateAppId is
automatically re-resolved against an existing role in the target org,
matching by app name (ignoring the subaccount-specific suffix after '!'),
roleTemplateName, and role_name.

If a referenced role collection does not exist (HTTP 404), it is skipped
with a warning — use 'bo create-role-collections' to create it first.

Without -y, a TOON preview of all role collections to be updated is shown
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
			"Role collections to be updated:",
			"Proceed with role collection update? [y/N] ",
			"Updating role collections...",
			"~",
			func(ctx context.Context, apiURL, token string, rc xsuaa.RoleCollection) error {
				return xsuaa.UpdateRoleCollection(ctx, apiURL, token, rc)
			})
	},
}

func init() {
	updateRoleCollectionsCmd.GroupID = "xsuaa"
	rootCmd.AddCommand(updateRoleCollectionsCmd)
	updateRoleCollectionsCmd.Flags().String("role-collections", "", "Path to a role-collections JSON/TOON file produced by 'bo role-collections' (required)")
	updateRoleCollectionsCmd.MarkFlagRequired("role-collections") //nolint:errcheck
	updateRoleCollectionsCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
}
