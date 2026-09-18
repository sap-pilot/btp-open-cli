package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"btp-open-cli/internal/store"
)

// multiOrgPage returns a JSON CF v3 organizations response with several orgs.
func multiOrgPage(orgs ...[2]string) string {
	resources := make([]map[string]string, len(orgs))
	for i, o := range orgs {
		resources[i] = map[string]string{"guid": o[0], "name": o[1]}
	}
	return mustJSONStr(map[string]interface{}{
		"pagination": map[string]interface{}{"total_pages": 1},
		"resources":  resources,
	})
}

func credsForAPIURLs(apiURLs ...string) *store.Credentials {
	tokens := make(map[string]store.RegionToken)
	for _, u := range apiURLs {
		tokens[u] = store.RegionToken{APIURL: u, AccessToken: "test-token", TokenType: "bearer"}
	}
	return &store.Credentials{ActiveAPIURLs: apiURLs, Tokens: tokens}
}

func TestListAccessibleOrgs_SortedByRegionThenName(t *testing.T) {
	srvA := fakeCFServer(t, map[string]string{
		"/v3/organizations": multiOrgPage([2]string{"g2", "zeta"}, [2]string{"g1", "alpha"}),
	})
	srvB := fakeCFServer(t, map[string]string{
		"/v3/organizations": multiOrgPage([2]string{"g3", "beta"}),
	})
	// APIURLToRegion falls back to the raw URL for non-"api.cf.<region>" hosts,
	// so these two fake servers act as two distinct "regions" for sorting.
	creds := credsForAPIURLs(srvA.URL, srvB.URL)

	choices, err := listAccessibleOrgs(context.Background(), creds, []string{srvA.URL, srvB.URL})
	if err != nil {
		t.Fatalf("listAccessibleOrgs failed: %v", err)
	}
	if len(choices) != 3 {
		t.Fatalf("expected 3 orgs, got %d: %+v", len(choices), choices)
	}

	// Within the same region (srvA), orgs must be sorted by name: alpha before zeta.
	var alphaIdx, zetaIdx = -1, -1
	for i, c := range choices {
		if c.Region != srvA.URL {
			continue
		}
		switch c.Name {
		case "alpha":
			alphaIdx = i
		case "zeta":
			zetaIdx = i
		}
	}
	if alphaIdx == -1 || zetaIdx == -1 {
		t.Fatalf("expected both alpha and zeta from srvA, got: %+v", choices)
	}
	if alphaIdx > zetaIdx {
		t.Errorf("expected alpha to sort before zeta within the same region, got order: %+v", choices)
	}
}

func TestListAccessibleOrgs_NoAccessibleOrgs(t *testing.T) {
	srv := fakeCFServer(t, map[string]string{
		"/v3/organizations": emptyPage(),
	})
	creds := credsForAPIURLs(srv.URL)

	_, err := listAccessibleOrgs(context.Background(), creds, []string{srv.URL})
	if err == nil {
		t.Fatal("expected error when no orgs are accessible")
	}
}

func TestSelectOrgsInteractive_NonTTYError(t *testing.T) {
	choices := []orgChoice{{Region: "us10", ID: "g1", Name: "my-org"}}
	// The test process's stdin is not an interactive terminal, so the prompt
	// must fail fast instead of blocking on a read that will never resolve.
	_, err := selectOrgsInteractive(context.Background(), choices)
	if err == nil {
		t.Fatal("expected error when stdin is not a terminal")
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Errorf("expected error to mention 'terminal', got: %v", err)
	}
}

func TestSelectOrgsInteractive_NoChoices(t *testing.T) {
	_, err := selectOrgsInteractive(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for empty choices")
	}
}

func TestResolveDefaultOrgScope_Empty(t *testing.T) {
	creds := &store.Credentials{}
	_, err := resolveDefaultOrgScope(creds)
	if err == nil {
		t.Fatal("expected error when no default org scope is set")
	}
	if !strings.Contains(err.Error(), "bo orgs") {
		t.Errorf("expected error to mention 'bo orgs', got: %v", err)
	}
}

func TestResolveDefaultOrgScope_Set(t *testing.T) {
	creds := &store.Credentials{
		DefaultOrgScope: []store.OrgScopeRef{
			{Region: "us10", ID: "g1", Name: "my-org", APIURL: "https://api.cf.us10.hana.ondemand.com"},
		},
	}
	set, err := resolveDefaultOrgScope(creds)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(set) != 1 || set[0].ID != "g1" || set[0].APIURL != "https://api.cf.us10.hana.ondemand.com" {
		t.Errorf("unexpected resolved scope: %+v", set)
	}
}

func TestResolveOrgTargets_OrgFlagTakesPrecedence(t *testing.T) {
	creds := &store.Credentials{
		DefaultOrgScope: []store.OrgScopeRef{{ID: "should-not-be-used"}},
	}
	targets, err := resolveOrgTargets(creds, "my-org-flag", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 1 || targets[0] != "my-org-flag" {
		t.Errorf("expected [my-org-flag], got: %+v", targets)
	}
}

func TestResolveOrgTargets_DefaultScope(t *testing.T) {
	creds := &store.Credentials{
		DefaultOrgScope: []store.OrgScopeRef{
			{ID: "g1", Name: "org-one"},
			{ID: "g2", Name: "org-two"},
		},
	}
	targets, err := resolveOrgTargets(creds, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 2 || targets[0] != "g1" || targets[1] != "g2" {
		t.Errorf("expected [g1 g2], got: %+v", targets)
	}
}

func TestResolveOrgTargets_NoScopeNoFlags(t *testing.T) {
	creds := &store.Credentials{}
	_, err := resolveOrgTargets(creds, "", "")
	if err == nil {
		t.Fatal("expected error when no --org/--orgs and no default scope")
	}
}

func TestParseCosOrgCSV_AcceptsEitherColumnOrder(t *testing.T) {
	cases := []struct {
		name   string
		header string
		row    string
	}{
		{"legacy order", "region,org_id,org_name", "us10,g1,my-org"},
		{"orgs-picker order", "region,org_name,org_id", "us10,my-org,g1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "orgs.csv")
			content := c.header + "\n" + c.row + "\n"
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			set, err := parseCosOrgCSV(path)
			if err != nil {
				t.Fatalf("parseCosOrgCSV failed: %v", err)
			}
			if len(set) != 1 {
				t.Fatalf("expected 1 ref, got %d", len(set))
			}
			got := set[0]
			if got.Region != "us10" || got.ID != "g1" || got.Name != "my-org" {
				t.Errorf("unexpected parsed ref: %+v", got)
			}
		})
	}
}

func TestParseCosOrgCSV_MissingColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orgs.csv")
	if err := os.WriteFile(path, []byte("region,org_name\nus10,my-org\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := parseCosOrgCSV(path)
	if err == nil {
		t.Fatal("expected error when org_id column is missing")
	}
}

func TestOrgNumWidth(t *testing.T) {
	cases := []struct {
		n    int
		want int
	}{
		{0, 1}, {1, 1}, {9, 1},
		{10, 2}, {50, 2}, {99, 2},
		{100, 3}, {999, 3},
	}
	for _, c := range cases {
		if got := orgNumWidth(c.n); got != c.want {
			t.Errorf("orgNumWidth(%d) = %d, want %d", c.n, got, c.want)
		}
	}
}
