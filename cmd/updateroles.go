package cmd

import (
	"context"
	"fmt"
	"strings"

	"btp-open-cli/internal/xsuaa"

	"github.com/spf13/cobra"
)

var updateRolesCmd = &cobra.Command{
	Use:   "update-roles [role_name_pattern]",
	Short: "Update XSUAA roles from an exported roles file",
	Long: `Update roles (description and attributeList) in the XSUAA (Authorization and
Trust Management) apiaccess service, read from --roles <file> — a JSON or TOON
document produced by 'bo roles' (or hand-authored with the same
regions -> orgs -> roles shape). Each role is identified by the combination
of roleTemplateAppId, roleTemplateName, and role_name, and is matched by
those fields against the existing role on the server.

<role_name_pattern> is optional — a comma-separated list of patterns, e.g.
'Custom_*,temp-*' or '*' (the default when omitted: every role in the file).
Each pattern is a glob (via filepath.Match) if it contains * ? [, otherwise
a case-insensitive substring; a pattern prefixed with '!' reverses it, and a
role is selected if any pattern — negated or not — matches it. Only roles
in --roles matching the pattern are updated; the rest of the file is
ignored.

For each org referenced in the (filtered) file an XSUAA token is resolved
the same way as 'bo roles'/'bo users' (via the xsuaa/apiaccess service key)
— only the orgs that actually appear are touched.

The orgs actually targeted depend on whether a default org scope is
currently selected via 'bo orgs': if one is, every role in the file is
updated within those org(s) instead of the file's own — discarding the
file's own org assignment entirely. If no default org scope is selected,
each role's own org (exactly as specified in the file) is used. Either
way, each role's roleTemplateAppId is automatically re-resolved against
the target org's own role templates, matching by app name (ignoring the
subaccount-specific suffix after '!') and roleTemplateName.

If a referenced role does not exist (HTTP 404), it is skipped with a
warning — use 'bo create-roles' to create it first.

Without -y, a TOON preview of all roles to be updated is shown and
confirmation is required before any changes are made.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		pattern := "*"
		if len(args) > 0 {
			pattern = strings.TrimSpace(args[0])
			if pattern == "" {
				return fmt.Errorf("role_name_pattern cannot be empty")
			}
		}
		return runRolesFileApply(cmd, pattern,
			"Roles to be updated:",
			"Proceed with role update? [y/N] ",
			"Updating roles...",
			"~",
			func(ctx context.Context, apiURL, token string, role xsuaa.Role) error {
				return xsuaa.UpdateRole(ctx, apiURL, token, role)
			})
	},
}

func init() {
	updateRolesCmd.GroupID = "xsuaa"
	rootCmd.AddCommand(updateRolesCmd)
	updateRolesCmd.Flags().String("roles", "", "Path to a roles JSON/TOON file produced by 'bo roles' (required)")
	updateRolesCmd.MarkFlagRequired("roles") //nolint:errcheck
	updateRolesCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
}
