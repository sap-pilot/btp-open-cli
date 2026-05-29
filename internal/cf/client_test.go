package cf

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// singlePageOrgsHandler returns an httptest handler that serves one page of orgs.
func singlePageOrgsHandler(orgs []Organization) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/organizations" {
			http.Error(w, "unexpected path: "+r.URL.Path, 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		resp := orgsResponse{
			Pagination: pagination{TotalPages: 1},
			Resources:  orgs,
		}
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}
}

// TestListOrganizations_SinglePage verifies that a single-page response
// returns all organizations.
func TestListOrganizations_SinglePage(t *testing.T) {
	srv := httptest.NewServer(singlePageOrgsHandler([]Organization{
		{GUID: "g1", Name: "org-one"},
		{GUID: "g2", Name: "org-two"},
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	orgs, err := client.ListOrganizations(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(orgs) != 2 {
		t.Fatalf("expected 2 orgs, got %d", len(orgs))
	}
	if orgs[0].Name != "org-one" {
		t.Errorf("expected org-one, got %s", orgs[0].Name)
	}
	if orgs[1].GUID != "g2" {
		t.Errorf("expected g2, got %s", orgs[1].GUID)
	}
}

// TestListOrganizations_Pagination verifies that the client follows Next
// pagination links and returns all orgs from all pages.
func TestListOrganizations_Pagination(t *testing.T) {
	var srvURL string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			json.NewEncoder(w).Encode(orgsResponse{ //nolint:errcheck
				Pagination: pagination{TotalPages: 2},
				Resources:  []Organization{{GUID: "g2", Name: "org-two"}},
			})
		} else {
			json.NewEncoder(w).Encode(orgsResponse{ //nolint:errcheck
				Pagination: pagination{
					TotalPages: 2,
					Next:       &hrefObject{Href: srvURL + "/v3/organizations?page=2"},
				},
				Resources: []Organization{{GUID: "g1", Name: "org-one"}},
			})
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	client := NewClient(srv.URL, "test-token")
	orgs, err := client.ListOrganizations(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(orgs) != 2 {
		t.Fatalf("expected 2 orgs from 2 pages, got %d", len(orgs))
	}
	if orgs[0].Name != "org-one" || orgs[1].Name != "org-two" {
		t.Errorf("unexpected org names: %v, %v", orgs[0].Name, orgs[1].Name)
	}
}

// TestListOrganizations_AuthError verifies that a non-200 status is returned
// as an error (without a configured token refresher the 401 is propagated).
func TestListOrganizations_AuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "bad-token")
	_, err := client.ListOrganizations(context.Background())
	if err == nil {
		t.Fatal("expected error for 401, got nil")
	}
}

// TestListOrganizations_TokenRefresh verifies that when the server returns 401
// and a token refresher is set, the client retries with the new token.
func TestListOrganizations_TokenRefresh(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		auth := r.Header.Get("Authorization")
		if auth == "Bearer new-token" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(orgsResponse{ //nolint:errcheck
				Pagination: pagination{TotalPages: 1},
				Resources:  []Organization{{GUID: "g1", Name: "org-one"}},
			})
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "old-token")
	client.SetTokenRefresher(func(ctx context.Context) (string, error) {
		return "new-token", nil
	})

	orgs, err := client.ListOrganizations(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(orgs) != 1 {
		t.Errorf("expected 1 org, got %d", len(orgs))
	}
	if calls < 2 {
		t.Errorf("expected at least 2 HTTP calls (initial 401 + retry), got %d", calls)
	}
}

// TestListAllSpaces verifies that spaces are indexed by org GUID.
func TestListAllSpaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/spaces" {
			http.Error(w, "unexpected path", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		resp := spacesResponse{
			Pagination: pagination{TotalPages: 1},
			Resources: []Space{
				{
					GUID: "s1",
					Name: "dev",
					Relationships: spaceRelationships{
						Organization: struct {
							Data struct {
								GUID string `json:"guid"`
							} `json:"data"`
						}{Data: struct {
							GUID string `json:"guid"`
						}{GUID: "g1"}},
					},
				},
				{
					GUID: "s2",
					Name: "prod",
					Relationships: spaceRelationships{
						Organization: struct {
							Data struct {
								GUID string `json:"guid"`
							} `json:"data"`
						}{Data: struct {
							GUID string `json:"guid"`
						}{GUID: "g1"}},
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	byOrg, err := client.ListAllSpaces(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(byOrg["g1"]) != 2 {
		t.Errorf("expected 2 spaces for org g1, got %d", len(byOrg["g1"]))
	}
}
