package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"btp-open-cli/internal/store"
)

func TestOrgs_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "orgs", "--all")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestOrgs_NoTTYRequiresAll verifies that without --all, the command tries to
// run the interactive picker and fails fast in a non-interactive environment
// (such as this test process) instead of hanging.
func TestOrgs_NoTTYRequiresAll(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("g1", "my-org"),
	})
	setupTestEnv(t, srv.URL)

	_, _, err := runCmd(t, "orgs")
	if err == nil {
		t.Fatal("expected error when stdin is not a terminal and --all is not given")
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Errorf("expected error to mention 'terminal', got: %v", err)
	}
}

func TestOrgs_DefaultToon(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("g1", "my-org"),
	})
	setupTestEnv(t, srv.URL)

	stdout, _, err := runCmd(t, "orgs", "--all")
	if err != nil {
		t.Fatalf("orgs command failed: %v", err)
	}
	if !strings.Contains(stdout, "my-org") {
		t.Errorf("expected org name in output, got: %q", stdout)
	}
}

func TestOrgs_JSON(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("g1", "my-org"),
	})
	setupTestEnv(t, srv.URL)

	stdout, _, err := runCmd(t, "orgs", "--all", "--format", "json")
	if err != nil {
		t.Fatalf("orgs --format json failed: %v", err)
	}

	var doc orgsOutDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v\nOutput: %s", err, stdout)
	}
	if len(doc.Regions) == 0 {
		t.Fatal("expected at least one region in JSON output")
	}
	if len(doc.Regions[0].Orgs) == 0 {
		t.Fatal("expected at least one org in JSON output")
	}
	if doc.Regions[0].Orgs[0].Name != "my-org" {
		t.Errorf("expected my-org, got %q", doc.Regions[0].Orgs[0].Name)
	}
}

func TestOrgs_CSV(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("g1", "my-org"),
	})
	setupTestEnv(t, srv.URL)

	stdout, _, err := runCmd(t, "orgs", "--all", "--format", "csv")
	if err != nil {
		t.Fatalf("orgs --format csv failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 lines (header + data), got %d", len(lines))
	}
	if lines[0] != "region,org_name,org_id" {
		t.Errorf("unexpected CSV header: %q", lines[0])
	}
	if !strings.Contains(lines[1], "my-org") {
		t.Errorf("expected my-org in CSV data row, got: %q", lines[1])
	}
}

func TestOrgs_MultipleRegions(t *testing.T) {
	srv1 := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("g1", "org-region1"),
	})
	srv2 := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("g2", "org-region2"),
	})
	setupTestEnv(t, srv1.URL, srv2.URL)

	stdout, _, err := runCmd(t, "orgs", "--all", "--format", "csv")
	if err != nil {
		t.Fatalf("orgs with multiple regions failed: %v", err)
	}
	if !strings.Contains(stdout, "org-region1") {
		t.Errorf("expected org-region1 in output, got: %q", stdout)
	}
	if !strings.Contains(stdout, "org-region2") {
		t.Errorf("expected org-region2 in output, got: %q", stdout)
	}
}

func TestOrgs_Include(t *testing.T) {
	twoOrgs := mustJSONStr(map[string]interface{}{
		"pagination": map[string]interface{}{"total_pages": 1},
		"resources": []map[string]string{
			{"guid": "g1", "name": "prod-org"},
			{"guid": "g2", "name": "dev-org"},
		},
	})
	srv := fakeCFServer(t, map[string]string{"/v3/organizations": twoOrgs})
	setupTestEnv(t, srv.URL)

	stdout, _, err := runCmd(t, "orgs", "--all", "--include", "prod")
	if err != nil {
		t.Fatalf("orgs --include failed: %v", err)
	}
	if !strings.Contains(stdout, "prod-org") {
		t.Errorf("expected prod-org in output, got: %q", stdout)
	}
	if strings.Contains(stdout, "dev-org") {
		t.Errorf("dev-org should have been excluded by --include, got: %q", stdout)
	}
}

func TestOrgs_Exclude(t *testing.T) {
	twoOrgs := mustJSONStr(map[string]interface{}{
		"pagination": map[string]interface{}{"total_pages": 1},
		"resources": []map[string]string{
			{"guid": "g1", "name": "prod-org"},
			{"guid": "g2", "name": "dev-org"},
		},
	})
	srv := fakeCFServer(t, map[string]string{"/v3/organizations": twoOrgs})
	setupTestEnv(t, srv.URL)

	stdout, _, err := runCmd(t, "orgs", "--all", "--exclude", "prod")
	if err != nil {
		t.Fatalf("orgs --exclude failed: %v", err)
	}
	if strings.Contains(stdout, "prod-org") {
		t.Errorf("prod-org should have been excluded, got: %q", stdout)
	}
	if !strings.Contains(stdout, "dev-org") {
		t.Errorf("expected dev-org in output, got: %q", stdout)
	}
}

// TestOrgs_NoMatches verifies that --include/--exclude filtering down to zero
// candidate orgs is a clear error rather than an empty successful selection.
func TestOrgs_NoMatches(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("g1", "my-org"),
	})
	setupTestEnv(t, srv.URL)

	_, _, err := runCmd(t, "orgs", "--all", "--include", "nonexistent")
	if err == nil {
		t.Fatal("expected error when no orgs match --include")
	}
}

// TestOrgs_EmptyAccount verifies that a fully empty account (zero orgs in
// any region) is a clear error — there is nothing to select.
func TestOrgs_EmptyAccount(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": emptyPage(),
	})
	setupTestEnv(t, srv.URL)

	_, _, err := runCmd(t, "orgs", "--all")
	if err == nil {
		t.Fatal("expected error when there are no accessible orgs to select")
	}
}

// TestOrgs_SetsDefaultScope verifies that running `bo orgs --all` persists
// the selection to credentials.json as the session's default org scope.
func TestOrgs_SetsDefaultScope(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("g1", "my-org"),
	})
	setupTestEnv(t, srv.URL)

	if _, _, err := runCmd(t, "orgs", "--all"); err != nil {
		t.Fatalf("orgs --all failed: %v", err)
	}

	creds, err := store.Load()
	if err != nil {
		t.Fatalf("loading creds: %v", err)
	}
	if len(creds.DefaultOrgScope) != 1 {
		t.Fatalf("expected 1 org in default scope, got %d", len(creds.DefaultOrgScope))
	}
	got := creds.DefaultOrgScope[0]
	if got.ID != "g1" || got.Name != "my-org" || got.APIURL != srv.URL {
		t.Errorf("unexpected default scope entry: %+v", got)
	}
}

// TestOrgs_OutputFlag verifies --output writes the selected-orgs listing to
// a file instead of stdout.
func TestOrgs_OutputFlag(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("g1", "my-org"),
	})
	setupTestEnv(t, srv.URL)

	outPath := filepath.Join(t.TempDir(), "orgs.csv")
	stdout, _, err := runCmd(t, "orgs", "--all", "--format", "csv", "--output", outPath)
	if err != nil {
		t.Fatalf("orgs --output failed: %v", err)
	}
	if stdout != "" {
		t.Errorf("expected no listing on stdout when --output is set, got: %q", stdout)
	}
	data, readErr := os.ReadFile(outPath)
	if readErr != nil {
		t.Fatalf("reading output file: %v", readErr)
	}
	if !strings.Contains(string(data), "my-org") {
		t.Errorf("expected my-org in output file, got: %q", string(data))
	}
}
