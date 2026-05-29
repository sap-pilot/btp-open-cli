package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"btp-open-cli/internal/store"
)

// setupTestEnv redirects HOME to a temp directory and writes a minimal
// credentials file pre-populated with tokens for the given CF API URLs.
// All cleanup is handled automatically via t.Setenv.
func setupTestEnv(t *testing.T, apiURLs ...string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	tokens := make(map[string]store.RegionToken)
	for _, u := range apiURLs {
		tokens[u] = store.RegionToken{
			APIURL:      u,
			AccessToken: "test-token",
			TokenType:   "bearer",
		}
	}
	creds := &store.Credentials{
		ActiveAPIURLs: apiURLs,
		Tokens:        tokens,
	}
	if err := store.Save(creds); err != nil {
		t.Fatal(err)
	}
}

// setupTestEnvWithXsuaa redirects HOME and writes credentials that include a
// pre-cached XSUAA token for orgGUID pointing at xsuaaAPIURL. The cached token
// is valid for one hour so resolveXsuaaClients skips the CF credential-fetch flow.
func setupTestEnvWithXsuaa(t *testing.T, cfAPIURL, orgGUID, xsuaaAPIURL string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	creds := &store.Credentials{
		ActiveAPIURLs: []string{cfAPIURL},
		Tokens: map[string]store.RegionToken{
			cfAPIURL: {
				APIURL:      cfAPIURL,
				AccessToken: "test-token",
				TokenType:   "bearer",
			},
		},
		OrgXsuaa: map[string]store.XsuaaData{
			orgGUID: {
				APIURL:      xsuaaAPIURL,
				AccessToken: "xsuaa-test-token",
				TokenExpiry: time.Now().Add(time.Hour),
			},
		},
	}
	if err := store.Save(creds); err != nil {
		t.Fatal(err)
	}
}

// setupTestEnvWithDestCache redirects HOME and writes credentials that include
// a pre-cached destination service token for spaceGUID / instanceGUID pointing
// at destURI. The cached token is valid for one hour so the credential-fetch
// flow is bypassed.
func setupTestEnvWithDestCache(t *testing.T, cfAPIURL, spaceGUID, instanceGUID, destURI string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	creds := &store.Credentials{
		ActiveAPIURLs: []string{cfAPIURL},
		Tokens: map[string]store.RegionToken{
			cfAPIURL: {
				APIURL:      cfAPIURL,
				AccessToken: "test-token",
				TokenType:   "bearer",
			},
		},
		SpaceDestServices: map[string]map[string]*store.DestInstanceCache{
			spaceGUID: {
				instanceGUID: {
					InstanceName: "dest-instance",
					URI:          destURI,
					AccessToken:  "dest-test-token",
					TokenExpiry:  time.Now().Add(time.Hour),
				},
			},
		},
	}
	if err := store.Save(creds); err != nil {
		t.Fatal(err)
	}
}

// resetAllFlags resets every subcommand's local flags to their default values,
// preventing pflag state leakage between consecutive Execute() calls.
func resetAllFlags() {
	resetFlagSet := func(fs *pflag.FlagSet) {
		fs.VisitAll(func(f *pflag.Flag) {
			if f.Changed {
				_ = f.Value.Set(f.DefValue)
				f.Changed = false
			}
		})
	}
	resetFlagSet(rootCmd.PersistentFlags())
	for _, sub := range rootCmd.Commands() {
		resetFlagSet(sub.Flags())
		resetFlagSet(sub.PersistentFlags())
	}
}

// runCmd resets cobra flags, executes the root command with the given args,
// captures both os.Stdout and os.Stderr (which commands may write to directly),
// and restores all I/O streams.
//
// Note: tests using runCmd must NOT run in parallel (t.Parallel) because stdout
// and stderr are process-global.
func runCmd(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	resetAllFlags()

	// Redirect os.Stdout/os.Stderr via OS-level pipes so commands that write
	// to os.Stdout directly (not via cmd.OutOrStdout()) are captured.
	origStdout := os.Stdout
	origStderr := os.Stderr

	outR, outW, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatal("creating stdout pipe:", pipeErr)
	}
	errR, errW, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatal("creating stderr pipe:", pipeErr)
	}

	os.Stdout = outW
	os.Stderr = errW

	// Also update cobra's output writers for commands that use cmd.OutOrStdout().
	rootCmd.SetOut(outW)
	rootCmd.SetErr(errW)
	rootCmd.SetArgs(args)

	// Read from both pipes concurrently to avoid deadlocking if commands produce
	// output on both streams.
	var outBuf, errBuf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(&outBuf, outR) }() //nolint:errcheck
	go func() { defer wg.Done(); io.Copy(&errBuf, errR) }() //nolint:errcheck

	err = rootCmd.Execute()

	// Close write ends to signal EOF to the readers.
	outW.Close()
	errW.Close()
	wg.Wait()

	// Restore original streams.
	os.Stdout = origStdout
	os.Stderr = origStderr
	rootCmd.SetOut(origStdout)
	rootCmd.SetErr(origStderr)
	outR.Close()
	errR.Close()

	return outBuf.String(), errBuf.String(), err
}

// fakeCFServer creates an httptest.Server that matches requests by URL path
// (query params ignored) and returns the configured JSON response body.
// A 404 is returned for unregistered paths.
func fakeCFServer(t *testing.T, routes map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.URL.Path]
		if !ok {
			// Try prefix match
			for k, v := range routes {
				if strings.HasPrefix(r.URL.Path, k) {
					body = v
					ok = true
					break
				}
			}
		}
		if !ok {
			http.Error(w, "no route for: "+r.URL.Path, 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	return srv
}

// mustJSON marshals v to compact JSON, fataling the test on error.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// emptyPage returns a JSON CF API response with zero results and no Next link.
func emptyPage() string {
	return `{"pagination":{"total_pages":1},"resources":[]}`
}

// singleOrgPage returns a JSON CF v3 organizations response with one org.
func singleOrgPage(guid, name string) string {
	return mustJSONStr(map[string]interface{}{
		"pagination": map[string]interface{}{"total_pages": 1},
		"resources":  []map[string]string{{"guid": guid, "name": name}},
	})
}

// mustJSONStr marshals v to a compact JSON string (panics on error — test use only).
func mustJSONStr(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
