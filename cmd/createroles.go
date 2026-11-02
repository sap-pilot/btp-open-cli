package cmd

import (
	"context"

	"btp-open-cli/internal/xsuaa"

	"github.com/spf13/cobra"
)

var createRolesCmd = &cobra.Command{
	Use:   "create-roles",
	Short: "Create XSUAA roles from an exported roles file",
	Long: `Create roles in the XSUAA (Authorization and Trust Management) apiaccess
service, read from --roles <file> — a JSON or TOON document produced by
'bo roles' (or hand-authored with the same regions -> orgs -> roles shape).
Each role entry carries its roleTemplateAppId, roleTemplateName, role_name,
description, and optional attributeList.

For each org referenced in the file an XSUAA token is resolved the same way
as 'bo roles'/'bo users' (via the xsuaa/apiaccess service key) — only the
orgs that actually appear in --roles are touched.

If a role with the same identity already exists (HTTP 409), creation is
skipped for that role.

Without -y, a TOON preview of all roles to be created is shown and
confirmation is required before any changes are made.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRolesFileApply(cmd,
			"Roles to be created:",
			"Proceed with role creation? [y/N] ",
			"Creating roles...",
			"+",
			func(ctx context.Context, apiURL, token string, role xsuaa.Role) error {
				return xsuaa.CreateRole(ctx, apiURL, token, role)
			})
	},
}

func init() {
	createRolesCmd.GroupID = "xsuaa"
	rootCmd.AddCommand(createRolesCmd)
	createRolesCmd.Flags().String("roles", "", "Path to a roles JSON/TOON file produced by 'bo roles' (required)")
	createRolesCmd.MarkFlagRequired("roles") //nolint:errcheck
	createRolesCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
}
