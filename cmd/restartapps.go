package cmd

import (
	"context"

	"btp-open-cli/internal/cf"

	"github.com/spf13/cobra"
)

var restartAppsCmd = &cobra.Command{
	Use:   "restart-apps <app_name_patterns>",
	Short: "Restart apps matching name patterns, across orgs and spaces",
	Long: `Restart every app whose name matches <app_name_patterns> — a comma-separated
list of patterns, e.g. 'web-*,worker-*' or '*' (matches every app name).
Each pattern is a glob (via filepath.Match) if it contains * ? [, otherwise
a case-insensitive substring. A pattern prefixed with '!' reverses that
pattern: '!web-*' selects every app name that does NOT match 'web-*'. An
app is selected if any pattern in the list — negated or not — matches it.

By default a restart is an immediate stop+start and causes downtime. Pass
--strategy rolling or --strategy canary to restart with zero downtime
instead, via a CF v3 Deployment of the app's current droplet.
--strategy null (the default — equivalent to omitting the flag) keeps the
classic, downtime-causing restart.

If neither --org nor --orgs is given, the default org scope selected via
'bo orgs' is used. --spaces/--space narrows to specific space names
(comma-separated, exact, case-insensitive) within each target org; if
omitted, every accessible space in scope is searched.

Without -y, every matched app is listed in a TOON preview (org -> space ->
apps, including each app's current state) and a "Proceed? [y/N]"
confirmation is required — declining aborts the whole command without
restarting anything. -y skips this confirmation.

If --regions is omitted the regions from the last login are used.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		strategy, err := parseAppStrategy(cmd)
		if err != nil {
			return err
		}
		return runStartStopApps(cmd, args, "restart", "restarting", "restarted",
			func(c *cf.Client, ctx context.Context, appGUID string) (*cf.App, error) {
				if strategy == "" {
					return c.RestartApp(ctx, appGUID)
				}
				_, err := c.CreateDeployment(ctx, appGUID, strategy, "")
				return nil, err
			})
	},
}

func init() {
	restartAppsCmd.GroupID = "cf-org"
	rootCmd.AddCommand(restartAppsCmd)
	restartAppsCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	restartAppsCmd.Flags().String("org", "", "Org GUID to target; only this org will be targeted")
	restartAppsCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	restartAppsCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,org_id,org_name)")
	restartAppsCmd.Flags().String("spaces", "", "Only include these comma-separated, exact (case-insensitive) space names; default is every accessible space in scope")
	restartAppsCmd.Flags().String("space", "", "Equivalent to --spaces with a single name (combined with --spaces if both are given)")
	restartAppsCmd.Flags().String("strategy", "null", "Deployment strategy for a zero-downtime restart: canary, rolling, or null (default — classic, downtime-causing restart)")
	restartAppsCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
}
