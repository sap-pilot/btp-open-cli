package xsuaa

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// overrideHTTPClient replaces the package-level httpClient with the test
// server's client and restores it when the test finishes.
func overrideHTTPClient(t *testing.T, srv *httptest.Server) {
	t.Helper()
	old := httpClient
	httpClient = srv.Client()
	t.Cleanup(func() { httpClient = old })
}

// TestGetAccessToken verifies the OAuth2 client_credentials flow.
func TestGetAccessToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method not allowed", 405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tokenResponse{ //nolint:errcheck
			AccessToken: "xsuaa-token",
			ExpiresIn:   3600,
		})
	}))
	defer srv.Close()
	overrideHTTPClient(t, srv)

	token, expiry, err := GetAccessToken(context.Background(), srv.URL, "client-id", "client-secret")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "xsuaa-token" {
		t.Errorf("expected xsuaa-token, got %s", token)
	}
	if expiry.IsZero() {
		t.Error("expected non-zero expiry")
	}
}

// TestGetAccessToken_Error verifies that a non-200 response is returned as an error.
func TestGetAccessToken_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()
	overrideHTTPClient(t, srv)

	_, _, err := GetAccessToken(context.Background(), srv.URL, "bad-id", "bad-secret")
	if err == nil {
		t.Fatal("expected error for 401, got nil")
	}
}

// TestListUsers_SinglePage verifies that a single-page SCIM response returns all users.
// Note: ListUsers creates its own http.Client internally but uses apiBaseURL as the
// target, so a plain-HTTP httptest server URL is passed directly.
func TestListUsers_SinglePage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Users" {
			http.Error(w, "unexpected path: "+r.URL.Path, 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(usersPage{ //nolint:errcheck
			TotalResults: 2,
			StartIndex:   1,
			ItemsPerPage: 500,
			Resources: []User{
				{ID: "u1", UserName: "alice@example.com", Origin: "sap.ids"},
				{ID: "u2", UserName: "bob@example.com", Origin: "uaa"},
			},
		})
	}))
	defer srv.Close()

	users, err := ListUsers(context.Background(), srv.URL, "test-token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}
	if users[0].UserName != "alice@example.com" {
		t.Errorf("expected alice@example.com, got %s", users[0].UserName)
	}
}

// TestListUsers_Pagination verifies that the SCIM startIndex loop fetches all pages.
func TestListUsers_Pagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Users" {
			http.Error(w, "unexpected path", 404)
			return
		}
		startIndex := r.URL.Query().Get("startIndex")
		w.Header().Set("Content-Type", "application/json")
		if startIndex == "2" {
			json.NewEncoder(w).Encode(usersPage{ //nolint:errcheck
				TotalResults: 2,
				StartIndex:   2,
				ItemsPerPage: 1,
				Resources:    []User{{ID: "u2", UserName: "bob@example.com"}},
			})
		} else {
			// First page: 1 of 2 results
			json.NewEncoder(w).Encode(usersPage{ //nolint:errcheck
				TotalResults: 2,
				StartIndex:   1,
				ItemsPerPage: 1,
				Resources:    []User{{ID: "u1", UserName: "alice@example.com"}},
			})
		}
	}))
	defer srv.Close()

	users, err := ListUsers(context.Background(), srv.URL, "test-token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 users across 2 pages, got %d", len(users))
	}
}

// TestPrimaryEmail verifies that PrimaryEmail returns the first email value.
func TestPrimaryEmail(t *testing.T) {
	emails := []Email{
		{Value: "alice@example.com", Primary: true},
		{Value: "alice2@example.com", Primary: false},
	}
	if got := PrimaryEmail(emails); got != "alice@example.com" {
		t.Errorf("expected alice@example.com, got %s", got)
	}
	if got := PrimaryEmail(nil); got != "" {
		t.Errorf("expected empty string for nil, got %q", got)
	}
}
