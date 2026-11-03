package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"btp-open-cli/internal/store"
)

func writeCosOrgsCSV(t *testing.T, rows [][3]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "orgs.csv")
	content := "region,org_id,org_name\n"
	for _, r := range rows {
		content += r[0] + "," + r[1] + "," + r[2] + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing orgs CSV: %v", err)
	}
	return path
}

func TestResolveFileOrgRetarget_NoDefaultScope(t *testing.T) {
	creds := &store.Credentials{}
	dest := resolveFileOrgRetarget(creds)
	if dest != nil {
		t.Errorf("expected nil (no default org scope, use the file's own orgs), got: %v", dest)
	}
}

func TestResolveFileOrgRetarget_WithDefaultScope(t *testing.T) {
	creds := &store.Credentials{DefaultOrgScope: []store.OrgScopeRef{
		{Region: "us10", ID: "org1", Name: "my-org"},
	}}
	dest := resolveFileOrgRetarget(creds)
	if len(dest) != 1 || dest[0].Region != "us10" || dest[0].OrgID != "org1" || dest[0].OrgName != "my-org" {
		t.Errorf("expected the default org scope to always be used, got: %v", dest)
	}
}

func TestResolveFileOrgFilterKeepSet_NoDefaultScope(t *testing.T) {
	creds := &store.Credentials{}
	keep := resolveFileOrgFilterKeepSet(creds)
	if keep != nil {
		t.Errorf("expected nil (no default org scope, keep every row), got: %v", keep)
	}
}

func TestResolveFileOrgFilterKeepSet_WithDefaultScope(t *testing.T) {
	creds := &store.Credentials{DefaultOrgScope: []store.OrgScopeRef{
		{Region: "us10", ID: "org1", Name: "my-org"},
	}}
	keep := resolveFileOrgFilterKeepSet(creds)
	want := orgRefKey("us10", "my-org")
	if !keep[want] || len(keep) != 1 {
		t.Errorf("expected the default org scope to always be used, got: %v", keep)
	}
}

func TestRetargetEntries(t *testing.T) {
	entries := []fileOrgRef{
		{Region: "us20", OrgID: "org2", OrgName: "other-org-1"},
		{Region: "us20", OrgID: "org2", OrgName: "other-org-1"},
	}
	setOrg := func(e fileOrgRef, region, orgID, orgName string) fileOrgRef {
		e.Region, e.OrgID, e.OrgName = region, orgID, orgName
		return e
	}

	if got := retargetEntries(entries, nil, setOrg); len(got) != 2 {
		t.Errorf("nil destOrgs should return entries unchanged, got: %v", got)
	}

	dest := []fileOrgRef{
		{Region: "us10", OrgID: "org1", OrgName: "my-org"},
		{Region: "us30", OrgID: "org3", OrgName: "third-org"},
	}
	got := retargetEntries(entries, dest, setOrg)
	if len(got) != len(entries)*len(dest) {
		t.Fatalf("expected %d entries (cross product), got %d: %v", len(entries)*len(dest), len(got), got)
	}
	for _, e := range got {
		if e.OrgName != "my-org" && e.OrgName != "third-org" {
			t.Errorf("expected every entry retargeted onto a destination org, got: %+v", e)
		}
	}
}

func TestFilterByOrgKeepSet(t *testing.T) {
	entries := []fileOrgRef{
		{Region: "us10", OrgName: "org-a"},
		{Region: "us20", OrgName: "org-b"},
	}
	orgOf := func(e fileOrgRef) (string, string) { return e.Region, e.OrgName }

	if got := filterByOrgKeepSet(entries, nil, orgOf); len(got) != 2 {
		t.Errorf("nil keep set should return every entry unchanged, got: %v", got)
	}

	keep := map[string]bool{orgRefKey("us10", "org-a"): true}
	got := filterByOrgKeepSet(entries, keep, orgOf)
	if len(got) != 1 || got[0].OrgName != "org-a" {
		t.Errorf("expected only org-a to survive filtering, got: %v", got)
	}
}
