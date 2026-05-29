package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOrgs_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "orgs")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestOrgs_DefaultToon(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": singleOrgPage("g1", "my-org"),
	})
	setupTestEnv(t, srv.URL)

	stdout, _, err := runCmd(t, "orgs")
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

	stdout, _, err := runCmd(t, "orgs", "--format", "json")
	if err != nil {
		t.Fatalf("orgs --format json failed: %v", err)
	}

	var doc orgsOutDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v\nOutput: %s", err, stdout)
	}
	if len(doc.Regions) == 0 {
		t.Error("expected at least one region in JSON output")
	}
	if len(doc.Regions[0].Orgs) == 0 {
		t.Error("expected at least one org in JSON output")
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

	stdout, _, err := runCmd(t, "orgs", "--format", "csv")
	if err != nil {
		t.Fatalf("orgs --format csv failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 lines (header + data), got %d", len(lines))
	}
	if lines[0] != "region,org_id,org_name" {
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

	stdout, _, err := runCmd(t, "orgs", "--format", "csv")
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

func TestOrgs_EmptyRegion(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": emptyPage(),
	})
	setupTestEnv(t, srv.URL)

	stdout, _, err := runCmd(t, "orgs", "--format", "json")
	if err != nil {
		t.Fatalf("orgs with empty region failed: %v", err)
	}
	var doc orgsOutDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	// With no orgs the region is still included but with an empty orgs list.
	if len(doc.Regions) > 0 && len(doc.Regions[0].Orgs) != 0 {
		t.Errorf("expected empty orgs in region, got %d", len(doc.Regions[0].Orgs))
	}
}
