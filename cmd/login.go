package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate against SAP BTP Cloud Foundry",
	Long: `Authenticate against SAP BTP Cloud Foundry.

Single-region (password):
  bo login --region us10
  bo login --api https://api.cf.us10-001.hana.ondemand.com

Multi-region (password, prompts once):
  bo login --regions us10,eu10

SSO (one-time passcode):
  bo login --sso --region us10
  bo login --sso --regions us10,eu10

Omit region flags to reuse the regions from the previous login.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		sso, _ := cmd.Flags().GetBool("sso")
		regionsFlag, _ := cmd.Flags().GetString("regions")
		region, _ := cmd.Flags().GetString("region")
		apiURL, _ := cmd.Flags().GetString("api")
		usernameFlag, _ := cmd.Flags().GetString("username")
		passwordFlag, _ := cmd.Flags().GetString("password")

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		// Resolve the ordered list of CF API base URLs to authenticate against.
		apiURLs, err := resolveAPIURLs(regionsFlag, region, apiURL)
		if err != nil {
			return err
		}

		// Load existing credentials to merge into; create fresh if absent.
		creds, loadErr := store.Load()
		if loadErr != nil {
			creds = &store.Credentials{Tokens: make(map[string]store.RegionToken)}
		}

		// Fetch CF endpoints for every URL in parallel.
		type endpointResult struct {
			apiURL    string
			endpoints *cf.Endpoints
			err       error
		}
		epResults := make([]endpointResult, len(apiURLs))
		var wg sync.WaitGroup
		for i, u := range apiURLs {
			wg.Add(1)
			go func(idx int, url string) {
				defer wg.Done()
				ep, e := cf.GetEndpoints(ctx, url)
				epResults[idx] = endpointResult{apiURL: url, endpoints: ep, err: e}
			}(i, u)
		}
		wg.Wait()

		// Validate: all endpoints must be reachable before we prompt credentials.
		for _, r := range epResults {
			if r.err != nil {
				return fmt.Errorf("could not reach CF API at %q: %w", r.apiURL, r.err)
			}
		}

		var (
			username string
			pwBytes  []byte
		)

		if !sso {
			// Use -u / -p flags when provided (non-interactive / CI mode).
			username = usernameFlag
			pwBytes = []byte(passwordFlag)

			// Fall back to interactive prompts for any missing value.
			if username == "" {
				fmt.Fprintf(os.Stdout, "Email> ")
				text, ok := readLine(ctx)
				if !ok {
					fmt.Fprintln(os.Stdout, "Aborted.")
					return nil
				}
				username = text
			}
			if username == "" {
				return fmt.Errorf("email cannot be empty")
			}
			if len(pwBytes) == 0 {
				fmt.Fprintf(os.Stdout, "Password> ")
				pwBytes, err = readPasswordCtx(ctx)
				fmt.Fprintln(os.Stdout)
				if err != nil {
					if errors.Is(err, context.Canceled) {
						fmt.Fprintln(os.Stdout, "Aborted.")
						return nil
					}
					return fmt.Errorf("reading password: %w", err)
				}
			}
			if len(pwBytes) == 0 {
				return fmt.Errorf("password cannot be empty")
			}
		}

		// For SSO: collect a passcode per region first (user can open all tabs
		// in the browser before entering codes), then authenticate in order.
		type ssoPasscode struct {
			apiURL   string
			region   string
			authEP   string
			passcode string
		}
		var ssoCodes []ssoPasscode
		if sso {
			fmt.Fprintln(os.Stdout, "\nGet a one-time passcode for each region:")
			for _, r := range epResults {
				regionName := store.APIURLToRegion(r.apiURL)
				slog.Debug("passcode URL", "region", regionName, "url", r.endpoints.Authorization+"/passcode")
				fmt.Fprintf(os.Stdout, "  %s → %s/passcode\n", regionName, r.endpoints.Authorization)
			}
			fmt.Fprintln(os.Stdout)
			for _, r := range epResults {
				regionName := store.APIURLToRegion(r.apiURL)
				fmt.Fprintf(os.Stdout, "%s Passcode> ", regionName)
				codeBytes, err := readPasswordCtx(ctx)
				fmt.Fprintln(os.Stdout)
				if err != nil {
					if errors.Is(err, context.Canceled) {
						fmt.Fprintln(os.Stdout, "Aborted.")
						return nil
					}
					return fmt.Errorf("reading passcode for %s: %w", regionName, err)
				}
				code := strings.TrimSpace(string(codeBytes))
				if code == "" {
					return fmt.Errorf("passcode for %s cannot be empty", regionName)
				}
				ssoCodes = append(ssoCodes, ssoPasscode{
					apiURL:   r.apiURL,
					region:   regionName,
					authEP:   r.endpoints.Authorization,
					passcode: code,
				})
			}
		}

		// Authenticate against each region (parallel for password, sequential for SSO
		// since we already have all codes).
		type authResult struct {
			apiURL string
			token  *store.RegionToken
			err    error
		}
		authResults := make([]authResult, len(apiURLs))

		if sso {
			// Sequential but non-blocking — codes already collected.
			var authWg sync.WaitGroup
			for i, sc := range ssoCodes {
				authWg.Add(1)
				go func(idx int, s ssoPasscode) {
					defer authWg.Done()
					tr, e := cf.ExchangePasscode(ctx, s.authEP, s.passcode)
					if e != nil {
						authResults[idx] = authResult{apiURL: s.apiURL, err: e}
						return
					}
					tok := buildRegionTokenWithType(s.apiURL, tr, "sso")
					authResults[idx] = authResult{apiURL: s.apiURL, token: &tok}
				}(i, sc)
			}
			authWg.Wait()
		} else {
			password := string(pwBytes)
			var authWg sync.WaitGroup
			for i, r := range epResults {
				authWg.Add(1)
				go func(idx int, ep endpointResult) {
					defer authWg.Done()
					tr, e := cf.PasswordLogin(ctx, ep.endpoints.Token, username, password)
					if e != nil {
						authResults[idx] = authResult{apiURL: ep.apiURL, err: e}
						return
					}
					tok := buildRegionTokenWithType(ep.apiURL, tr, "password")
					authResults[idx] = authResult{apiURL: ep.apiURL, token: &tok}
				}(i, r)
			}
			authWg.Wait()
		}

		// Merge successful tokens into credentials; report failures.
		var activeURLs []string
		for _, ar := range authResults {
			region := store.APIURLToRegion(ar.apiURL)
			if ar.err != nil {
				fmt.Fprintf(os.Stderr, "  %s: FAILED — %v\n", region, ar.err)
				continue
			}
			creds.Tokens[ar.apiURL] = *ar.token
			activeURLs = append(activeURLs, ar.apiURL)
			fmt.Fprintf(os.Stdout, "  %s: OK\n", region)
		}

		if len(activeURLs) == 0 {
			return fmt.Errorf("authentication failed for all regions")
		}

		creds.ActiveAPIURLs = activeURLs
		if err := store.Save(creds); err != nil {
			return fmt.Errorf("saving credentials: %w", err)
		}

		fmt.Fprintf(os.Stdout, "\nAuthenticated. %d region(s) active.\n", len(activeURLs))
		return nil
	},
}

// resolveAPIURLs builds the ordered list of CF API base URLs from flags.
// Priority: --regions > --api > --region > stored credentials.
func resolveAPIURLs(regionsFlag, region, apiURL string) ([]string, error) {
	switch {
	case regionsFlag != "":
		var urls []string
		for _, r := range splitCSV(regionsFlag) {
			urls = append(urls, store.RegionToAPIURL(r))
		}
		return urls, nil
	case apiURL != "":
		return []string{apiURL}, nil
	case region != "":
		return []string{store.RegionToAPIURL(region)}, nil
	default:
		creds, err := store.Load()
		if err != nil || len(creds.ActiveAPIURLs) == 0 {
			return nil, fmt.Errorf(
				"provide --regions <r1,r2>, --region <region>, or --api <url>\n" +
					"(find the API endpoint in BTP Cockpit → subaccount → Cloud Foundry Environment)",
			)
		}
		regions := make([]string, len(creds.ActiveAPIURLs))
		for i, u := range creds.ActiveAPIURLs {
			regions[i] = store.APIURLToRegion(u)
		}
		fmt.Fprintf(os.Stderr, "Using previously configured region(s): %s\n",
			strings.Join(regions, ", "))
		return creds.ActiveAPIURLs, nil
	}
}

func buildRegionToken(apiURL string, tr *cf.TokenResponse) store.RegionToken {
	return store.RegionToken{
		APIURL:       apiURL,
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		TokenType:    tr.TokenType,
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}
}

func buildRegionTokenWithType(apiURL string, tr *cf.TokenResponse, loginType string) store.RegionToken {
	tok := buildRegionToken(apiURL, tr)
	tok.LoginType = loginType
	return tok
}

// splitCSV parses a comma-separated string, trimming whitespace from each element.
func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func init() {
	loginCmd.GroupID = "common"
	rootCmd.AddCommand(loginCmd)
	loginCmd.Flags().Bool("sso", false, "Authenticate using a one-time SSO passcode")
	loginCmd.Flags().String("regions", "", "Comma-separated CF regions (e.g. us10,eu10); persisted for future commands")
	loginCmd.Flags().String("region", "", "Single CF region shorthand (e.g. us10)")
	loginCmd.Flags().String("api", "", "Full CF API endpoint URL (overrides --region)")
	loginCmd.Flags().StringP("username", "u", "", "Username (email); skips the interactive email prompt")
	loginCmd.Flags().StringP("password", "p", "", "Password; skips the interactive password prompt")
}
