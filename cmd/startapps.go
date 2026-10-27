package cmd

import (
	"context"

	"btp-open-cli/internal/cf"

	"github.com/spf13/cobra"
)

var startAppsCmd = &cobra.Command{
	Use:   "start-apps <app_name_patterns>",
	Short: "Start apps matching name patterns, across orgs and spaces",
	Long: `Start every app whose name matches <app_name_patterns> — a comma-separated
list of patterns, e.g. 'web-*,worker-*' or '*' (matches every app name).
Each pattern is a glob (via filepath.Match) if it contains * ? [, otherwise
a case-insensitive substring. A pattern prefixed with '!' reverses that
pattern: '!web-*' selects every app name that does NOT match 'web-*'. An
app is selected if any pattern in the list — negated or not — matches it.

If neither --org nor --orgs is given, the default org scope selected via
'bo orgs' is used. --spaces/--space narrows to specific space names
(comma-separated, exact, case-insensitive) within each target org; if
omitted, every accessible space in scope is searched.

Without -y, every matched app is listed in a TOON preview (org -> space ->
apps, including each app's current state) and a "Proceed? [y/N]"
confirmation is required — declining aborts the whole command without
starting anything. -y skips this confirmation.

If --regions is omitted the regions from the last login are used.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStartStopApps(cmd, args, "start", "starting", "started",
			func(c *cf.Client, ctx context.Context, appGUID string) (*cf.App, error) {
				return c.StartApp(ctx, appGUID)
			})
	},
}

func init() {
	startAppsCmd.GroupID = "cf-org"
	rootCmd.AddCommand(startAppsCmd)
	startAppsCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	startAppsCmd.Flags().String("org", "", "Org GUID to target; only this org will be targeted")
	startAppsCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	startAppsCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,org_id,org_name)")
	startAppsCmd.Flags().String("spaces", "", "Only include these comma-separated, exact (case-insensitive) space names; default is every accessible space in scope")
	startAppsCmd.Flags().String("space", "", "Equivalent to --spaces with a single name (combined with --spaces if both are given)")
	startAppsCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
}
