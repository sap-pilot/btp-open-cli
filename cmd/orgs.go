package cmd

import (
	"encoding/csv"
	"fmt"
	"os"
	"os/signal"
	"sync"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

var orgsCmd = &cobra.Command{
	Use:   "orgs",
	Short: "List all accessible CF organizations across one or more regions",
	Long: `List all Cloud Foundry organizations the authenticated user can access.

Output is CSV with columns: region,id,name

If --regions is omitted, the regions from the last login are used.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		regionsFlag, _ := cmd.Flags().GetString("regions")

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

		type regionOrgs struct {
			region string
			orgs   []cf.Organization
			err    error
		}
		results := make([]regionOrgs, len(apiURLs))
		var wg sync.WaitGroup

		for i, apiURL := range apiURLs {
			wg.Add(1)
			go func(idx int, url string) {
				defer wg.Done()
				regionName := store.APIURLToRegion(url)
				tok, ok := creds.Tokens[url]
				if !ok {
					results[idx] = regionOrgs{region: regionName,
						err: fmt.Errorf("no token — run: bo login --regions %s", regionName)}
					return
				}
				orgs, err := cf.NewClient(url, tok.AccessToken).ListOrganizations(ctx)
				results[idx] = regionOrgs{region: regionName, orgs: orgs, err: err}
			}(i, apiURL)
		}
		wg.Wait()

		w := csv.NewWriter(os.Stdout)
		defer w.Flush()

		if err := w.Write([]string{"region", "id", "name"}); err != nil {
			return err
		}
		for _, r := range results {
			if r.err != nil {
				fmt.Fprintf(os.Stderr, "[%s] error: %v\n", r.region, r.err)
				continue
			}
			for _, o := range r.orgs {
				if err := w.Write([]string{r.region, o.GUID, o.Name}); err != nil {
					return err
				}
			}
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(orgsCmd)
	orgsCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); uses stored regions if omitted")
}
