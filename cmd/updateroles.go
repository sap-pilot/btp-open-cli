package cmd

import (
	"context"

	"btp-open-cli/internal/xsuaa"

	"github.com/spf13/cobra"
)

var updateRolesCmd = &cobra.Command{
	Use:   "update-roles",
	Short: "Update XSUAA roles from an exported roles file",
	Long: `Update roles (description and attributeList) in the XSUAA (Authorization and
Trust Management) apiaccess service, read from --roles <file> — a JSON or TOON
document produced by 'bo roles' (or hand-authored with the same
regions -> orgs -> roles shape). Each role is identified by the combination
of roleTemplateAppId, roleTemplateName, and role_name, and is matched by
those fields against the existing role on the server.

For each org referenced in the file an XSUAA token is resolved the same way
as 'bo roles'/'bo users' (via the xsuaa/apiaccess service key) — only the
orgs that actually appear in --roles are touched.

If a referenced role does not exist (HTTP 404), it is skipped with a
warning — use 'bo create-roles' to create it first.

Without -y, a TOON preview of all roles to be updated is shown and
confirmation is required before any changes are made.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRolesFileApply(cmd,
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
