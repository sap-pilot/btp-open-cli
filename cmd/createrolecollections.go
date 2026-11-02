package cmd

import (
	"context"

	"btp-open-cli/internal/xsuaa"

	"github.com/spf13/cobra"
)

var createRoleCollectionsCmd = &cobra.Command{
	Use:   "create-role-collections",
	Short: "Create XSUAA role collections from an exported role-collections file",
	Long: `Create role collections (with their role references) in the XSUAA
(Authorization and Trust Management) apiaccess service, read from
--role-collections <file> — a JSON or TOON document produced by
'bo role-collections' (or hand-authored with the same
regions -> orgs -> roleCollections shape; its "roles" field, if present, is
ignored).

For each org referenced in the file an XSUAA token is resolved the same way
as 'bo role-collections'/'bo users' (via the xsuaa/apiaccess service key) —
only the orgs that actually appear in --role-collections are touched.

If a role collection with the same name already exists (HTTP 409), creation
is skipped for it.

Without -y, a TOON preview of all role collections to be created is shown
and confirmation is required before any changes are made.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRoleCollectionsFileApply(cmd,
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
