package cmd

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"

	toonenc "github.com/toon-format/toon-go"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

// ── output types ──────────────────────────────────────────────────────────────

type orgsOutOrg struct {
	ID   string `json:"org_id"   toon:"org_id"`
	Name string `json:"org_name" toon:"org_name"`
}

type orgsOutRegion struct {
	ID   string       `json:"region" toon:"region"`
	Orgs []orgsOutOrg `json:"orgs"   toon:"orgs"`
}

type orgsOutDoc struct {
	Regions []orgsOutRegion `json:"regions" toon:"regions"`
}

// ── command ───────────────────────────────────────────────────────────────────

var orgsCmd = &cobra.Command{
	Use:   "orgs",
	Short: "List all accessible CF organizations across one or more regions",
	Long: `List all Cloud Foundry organizations the authenticated user can access.

Output formats (--format):
  toon  Token-Oriented Object Notation — compact, human-readable (default)
  json  JSON document
  csv   CSV rows: region,org_id,org_name

The csv format is compatible with the --orgs and --excludeOrgs flags accepted
by create-org-space-users, delete-org-space-users, org-users, org-space-users,
apps, users, delete-users, and role-collections.

If --regions is omitted, the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		format, _ := cmd.Flags().GetString("format")

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

		type regionResult struct {
			region string
			orgs   []cf.Organization
			err    error
		}
		results := make([]regionResult, len(apiURLs))
		var wg sync.WaitGroup

		for i, apiURL := range apiURLs {
			wg.Add(1)
			go func(idx int, url string) {
				defer wg.Done()
				regionName := store.APIURLToRegion(url)
				tok, ok := creds.Tokens[url]
				if !ok {
					results[idx] = regionResult{region: regionName,
						err: fmt.Errorf("no token — run: bo login --regions %s", regionName)}
					return
				}
				client := cf.NewClient(url, tok.AccessToken)
				client.SetTokenRefresher(makeTokenRefresher(url, tok.AccessToken))
				orgs, err := client.ListOrganizations(ctx)
				results[idx] = regionResult{region: regionName, orgs: orgs, err: err}
			}(i, apiURL)
		}
		wg.Wait()

		// Assemble output document (preserves region order from apiURLs).
		var doc orgsOutDoc
		for _, r := range results {
			if r.err != nil {
				fmt.Fprintf(os.Stderr, "[%s] error: %v\n", r.region, r.err)
				continue
			}
			outOrgs := make([]orgsOutOrg, 0, len(r.orgs))
			for _, o := range r.orgs {
				outOrgs = append(outOrgs, orgsOutOrg{ID: o.GUID, Name: o.Name})
			}
			doc.Regions = append(doc.Regions, orgsOutRegion{ID: r.region, Orgs: outOrgs})
		}

		switch strings.ToLower(format) {
		case "json":
			return writeOrgsJSON(doc)
		case "csv":
			return writeOrgsCSV(doc)
		default: // "toon"
			return writeOrgsToon(doc)
		}
	},
}

func writeOrgsToon(doc orgsOutDoc) error {
	out, err := toonenc.Marshal(doc, toonenc.WithIndent(2))
	if err != nil {
		return fmt.Errorf("encoding TOON: %w", err)
	}
	if _, err = os.Stdout.Write(out); err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout)
	return err
}

func writeOrgsJSON(doc orgsOutDoc) error {
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding JSON: %w", err)
	}
	fmt.Fprintln(os.Stdout, string(out))
	return nil
}

// writeOrgsCSV writes the traditional flat CSV (region,org_id,org_name) that
// is compatible with the --orgs / --excludeOrgs flags of other commands.
func writeOrgsCSV(doc orgsOutDoc) error {
	w := csv.NewWriter(os.Stdout)
	defer w.Flush()
	if err := w.Write([]string{"region", "org_id", "org_name"}); err != nil {
		return err
	}
	for _, r := range doc.Regions {
		for _, o := range r.Orgs {
			if err := w.Write([]string{r.ID, o.ID, o.Name}); err != nil {
				return err
			}
		}
	}
	return nil
}

func init() {
	orgsCmd.GroupID = "cf-org"
	rootCmd.AddCommand(orgsCmd)
	orgsCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	orgsCmd.Flags().String("format", "toon", "Output format: toon (default), json, or csv")
}
