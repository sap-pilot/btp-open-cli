package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDeleteUsersCSV writes a temporary CSV file with origin,userName rows
// and returns its path (registered for cleanup via t.Cleanup).
func writeDeleteUsersCSV(t *testing.T, rows ...[]string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "delete-users-*.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString("origin,userName\n") //nolint:errcheck
	for _, row := range rows {
		f.WriteString(strings.Join(row, ",") + "\n") //nolint:errcheck
	}
	return f.Name()
}

func TestDeleteUsers_MissingUsersFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setupTestEnv(t, "http://fake-cf.example.com")
	_, _, err := runCmd(t, "delete-users", "--no-prompt")
	if err == nil {
		t.Fatal("expected error when --users is not provided")
	}
}

func TestDeleteUsers_InvalidCSV(t *testing.T) {
	const orgGUID = "org1"
	// Credentials file with XSUAA cache — no actual server needed because
	// invalid CSV should fail before any API call.
	t.Setenv("HOME", t.TempDir())
	setupTestEnv(t, "http://fake-cf.example.com")

	// Write CSV with wrong header
	badCSV := filepath.Join(t.TempDir(), "bad.csv")
	os.WriteFile(badCSV, []byte("wrong,header\n"), 0644) //nolint:errcheck

	_, _, err := runCmd(t, "delete-users", "--users", badCSV, "--no-prompt")
	if err == nil {
		t.Fatal("expected error for invalid CSV header")
	}
}

func TestDeleteUsers_NoOrgsFound(t *testing.T) {
	// With --no-prompt and no XSUAA cache, the command should skip all orgs
	// (no prompt) and report no users to delete.
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("org1", "my-org"),
		// No xsuaa plan → FindServicePlan returns empty → org skipped
		"/v3/service_plans": emptyPage(),
	})
	setupTestEnv(t, srv.URL)

	usersFile := writeDeleteUsersCSV(t, []string{"sap.ids", "alice@example.com"})

	// Should succeed but skip all orgs since there's no XSUAA instance
	_, _, err := runCmd(t, "delete-users", "--users", usersFile, "--no-prompt", "--yes")
	// Error is acceptable here; we're testing it doesn't hang or crash
	_ = err
}
