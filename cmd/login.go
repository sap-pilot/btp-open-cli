package cmd

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	"golang.org/x/term"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate against SAP BTP Cloud Foundry",
	Long: `Authenticate against SAP BTP Cloud Foundry.

Password login (prompts for email and password):
  bo login --region us10
  bo login --api https://api.cf.us10-001.hana.ondemand.com

SSO login (one-time passcode):
  bo login --sso --region us10

If --region and --api are both omitted, the API endpoint from the previous
login is reused. Find your exact endpoint in BTP Cockpit → subaccount →
Cloud Foundry Environment.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		sso, _ := cmd.Flags().GetBool("sso")
		apiURL, _ := cmd.Flags().GetString("api")
		region, _ := cmd.Flags().GetString("region")

		// Resolve CF API base URL: --api > --region > stored from last login.
		switch {
		case apiURL != "":
			// use as-is
		case region != "":
			apiURL = fmt.Sprintf("https://api.cf.%s.hana.ondemand.com", region)
		default:
			stored, err := store.Load()
			if err != nil || stored.APIURL == "" {
				return fmt.Errorf(
					"provide --region <region> or --api <url>\n" +
						"(find the API endpoint in BTP Cockpit → subaccount → Cloud Foundry Environment)",
				)
			}
			apiURL = stored.APIURL
			fmt.Fprintf(os.Stderr, "Using previously configured API: %s\n", apiURL)
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		slog.Debug("resolving CF endpoints", "api_url", apiURL)
		endpoints, err := cf.GetEndpoints(ctx, apiURL)
		if err != nil {
			return fmt.Errorf("could not reach CF API at %q: %w", apiURL, err)
		}

		var tr *cf.TokenResponse

		if sso {
			passcodeURL := endpoints.Authorization + "/passcode"
			fmt.Fprintf(os.Stdout, "\nOne Time Code (Get one at %s)\nPasscode> ", passcodeURL)

			scanner := bufio.NewScanner(os.Stdin)
			scanner.Scan()
			passcode := strings.TrimSpace(scanner.Text())
			if passcode == "" {
				return fmt.Errorf("passcode cannot be empty")
			}

			slog.Debug("exchanging SSO passcode", "token_url", endpoints.Authorization+"/oauth/token")
			tr, err = cf.ExchangePasscode(ctx, endpoints.Authorization, passcode)
		} else {
			fmt.Fprintf(os.Stdout, "Email> ")
			scanner := bufio.NewScanner(os.Stdin)
			scanner.Scan()
			username := strings.TrimSpace(scanner.Text())
			if username == "" {
				return fmt.Errorf("email cannot be empty")
			}

			fmt.Fprintf(os.Stdout, "Password> ")
			pwBytes, readErr := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stdout)
			if readErr != nil {
				return fmt.Errorf("reading password: %w", readErr)
			}
			if len(pwBytes) == 0 {
				return fmt.Errorf("password cannot be empty")
			}

			slog.Debug("authenticating with password", "token_url", endpoints.Token+"/oauth/token", "username", username)
			tr, err = cf.PasswordLogin(ctx, endpoints.Token, username, string(pwBytes))
		}

		if err != nil {
			return fmt.Errorf("authentication failed: %w", err)
		}

		token := &store.Token{
			APIURL:       apiURL,
			AccessToken:  tr.AccessToken,
			RefreshToken: tr.RefreshToken,
			TokenType:    tr.TokenType,
			ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
		}
		if err := store.Save(token); err != nil {
			return fmt.Errorf("saving credentials: %w", err)
		}

		fmt.Fprintln(os.Stdout, "\nAuthenticated successfully.")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(loginCmd)
	loginCmd.Flags().Bool("sso", false, "Authenticate using a one-time SSO passcode")
	loginCmd.Flags().String("region", "", "SAP BTP CF region (e.g. us10, eu10); omit to reuse last login's region")
	loginCmd.Flags().String("api", "", "Full CF API endpoint URL (overrides --region)")
}
