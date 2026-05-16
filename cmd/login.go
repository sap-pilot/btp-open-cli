package cmd

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate against SAP BTP Cloud Foundry",
	Long: `Authenticate against SAP BTP Cloud Foundry using SSO (one-time passcode).

Specify either --region for a standard SAP BTP region (e.g. us10, eu10),
or --api for the exact CF API endpoint shown in BTP Cockpit:

  bo login --sso --region us10
  bo login --sso --api https://api.cf.us10-001.hana.ondemand.com`,
	RunE: func(cmd *cobra.Command, args []string) error {
		sso, _ := cmd.Flags().GetBool("sso")
		if !sso {
			return fmt.Errorf("only SSO authentication is supported; use: bo login --sso --region <region>")
		}

		apiURL, _ := cmd.Flags().GetString("api")
		region, _ := cmd.Flags().GetString("region")

		switch {
		case apiURL != "":
			// use as-is
		case region != "":
			apiURL = fmt.Sprintf("https://api.cf.%s.hana.ondemand.com", region)
		default:
			return fmt.Errorf("provide --api <cf-api-url> or --region <region>")
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		slog.Debug("fetching CF authorization endpoint", "api_url", apiURL)
		authEndpoint, err := cf.GetAuthorizationEndpoint(ctx, apiURL)
		if err != nil {
			return fmt.Errorf("could not reach CF API at %q: %w", apiURL, err)
		}
		slog.Debug("authorization endpoint resolved", "url", authEndpoint)

		passcodeURL := authEndpoint + "/passcode"
		fmt.Fprintf(os.Stdout, "\nOne Time Code (Get one at %s)\nPasscode> ", passcodeURL)

		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		passcode := strings.TrimSpace(scanner.Text())
		if passcode == "" {
			return fmt.Errorf("passcode cannot be empty")
		}

		slog.Debug("exchanging passcode for tokens", "token_url", authEndpoint+"/oauth/token")
		tr, err := cf.ExchangePasscode(ctx, authEndpoint, passcode)
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
	loginCmd.Flags().String("region", "", "SAP BTP CF region shorthand (e.g. us10, eu10)")
	loginCmd.Flags().String("api", "", "Full CF API endpoint URL (overrides --region; find it in BTP Cockpit)")
}
