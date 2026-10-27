package cmd

import (
	"context"

	"btp-open-cli/internal/cf"

	"github.com/spf13/cobra"
)

var restageAppsCmd = &cobra.Command{
	Use:   "restage-apps <app_name_patterns>",
	Short: "Restage apps matching name patterns, across orgs and spaces",
	Long: `Restage every app whose name matches <app_name_patterns> — a comma-separated
list of patterns, e.g. 'web-*,worker-*' or '*' (matches every app name).
Each pattern is a glob (via filepath.Match) if it contains * ? [, otherwise
a case-insensitive substring. A pattern prefixed with '!' reverses that
pattern: '!web-*' selects every app name that does NOT match 'web-*'. An
app is selected if any pattern in the list — negated or not — matches it.

By default a restage rebuilds a droplet from the app's current package and
redeploys it via CF's classic "restage" action, causing downtime. Pass
--strategy rolling or --strategy canary to build the new droplet the same
way but roll it out with zero downtime instead, via a CF v3 Deployment.
--strategy null (the default — equivalent to omitting the flag) keeps the
classic, downtime-causing restage.

If neither --org nor --orgs is given, the default org scope selected via
'bo orgs' is used. --spaces/--space narrows to specific space names
(comma-separated, exact, case-insensitive) within each target org; if
omitted, every accessible space in scope is searched.

Without -y, every matched app is listed in a TOON preview (org -> space ->
apps, including each app's current state) and a "Proceed? [y/N]"
confirmation is required — declining aborts the whole command without
restaging anything. -y skips this confirmation.

If --regions is omitted the regions from the last login are used.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		strategy, err := parseAppStrategy(cmd)
		if err != nil {
			return err
		}
		return runStartStopApps(cmd, args, "restage", "restaging", "restaged",
			func(c *cf.Client, ctx context.Context, appGUID string) (*cf.App, error) {
				if strategy == "" {
					return c.RestageApp(ctx, appGUID)
				}
				return nil, c.RestageAppWithStrategy(ctx, appGUID, strategy)
			})
	},
}

func init() {
	restageAppsCmd.GroupID = "cf-org"
	rootCmd.AddCommand(restageAppsCmd)
	restageAppsCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	restageAppsCmd.Flags().String("org", "", "Org GUID to target; only this org will be targeted")
	restageAppsCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	restageAppsCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,org_id,org_name)")
	restageAppsCmd.Flags().String("spaces", "", "Only include these comma-separated, exact (case-insensitive) space names; default is every accessible space in scope")
	restageAppsCmd.Flags().String("space", "", "Equivalent to --spaces with a single name (combined with --spaces if both are given)")
	restageAppsCmd.Flags().String("strategy", "null", "Deployment strategy for a zero-downtime restage: canary, rolling, or null (default — classic, downtime-causing restage)")
	restageAppsCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
}
