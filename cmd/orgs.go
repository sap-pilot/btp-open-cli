package cmd

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	toonenc "github.com/toon-format/toon-go"

	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

// ── output types ──────────────────────────────────────────────────────────────

// Field order matches the interactive picker's display order (region, then
// org name, then org id) and is preserved by the TOON/JSON encoders.
type orgsOutOrg struct {
	Name string `json:"org_name" toon:"org_name"`
	ID   string `json:"org_id"   toon:"org_id"`
}

type orgsOutRegion struct {
	ID   string       `json:"region" toon:"region"`
	Orgs []orgsOutOrg `json:"orgs"   toon:"orgs"`
}

type orgsOutDoc struct {
	Regions []orgsOutRegion `json:"regions" toon:"regions"`
}

// buildOrgsOutDoc groups the selected orgs by region, in selection order.
func buildOrgsOutDoc(selected []orgChoice) orgsOutDoc {
	var doc orgsOutDoc
	order := make([]string, 0)
	byRegion := make(map[string][]orgsOutOrg)
	seen := make(map[string]bool)
	for _, c := range selected {
		if !seen[c.Region] {
			order = append(order, c.Region)
			seen[c.Region] = true
		}
		byRegion[c.Region] = append(byRegion[c.Region], orgsOutOrg{ID: c.ID, Name: c.Name})
	}
	for _, r := range order {
		doc.Regions = append(doc.Regions, orgsOutRegion{ID: r, Orgs: byRegion[r]})
	}
	return doc
}

// ── command ───────────────────────────────────────────────────────────────────

var orgsCmd = &cobra.Command{
	Use:   "orgs",
	Short: "Select a default set of orgs to scope other commands",
	Long: `Interactively select which accessible CF orgs should be the default scope for
org-aware commands (org-users, org-space-users, apps, users, role-collections,
subaccount-destinations and its create/update/delete variants, and
describe-subaccount) when they are run without --org or --orgs.

Each org is numbered (01, 02, ... — width grows to 3 digits past 99 orgs).
Controls: up/down to move the highlight, space to toggle the highlighted org,
type an org's number to jump to and toggle it directly, 'a' to select every
org, 'c' to clear the selection, and enter to confirm — or just confirm the
highlighted org if nothing was checked. Pass --all to select every matching
org (after --include/--exclude filtering) without prompting.

Use --include/--exclude to narrow which orgs appear in the picker (or get
auto-selected with --all): each accepts a comma-separated list of keywords,
and an org matches if org_name contains any of them (case-insensitive).

The selection is saved to ~/.bo/credentials.json and applies only to the
current login session: running 'bo login' or 'bo logoff' clears it, and you
will be prompted to run 'bo orgs' again (or pass --org/--orgs) the next time
you run an org-aware command without an explicit scope.

Output formats (--format), showing the orgs just selected, columns/fields
ordered region, org_name, org_id to match the picker:
  toon  Token-Oriented Object Notation — compact, human-readable (default)
  json  JSON document
  csv   CSV rows: region,org_name,org_id — directly reusable as any other
        command's --orgs/--excludeOrgs file (column order is not significant
        when read back — only the column names are)

Use --output/-o to write this listing to a file instead of stdout — shell '>'
redirection won't work here since the interactive picker also writes to
stdout.

If --regions is omitted the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		format, _ := cmd.Flags().GetString("format")
		includePattern, _ := cmd.Flags().GetString("include")
		excludePattern, _ := cmd.Flags().GetString("exclude")
		allOrgs, _ := cmd.Flags().GetBool("all")
		outputFile, _ := cmd.Flags().GetString("output")

		creds, err := store.Load()
		if err != nil {
			return fmt.Errorf("not logged in — run: bo login --regions <region>")
		}

		var apiURLs []string
		if regionsFlag != "" {
			for _, r := range splitCSV(regionsFlag) {
				apiURLs = append(apiURLs, store.RegionToAPIURL(r))
			}
		} else {
			apiURLs = creds.ActiveAPIURLs
		}
		if len(apiURLs) == 0 {
			return fmt.Errorf("no regions configured — run: bo login --regions <region1,region2>")
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		choices, err := listAccessibleOrgs(ctx, creds, apiURLs)
		if err != nil {
			return err
		}

		var candidates []orgChoice
		for _, c := range choices {
			if includePattern != "" && !skipMatches(includePattern, c.Name) {
				continue
			}
			if excludePattern != "" && skipMatches(excludePattern, c.Name) {
				continue
			}
			candidates = append(candidates, c)
		}
		if len(candidates) == 0 {
			return fmt.Errorf("no orgs match the given --include/--exclude filters")
		}

		var selected []orgChoice
		if allOrgs {
			selected = candidates
		} else {
			selected, err = selectOrgsInteractive(ctx, candidates)
			if err != nil {
				return err
			}
		}

		scope := make([]store.OrgScopeRef, len(selected))
		for i, c := range selected {
			scope[i] = store.OrgScopeRef{Region: c.Region, ID: c.ID, Name: c.Name, APIURL: c.APIURL}
		}
		creds.DefaultOrgScope = scope
		if err := store.Save(creds); err != nil {
			return fmt.Errorf("saving default org scope: %w", err)
		}

		out, closeOut, err := resolveOutputWriter(outputFile)
		if err != nil {
			return err
		}
		defer closeOut()

		doc := buildOrgsOutDoc(selected)
		switch strings.ToLower(format) {
		case "json":
			err = writeOrgsJSON(out, doc)
		case "csv":
			err = writeOrgsCSV(out, doc)
		default: // "toon"
			err = writeOrgsToon(out, doc)
		}
		if err != nil {
			return err
		}

		fmt.Fprintf(os.Stderr, "\nDefault org scope set to %d org(s) for this login session.\n", len(selected))
		return nil
	},
}

func writeOrgsToon(w io.Writer, doc orgsOutDoc) error {
	out, err := toonenc.Marshal(doc, toonenc.WithIndent(2))
	if err != nil {
		return fmt.Errorf("encoding TOON: %w", err)
	}
	if _, err = w.Write(out); err != nil {
		return err
	}
	_, err = fmt.Fprintln(w)
	return err
}

func writeOrgsJSON(w io.Writer, doc orgsOutDoc) error {
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding JSON: %w", err)
	}
	fmt.Fprintln(w, string(out))
	return nil
}

// writeOrgsCSV writes the flat CSV (region,org_name,org_id) that is
// compatible with the --orgs / --excludeOrgs flags of other commands — those
// flags identify columns by name, so this order (matching the interactive
// picker) round-trips regardless of position.
func writeOrgsCSV(w io.Writer, doc orgsOutDoc) error {
	csvW := csv.NewWriter(w)
	defer csvW.Flush()
	if err := csvW.Write([]string{"region", "org_name", "org_id"}); err != nil {
		return err
	}
	for _, r := range doc.Regions {
		for _, o := range r.Orgs {
			if err := csvW.Write([]string{r.ID, o.Name, o.ID}); err != nil {
				return err
			}
		}
	}
	return nil
}

func init() {
	orgsCmd.GroupID = "common"
	rootCmd.AddCommand(orgsCmd)
	orgsCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	orgsCmd.Flags().String("format", "toon", "Output format: toon (default), json, or csv")
	orgsCmd.Flags().String("include", "", "Only show/select orgs whose org_name contains any of these comma-separated, case-insensitive keywords")
	orgsCmd.Flags().String("exclude", "", "Exclude orgs whose org_name contains any of these comma-separated, case-insensitive keywords")
	orgsCmd.Flags().Bool("all", false, "Select every matching org without prompting for interactive selection")
	orgsCmd.Flags().StringP("output", "o", "", "Write the selected orgs listing to this file instead of stdout")
}
