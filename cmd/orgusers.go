package cmd

import (
	"encoding/csv"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

type orgDetail struct {
	Org   cf.Organization
	Users []cf.User
}

type regionData struct {
	Region string // display name, e.g. "us10"
	Orgs   []orgDetail
	Err    error
}

var orgUsersCmd = &cobra.Command{
	Use:   "org-users",
	Short: "List all org users across every accessible organization",
	Long: `List users in every CF organization across one or more regions.

Default output is a tree grouped by region → org → user.
Use --format=csv for machine-readable output.

If --regions is omitted, the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")
		format, _ := cmd.Flags().GetString("format")

		creds, err := store.Load()
		if err != nil {
			return fmt.Errorf("not logged in — run: bo login --regions <region>")
		}

		// Determine API URLs: --regions flag > stored ActiveAPIURLs.
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

		// Fetch each region's data in parallel, preserving input order.
		results := make([]regionData, len(apiURLs))
		var wg sync.WaitGroup
		for i, apiURL := range apiURLs {
			wg.Add(1)
			go func(idx int, url string) {
				defer wg.Done()
				regionName := store.APIURLToRegion(url)
				slog.Debug("fetching region", "region", regionName)

				tok, ok := creds.Tokens[url]
				if !ok {
					results[idx] = regionData{
						Region: regionName,
						Err: fmt.Errorf("no token — run: bo login --regions %s", regionName),
					}
					return
				}

				client := cf.NewClient(url, tok.AccessToken)

				orgs, err := client.ListOrganizations(ctx)
				if err != nil {
					results[idx] = regionData{Region: regionName, Err: fmt.Errorf("listing orgs: %w", err)}
					return
				}
				slog.Debug("orgs fetched", "region", regionName, "count", len(orgs))

				details := make([]orgDetail, 0, len(orgs))
				for _, org := range orgs {
					users, err := client.ListOrganizationUsers(ctx, org.GUID)
					if err != nil {
						fmt.Fprintf(os.Stderr, "warning: skipping org %q in %s: %v\n", org.Name, regionName, err)
						continue
					}
					details = append(details, orgDetail{Org: org, Users: users})
				}
				results[idx] = regionData{Region: regionName, Orgs: details}
			}(i, apiURL)
		}
		wg.Wait()

		switch strings.ToLower(format) {
		case "csv":
			return writeOrgUsersCSV(results)
		default:
			writeOrgUsersTree(results)
			return nil
		}
	},
}

// writeOrgUsersTree prints a human-readable hierarchy:
//
//	Region: us10
//	  Org: my-org (abc-123)
//	    user@example.com  (guid-xyz)  sap.ids
func writeOrgUsersTree(results []regionData) {
	for _, r := range results {
		fmt.Fprintf(os.Stdout, "Region: %s\n", r.Region)
		if r.Err != nil {
			fmt.Fprintf(os.Stdout, "  error: %v\n\n", r.Err)
			continue
		}
		for _, od := range r.Orgs {
			fmt.Fprintf(os.Stdout, "  Org: %s (%s)\n", od.Org.Name, od.Org.GUID)
			for _, u := range od.Users {
				fmt.Fprintf(os.Stdout, "    %-45s (%s)  %s\n", u.Username, u.GUID, u.Origin)
			}
		}
		fmt.Fprintln(os.Stdout)
	}
}

// writeOrgUsersCSV writes region,org_id,org_name,user_id,user_name,user_origin rows.
func writeOrgUsersCSV(results []regionData) error {
	w := csv.NewWriter(os.Stdout)
	defer w.Flush()

	if err := w.Write([]string{"region", "org_id", "org_name", "user_id", "user_name", "user_origin"}); err != nil {
		return err
	}
	for _, r := range results {
		if r.Err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping region %q: %v\n", r.Region, r.Err)
			continue
		}
		for _, od := range r.Orgs {
			for _, u := range od.Users {
				if err := w.Write([]string{
					r.Region, od.Org.GUID, od.Org.Name,
					u.GUID, u.Username, u.Origin,
				}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func init() {
	rootCmd.AddCommand(orgUsersCmd)
	orgUsersCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
	orgUsersCmd.Flags().String("format", "tree", `Output format: tree (default) or csv`)
}
