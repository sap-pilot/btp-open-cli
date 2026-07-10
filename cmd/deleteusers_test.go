package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDeleteUsersCSV writes a temporary CSV file with region,org_id,user_id rows
// and returns its path (registered for cleanup via t.TempDir).
func writeDeleteUsersCSV(t *testing.T, rows ...[]string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "delete-users-*.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString("region,org_id,user_id\n") //nolint:errcheck
	for _, row := range rows {
		f.WriteString(strings.Join(row, ",") + "\n") //nolint:errcheck
	}
	return f.Name()
}

func TestDeleteUsers_MissingArg(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setupTestEnv(t, "http://fake-cf.example.com")
	_, _, err := runCmd(t, "delete-users")
	if err == nil {
		t.Fatal("expected error when CSV argument is not provided")
	}
}

func TestDeleteUsers_InvalidCSV(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setupTestEnv(t, "http://fake-cf.example.com")

	badCSV := filepath.Join(t.TempDir(), "bad.csv")
	os.WriteFile(badCSV, []byte("wrong,header\n"), 0644) //nolint:errcheck

	_, _, err := runCmd(t, "delete-users", badCSV)
	if err == nil {
		t.Fatal("expected error for invalid CSV header")
	}
	if !strings.Contains(err.Error(), "invalid users CSV") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestDeleteUsers_Exclude verifies that --exclude filters out matched users and only
// deletes the remaining ones.
func TestDeleteUsers_Exclude(t *testing.T) {
	const (
		orgGUID    = "org1"
		regionName = "eu20"
	)

	deleteCount := 0
	var deletedIDs []string
	xsuaaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/Users" && r.Method == "GET":
			w.Write([]byte(xsuaaUsersPage( //nolint:errcheck
				xsuaaUser("u1", "alice@example.com", "sap.ids"),
				xsuaaUser("u2", "bob@example.com", "sap.ids"),
			)))
		case strings.HasPrefix(r.URL.Path, "/Users/") && r.Method == "DELETE":
			deleteCount++
			deletedIDs = append(deletedIDs, strings.TrimPrefix(r.URL.Path, "/Users/"))
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected: "+r.URL.Path, 404)
		}
	}))
	defer xsuaaSrv.Close()

	// Use a fake CF API URL that matches the "eu20" region mapping, but the
	// XSUAA fast path bypasses CF calls entirely when OrgName+RegionName are cached.
	cfAPIURL := "https://api.cf.eu20.hana.ondemand.com"
	setupTestEnvWithFullXsuaa(t, cfAPIURL, orgGUID, "my-org", regionName, xsuaaSrv.URL)

	usersFile := writeDeleteUsersCSV(t,
		[]string{regionName, orgGUID, "u1"}, // alice — should be skipped
		[]string{regionName, orgGUID, "u2"}, // bob   — should be deleted
	)

	_, _, err := runCmd(t, "delete-users", usersFile, "--exclude", "alice", "--yes")
	if err != nil {
		t.Fatalf("delete-users --exclude failed: %v", err)
	}
	if deleteCount != 1 {
		t.Errorf("expected exactly 1 DELETE, got %d (deleted: %v)", deleteCount, deletedIDs)
	}
	for _, id := range deletedIDs {
		if id == "u1" {
			t.Error("alice (u1) should have been skipped but was deleted")
		}
	}
}

// TestDeleteUsers_Include verifies that --include filters to only matched users.
func TestDeleteUsers_Include(t *testing.T) {
	const (
		orgGUID    = "org1"
		regionName = "eu20"
	)

	deleteCount := 0
	var deletedIDs []string
	xsuaaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/Users" && r.Method == "GET":
			w.Write([]byte(xsuaaUsersPage( //nolint:errcheck
				xsuaaUser("u1", "alice@example.com", "sap.ids"),
				xsuaaUser("u2", "bob@example.com", "sap.ids"),
			)))
		case strings.HasPrefix(r.URL.Path, "/Users/") && r.Method == "DELETE":
			deleteCount++
			deletedIDs = append(deletedIDs, strings.TrimPrefix(r.URL.Path, "/Users/"))
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected: "+r.URL.Path, 404)
		}
	}))
	defer xsuaaSrv.Close()

	cfAPIURL := "https://api.cf.eu20.hana.ondemand.com"
	setupTestEnvWithFullXsuaa(t, cfAPIURL, orgGUID, "my-org", regionName, xsuaaSrv.URL)

	usersFile := writeDeleteUsersCSV(t,
		[]string{regionName, orgGUID, "u1"}, // alice — should be included
		[]string{regionName, orgGUID, "u2"}, // bob   — should be excluded
	)

	_, _, err := runCmd(t, "delete-users", usersFile, "--include", "alice", "--yes")
	if err != nil {
		t.Fatalf("delete-users --include failed: %v", err)
	}
	if deleteCount != 1 {
		t.Errorf("expected exactly 1 DELETE, got %d (deleted: %v)", deleteCount, deletedIDs)
	}
	for _, id := range deletedIDs {
		if id == "u2" {
			t.Error("bob (u2) should have been excluded by --include but was deleted")
		}
	}
}
