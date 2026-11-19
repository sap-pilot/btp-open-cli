package cmd

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"

	toonenc "github.com/toon-format/toon-go"

	"btp-open-cli/internal/destination"
	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

// destinationsLeadingCols are the 'destinations' command's own structural
// columns (see destFlatRow): org, space, and destination-service-instance
// identity. A subaccount-level row leaves space_id/space_name/instance_id/
// instance_name empty, since it isn't tied to any one space or instance.
var destinationsLeadingCols = []string{"org_id", "org_name", "space_id", "space_name", "instance_id", "instance_name"}

var destinationsCmd = &cobra.Command{
	Use:   "destinations",
	Short: "List both subaccount-level and instance-level destinations for the target orgs",
	Long: `Retrieves both subaccount-level and instance-level destinations for one or
more target orgs (--org GUID or name, or --orgs): subaccount-level via any
destination service instance found in the org, and instance-level from
every destination service instance in every space of the org (or just the
--spaces given). This is the combined view 'subaccount-destinations' and
'space-destinations' each give separately.

Every destination property is included exactly as the destination service
API responds — nothing is redacted, including sensitive fields such as
Password, ClientSecret, and ProxyPassword; this is sensitive output, see
README.

The result is a flat list, one row per destination: org_id, org_name,
space_id, space_name, instance_id, and instance_name lead (space_id,
space_name, instance_id, and instance_name are empty for a subaccount-level
destination, since it isn't tied to any one space or instance), followed —
for --format csv — by every property key that appears anywhere in the
result set, sorted alphabetically, so the column set reflects whatever
destinations were actually returned. json/toon output the same flat
{"rows": [...]} list, each row as an object carrying only its own
properties (plus the leading columns).

Use --include/--exclude to narrow results: each accepts a comma-separated
list of keywords, matched against any destination property key or value —
as a glob pattern (e.g. API*PP) if a keyword contains * ? [, otherwise as a
case-insensitive substring.

Use --fields <column,...> (comma-separated, in the order given — any of
the leading columns above, or any destination property name exactly as the
API casing has it, e.g. "Name" not "name") to limit output to just those
columns instead. Omit it to get every column.

Use --sort-by <column,...> to sort rows by one or more of those same
columns (any name accepted, matched the same way), each optionally
suffixed with ":desc" for descending (":asc" is the default). Omit it to
keep the default order: sorted by org_name, then space_name (subaccount-
level rows sort first, having an empty space_name), then Name.

If neither --org nor --orgs is given, the default org scope selected via
'bo orgs' is used. --spaces narrows instance-level destinations to specific
space names or GUIDs (comma-separated, case-insensitive) within each
target org; omit it to scan every accessible space. --spaces has no effect
on subaccount-level destinations, which aren't tied to any one space.

Use --output/-o to write the result to a file instead of stdout. If --format
is not explicitly given, it is auto-detected from --output's extension:
.json selects json, .csv selects csv, .toon selects toon — otherwise the
default toon applies. Pass --format explicitly to override the extension.

If --regions is omitted the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		orgFlag, _ := cmd.Flags().GetString("org")
		orgsFile, _ := cmd.Flags().GetString("orgs")
		regionsFlag, _ := cmd.Flags().GetString("regions")
		spacesFlag, _ := cmd.Flags().GetString("spaces")
		format, _ := cmd.Flags().GetString("format")
		includePattern, _ := cmd.Flags().GetString("include")
		excludePattern, _ := cmd.Flags().GetString("exclude")
		noPrompt, _ := cmd.Flags().GetBool("no-prompt")
		outputFile, _ := cmd.Flags().GetString("output")
		if !cmd.Flags().Changed("format") {
			if auto := autoFormatFromOutputExt(outputFile, map[string]string{"json": ".json", "csv": ".csv", "toon": ".toon"}); auto != "" {
				format = auto
			}
		}
		fieldsFlag, _ := cmd.Flags().GetString("fields")
		fieldIDs := destParseFields(fieldsFlag)
		sortByFlag, _ := cmd.Flags().GetString("sort-by")
		sortKeys, err := destParseSortBy(sortByFlag, "--sort-by")
		if err != nil {
			return err
		}

		creds, err := store.Load()
		if err != nil {
			return fmt.Errorf("not logged in — run: bo login --regions <region>")
		}
		apiURLs := activeAPIURLs(creds, regionsFlag)
		if len(apiURLs) == 0 {
			return fmt.Errorf("no regions configured — run: bo login --regions <region1,region2>")
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		orgTargets, err := resolveOrgTargets(creds, orgFlag, orgsFile)
		if err != nil {
			return err
		}

		var spacesFilter map[string]bool
		if spacesFlag != "" {
			spacesFilter = make(map[string]bool)
			for _, s := range splitCSV(spacesFlag) {
				spacesFilter[strings.ToLower(s)] = true
			}
		}

		var rows []destFlatRow
		for _, target := range orgTargets {
			_, orgGUID, orgName, _, spaces, findErr := findOrgAndSpaces(ctx, cmd, target, creds, apiURLs)
			if findErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", findErr)
				continue
			}

			// Subaccount-level destinations.
			_, _, destClient, resolveErr := resolveOrgDestClient(ctx, cmd, target, creds, apiURLs, noPrompt)
			if resolveErr != nil {
				if errors.Is(resolveErr, errAborted) {
					return errAborted
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", resolveErr)
			} else {
				rawDests, fetchErr := destination.ListSubaccountDestinationsFull(ctx, destClient.URI, destClient.Token)
				if fetchErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s] listing subaccount destinations: %v\n", orgName, fetchErr)
				} else {
					for _, raw := range rawDests {
						if includePattern != "" && !sdMatchesAnyKeyword(includePattern, raw) {
							continue
						}
						if excludePattern != "" && sdMatchesAnyKeyword(excludePattern, raw) {
							continue
						}
						rows = append(rows, destFlatRow{OrgID: orgGUID, OrgName: orgName, Props: raw})
					}
				}
			}

			// Instance-level destinations, per space.
			for _, sp := range spaces {
				if !matchesSpaceFilter(spacesFilter, sp) {
					continue
				}
				_, clients, spErr := resolveSpaceDestClients(ctx, cmd, sp.GUID, creds, apiURLs)
				if spErr != nil {
					if errors.Is(spErr, errAborted) {
						return errAborted
					}
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s/%s] %v\n", orgName, sp.Name, spErr)
					continue
				}
				for _, c := range clients {
					rawDests, fetchErr := destination.ListInstanceDestinationsFull(ctx, c.URI, c.Token)
					if fetchErr != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: [%s/%s] instance %q: listing destinations: %v\n",
							orgName, sp.Name, c.InstanceName, fetchErr)
						continue
					}
					for _, raw := range rawDests {
						if includePattern != "" && !sdMatchesAnyKeyword(includePattern, raw) {
							continue
						}
						if excludePattern != "" && sdMatchesAnyKeyword(excludePattern, raw) {
							continue
						}
						rows = append(rows, destFlatRow{
							OrgID: orgGUID, OrgName: orgName, SpaceID: sp.GUID, SpaceName: sp.Name,
							InstanceID: c.InstanceGUID, InstanceName: c.InstanceName, Props: raw,
						})
					}
				}
			}
		}

		if len(sortKeys) > 0 {
			destSortRowsBy(rows, sortKeys)
		} else {
			sort.SliceStable(rows, func(i, j int) bool {
				if rows[i].OrgName != rows[j].OrgName {
					return rows[i].OrgName < rows[j].OrgName
				}
				if rows[i].SpaceName != rows[j].SpaceName {
					return rows[i].SpaceName < rows[j].SpaceName
				}
				return rows[i].Props["Name"] < rows[j].Props["Name"]
			})
		}

		out, closeOut, err := resolveOutputWriter(outputFile)
		if err != nil {
			return err
		}
		defer closeOut()

		switch strings.ToLower(format) {
		case "json":
			outB, jsonErr := json.MarshalIndent(flatRowsDoc{Rows: destRowMaps(rows, fieldIDs, destinationsLeadingCols)}, "", "  ")
			if jsonErr != nil {
				return fmt.Errorf("encoding JSON: %w", jsonErr)
			}
			fmt.Fprintln(out, string(outB))
			return nil
		case "csv":
			csvW := csv.NewWriter(out)
			defer csvW.Flush()
			return destWriteCSV(csvW, rows, fieldIDs, destinationsLeadingCols)
		default: // toon
			outB, toonErr := toonenc.Marshal(flatRowsDoc{Rows: destRowMaps(rows, fieldIDs, destinationsLeadingCols)}, toonenc.WithIndent(2))
			if toonErr != nil {
				return fmt.Errorf("encoding TOON: %w", toonErr)
			}
			fmt.Fprintln(out, string(outB))
			return nil
		}
	},
}

func init() {
	destinationsCmd.GroupID = "destination"
	rootCmd.AddCommand(destinationsCmd)
	destinationsCmd.Flags().String("org", "", "Org GUID or name substring to target")
	destinationsCmd.Flags().String("orgs", "", "Path to CSV of orgs to target (columns: region,org_id,org_name)")
	destinationsCmd.Flags().String("regions", "", "Comma-separated CF regions to search (default: last login regions)")
	destinationsCmd.Flags().String("spaces", "", "Comma-separated space names or GUIDs to narrow instance-level destinations down to (default: every space in each target org); has no effect on subaccount-level destinations")
	destinationsCmd.Flags().String("format", "toon", "Output format: toon (default), json, or csv; auto-detected from --output's extension (.json, .csv, or .toon) when --format is not explicitly given")
	destinationsCmd.Flags().String("include", "", "Only include destinations where any property contains any of these comma-separated keywords (substring, or glob if a keyword has * ? [)")
	destinationsCmd.Flags().String("exclude", "", "Exclude destinations where any property contains any of these comma-separated keywords (substring, or glob if a keyword has * ? [)")
	destinationsCmd.Flags().Bool("no-prompt", false, "Skip interactive prompts — skip instances with no service key")
	destinationsCmd.Flags().StringP("output", "o", "", "Write output to this file instead of stdout")
	destinationsCmd.Flags().String("fields", "", "Comma-separated column names to limit output to, in the order given (org_id, org_name, space_id, space_name, instance_id, instance_name, or any destination property name exactly as the API casing has it)")
	destinationsCmd.Flags().String("sort-by", "", "Comma-separated column names to sort by, each optionally suffixed with \":desc\" (default \":asc\")")
}
