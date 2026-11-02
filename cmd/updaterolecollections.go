package cmd

import (
	"context"

	"btp-open-cli/internal/xsuaa"

	"github.com/spf13/cobra"
)

var updateRoleCollectionsCmd = &cobra.Command{
	Use:   "update-role-collections",
	Short: "Update XSUAA role collections from an exported role-collections file",
	Long: `Update role collections in the XSUAA (Authorization and Trust Management)
apiaccess service, read from --role-collections <file> — a JSON or TOON
document produced by 'bo role-collections' (or hand-authored with the same
regions -> orgs -> roleCollections shape; its "roles" field, if present, is
ignored). Each role collection is identified by name, and the update fully
replaces its description and roleReferences with what the file specifies —
a role reference present on the server but absent from the file's
roleReferences list is removed.

For each org referenced in the file an XSUAA token is resolved the same way
as 'bo role-collections'/'bo users' (via the xsuaa/apiaccess service key) —
only the orgs that actually appear in --role-collections are touched.

If a referenced role collection does not exist (HTTP 404), it is skipped
with a warning — use 'bo create-role-collections' to create it first.

Without -y, a TOON preview of all role collections to be updated is shown
and confirmation is required before any changes are made.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRoleCollectionsFileApply(cmd,
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
