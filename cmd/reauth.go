package cmd

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"golang.org/x/term"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"
)

// stdinReader is the single shared buffered reader for os.Stdin. All code that
// reads from stdin (prompts, [y/N] confirmations) must use readLine or
// term.ReadPassword — never create a separate bufio.Scanner/Reader on os.Stdin.
var stdinReader = bufio.NewReader(os.Stdin)

// reauthMu ensures only one interactive re-authentication session runs at a
// time when multiple region goroutines hit 401 simultaneously.
var reauthMu sync.Mutex

// readLine reads one line from stdin, respecting context cancellation.
// Returns ("", false) if the context was cancelled or stdin was closed.
func readLine(ctx context.Context) (string, bool) {
	type result struct {
		text string
		ok   bool
	}
	ch := make(chan result, 1)
	go func() {
		line, err := stdinReader.ReadString('\n')
		ch <- result{text: strings.TrimSpace(strings.TrimRight(line, "\r\n")), ok: err == nil || (line != "" && err != nil)}
	}()
	select {
	case <-ctx.Done():
		return "", false
	case r := <-ch:
		return r.text, r.ok
	}
}

// makeTokenRefresher returns a cf.TokenRefresher for the given CF API URL.
// On 401, it first tries the stored refresh token; if that fails it prompts
// the user to re-authenticate using the same method as their last login.
// originalToken is the access token the client was initialised with — if
// another goroutine has already refreshed the token, we skip to using theirs.
func makeTokenRefresher(apiURL, originalToken string) cf.TokenRefresher {
	return func(ctx context.Context) (string, error) {
		reauthMu.Lock()
		defer reauthMu.Unlock()

		// Reload credentials — another goroutine may have already refreshed.
		creds, err := store.Load()
		if err != nil {
			return "", fmt.Errorf("loading credentials: %w", err)
		}
		tok, ok := creds.Tokens[apiURL]
		if !ok {
			return "", fmt.Errorf("no stored token for this region")
		}
		if tok.AccessToken != originalToken {
			// A concurrent goroutine already refreshed; use the new token.
			return tok.AccessToken, nil
		}

		regionName := store.APIURLToRegion(apiURL)

		// Step 1: try the stored refresh token silently.
		if tok.RefreshToken != "" {
			ep, epErr := cf.GetEndpoints(ctx, apiURL)
			if epErr == nil {
				if tr, rfErr := cf.RefreshToken(ctx, ep.Token, tok.RefreshToken); rfErr == nil {
					newTok := buildRegionToken(apiURL, tr)
					newTok.LoginType = tok.LoginType
					creds.Tokens[apiURL] = newTok
					_ = store.Save(creds)
					fmt.Fprintf(os.Stderr, "\n[%s] token refreshed automatically\n", regionName)
					return newTok.AccessToken, nil
				} else {
					slog.Debug("refresh token failed", "region", regionName, "err", rfErr)
				}
			}
		}

		// Step 2: interactive re-authentication.
		ep, err := cf.GetEndpoints(ctx, apiURL)
		if err != nil {
			return "", fmt.Errorf("[%s] reaching CF endpoints: %w", regionName, err)
		}

		fmt.Fprintf(os.Stderr, "\n[%s] Session expired. Please re-authenticate.\n", regionName)

		var tr *cf.TokenResponse
		loginType := tok.LoginType

		if loginType == "sso" {
			fmt.Fprintf(os.Stdout, "  Passcode URL: %s/passcode\n", ep.Authorization)
			tr, err = promptSSO(ctx, ep.Authorization, regionName)
		} else {
			tr, err = promptPassword(ctx, ep.Token, regionName)
		}
		if err != nil {
			if ctx.Err() != nil {
				return "", fmt.Errorf("aborted")
			}
			return "", fmt.Errorf("[%s] re-authentication failed: %w", regionName, err)
		}

		newTok := buildRegionToken(apiURL, tr)
		newTok.LoginType = loginType
		creds.Tokens[apiURL] = newTok
		_ = store.Save(creds)
		fmt.Fprintf(os.Stderr, "[%s] re-authenticated successfully\n", regionName)
		return newTok.AccessToken, nil
	}
}

// promptPassword prompts interactively for email + password and returns tokens.
func promptPassword(ctx context.Context, tokenEndpoint, regionName string) (*cf.TokenResponse, error) {
	fmt.Fprintf(os.Stdout, "%s Email> ", regionName)
	username, ok := readLine(ctx)
	if !ok {
		return nil, fmt.Errorf("aborted")
	}
	if username == "" {
		return nil, fmt.Errorf("email cannot be empty")
	}
	fmt.Fprintf(os.Stdout, "%s Password> ", regionName)
	pwBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stdout)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("aborted")
		}
		return nil, err
	}
	if len(pwBytes) == 0 {
		return nil, fmt.Errorf("password cannot be empty")
	}
	return cf.PasswordLogin(ctx, tokenEndpoint, username, string(pwBytes))
}

// promptSSO prompts for a one-time SSO passcode and returns tokens.
func promptSSO(ctx context.Context, authEndpoint, regionName string) (*cf.TokenResponse, error) {
	fmt.Fprintf(os.Stdout, "%s Passcode> ", regionName)
	codeBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stdout)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("aborted")
		}
		return nil, err
	}
	code := strings.TrimSpace(string(codeBytes))
	if code == "" {
		return nil, fmt.Errorf("passcode cannot be empty")
	}
	return cf.ExchangePasscode(ctx, authEndpoint, code)
}
