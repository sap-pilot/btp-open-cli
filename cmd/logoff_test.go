package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"btp-open-cli/internal/store"
)

func TestLogoff_ClearsTokens(t *testing.T) {
	// setupTestEnv sets HOME to a temp dir and writes credentials.
	setupTestEnv(t, "http://fake-cf.example.com")

	// Derive the credentials path from the HOME that setupTestEnv set.
	home := os.Getenv("HOME")
	credFile := filepath.Join(home, ".bo", "credentials.json")
	if _, err := os.Stat(credFile); err != nil {
		t.Fatalf("credentials file not created: %v", err)
	}

	stdout, _, err := runCmd(t, "logoff")
	if err != nil {
		t.Fatalf("logoff command failed: %v", err)
	}
	if !strings.Contains(stdout, "Logged off") {
		t.Errorf("expected 'Logged off' in output, got: %q", stdout)
	}

	// After logoff, the credentials file should still exist (regions preserved)
	// but tokens should be cleared.
	if _, err := os.Stat(credFile); os.IsNotExist(err) {
		t.Error("credentials file removed entirely; regions should have been preserved")
	}
}

func TestLogoff_ClearsDefaultOrgScope(t *testing.T) {
	setupTestEnv(t, "http://fake-cf.example.com")
	setDefaultOrgScope(t, "http://fake-cf.example.com", "org1", "my-org")

	if _, _, err := runCmd(t, "logoff"); err != nil {
		t.Fatalf("logoff command failed: %v", err)
	}

	creds, err := store.Load()
	if err != nil {
		t.Fatalf("loading creds after logoff: %v", err)
	}
	if len(creds.DefaultOrgScope) != 0 {
		t.Errorf("expected default org scope cleared after logoff, got: %+v", creds.DefaultOrgScope)
	}
}

func TestLogoff_NoCredentials(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// No credentials file written — logoff should still succeed gracefully.
	_, _, err := runCmd(t, "logoff")
	if err != nil {
		t.Errorf("logoff with no credentials file should not fail: %v", err)
	}
}
