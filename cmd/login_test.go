package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"btp-open-cli/internal/store"
)

// fakeAuthServer serves the two endpoints `bo login` needs: CF's /v2/info
// (to discover the login/token servers) and /oauth/token (both password and
// passcode grants land here). It always reports itself as both endpoints,
// which is fine since the test only ever talks to this one server.
func fakeAuthServer(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/info":
			w.Write([]byte(mustJSONStr(map[string]string{ //nolint:errcheck
				"authorization_endpoint": srv.URL,
				"token_endpoint":         srv.URL,
			})))
		case "/oauth/token":
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"access_token":  "test-access-token",
				"refresh_token": "test-refresh-token",
				"token_type":    "bearer",
				"expires_in":    3600,
			})))
		default:
			http.Error(w, "no route for: "+r.URL.Path, 404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLogin_SSO_PasscodeFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := fakeAuthServer(t)

	stdout, _, err := runCmd(t, "login", "--sso", "--api", srv.URL, "--passcode", "the-passcode")
	if err != nil {
		t.Fatalf("login --sso --passcode failed: %v\nstdout: %s", err, stdout)
	}
	if !strings.Contains(stdout, "Authenticated. 1 region(s) active.") {
		t.Errorf("expected success message, got: %q", stdout)
	}

	creds, err := store.Load()
	if err != nil {
		t.Fatalf("loading creds: %v", err)
	}
	tok, ok := creds.Tokens[srv.URL]
	if !ok {
		t.Fatalf("expected a token for %s, got: %+v", srv.URL, creds.Tokens)
	}
	if tok.LoginType != "sso" {
		t.Errorf("expected LoginType 'sso', got: %q", tok.LoginType)
	}
	if tok.AccessToken != "test-access-token" {
		t.Errorf("unexpected access token: %q", tok.AccessToken)
	}
}

func TestLogin_SSO_PasscodeCountMismatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := fakeAuthServer(t)

	_, _, err := runCmd(t, "login", "--sso", "--api", srv.URL, "--passcode", "code-one,code-two")
	if err == nil {
		t.Fatal("expected an error when --passcode count doesn't match the number of regions")
	}
	if !strings.Contains(err.Error(), "--passcode has 2 code(s) but 1 region(s)") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLogin_Password_Flags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := fakeAuthServer(t)

	stdout, _, err := runCmd(t, "login", "--api", srv.URL, "--username", "alice@example.com", "--password", "secret")
	if err != nil {
		t.Fatalf("login with -u/-p failed: %v\nstdout: %s", err, stdout)
	}

	creds, err := store.Load()
	if err != nil {
		t.Fatalf("loading creds: %v", err)
	}
	tok, ok := creds.Tokens[srv.URL]
	if !ok {
		t.Fatalf("expected a token for %s, got: %+v", srv.URL, creds.Tokens)
	}
	if tok.LoginType != "password" {
		t.Errorf("expected LoginType 'password', got: %q", tok.LoginType)
	}
}
