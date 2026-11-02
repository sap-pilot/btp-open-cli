package cmd

import (
	"context"

	"btp-open-cli/internal/cf"

	"github.com/spf13/cobra"
)

var stopAppsCmd = &cobra.Command{
	Use:   "stop-apps [app_name_patterns]",
	Short: "Stop apps matching name patterns, across orgs and spaces",
	Long: `Stop every app whose name matches <app_name_patterns> — a comma-separated
list of patterns, e.g. 'web-*,worker-*' or '*' (matches every app name).
Each pattern is a glob (via filepath.Match) if it contains * ? [, otherwise
a case-insensitive substring. A pattern prefixed with '!' reverses that
pattern: '!web-*' selects every app name that does NOT match 'web-*'. An
app is selected if any pattern in the list — negated or not — matches it.

If neither --org nor --orgs is given, the default org scope selected via
'bo orgs' is used. --spaces/--space narrows to specific space names
(comma-separated, exact, case-insensitive) within each target org; if
omitted, every accessible space in scope is searched.

Pass --apps <file> to target specific apps directly instead, skipping all
org/space discovery: <file> is a CSV with the columns region_id, org_name,
space_id, space_name, app_id, app_name — exactly what
'bo apps --format csv' writes, so the straightforward way to build one is
'bo apps --include STARTED --format csv -o apps.csv' (any --include/
--exclude filter works) and reuse its output here. When --apps is given,
<app_name_patterns> becomes optional (default '*', every CSV row) and
still further narrows the CSV's rows by app_name; --org, --orgs,
--excludeOrgs, --spaces, --space, --regions, and the default org scope
selected via 'bo orgs' are all ignored, since each CSV row already names
its own exact region/org/space/app.

Without -y, every matched app is listed in a TOON preview (org -> space ->
apps, including each app's current state) and a "Proceed? [y/N]"
confirmation is required — declining aborts the whole command without
stopping anything. -y skips this confirmation.

If --regions is omitted the regions from the last login are used.`,
	Args: sawAppPatternArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStartStopApps(cmd, args, "stop", "stopping", "stopped",
			func(c *cf.Client, ctx context.Context, appGUID string) (*cf.App, error) {
				return c.StopApp(ctx, appGUID)
			})
	},
}

func init() {
	stopAppsCmd.GroupID = "cf-org"
	rootCmd.AddCommand(stopAppsCmd)
	stopAppsCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	stopAppsCmd.Flags().String("org", "", "Org GUID to target; only this org will be targeted")
	stopAppsCmd.Flags().String("orgs", "", "Path to CSV of orgs to include (columns: region,org_id,org_name)")
	stopAppsCmd.Flags().String("excludeOrgs", "", "Path to CSV of orgs to exclude (columns: region,org_id,org_name)")
	stopAppsCmd.Flags().String("spaces", "", "Only include these comma-separated, exact (case-insensitive) space names; default is every accessible space in scope")
	stopAppsCmd.Flags().String("space", "", "Equivalent to --spaces with a single name (combined with --spaces if both are given)")
	stopAppsCmd.Flags().String("apps", "", "Path to CSV of apps to target directly (columns: region_id,org_name,space_id,space_name,app_id,app_name — as produced by 'bo apps --format csv'); bypasses --org/--orgs/--excludeOrgs/--spaces/--space/--regions and the 'bo orgs' default scope")
	stopAppsCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
}
