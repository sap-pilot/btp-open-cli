package destination

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
			AccessToken: "dest-token",
			ExpiresIn:   3600,
		})
	}))
	defer srv.Close()
	overrideHTTPClient(t, srv)

	token, expiry, err := GetAccessToken(context.Background(), srv.URL, "client-id", "client-secret")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "dest-token" {
		t.Errorf("expected dest-token, got %s", token)
	}
	if expiry.IsZero() {
		t.Error("expected non-zero expiry")
	}
}

// TestGetAccessToken_Error verifies that a non-200 response is an error.
func TestGetAccessToken_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()
	overrideHTTPClient(t, srv)

	_, _, err := GetAccessToken(context.Background(), srv.URL, "bad-id", "bad-secret")
	if err == nil {
		t.Fatal("expected error for 403, got nil")
	}
}

// TestListInstanceDestinations verifies that the instance destinations are
// fetched and sensitive fields are redacted.
func TestListInstanceDestinations(t *testing.T) {
	raw := []map[string]interface{}{
		{"Name": "dest-one", "URL": "https://example.com", "ClientSecret": "secret-value"},
		{"Name": "dest-two", "URL": "https://example2.com"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/destination-configuration/v1/instanceDestinations" {
			http.Error(w, "unexpected path: "+r.URL.Path, 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(raw) //nolint:errcheck
	}))
	defer srv.Close()
	overrideHTTPClient(t, srv)

	dests, err := ListInstanceDestinations(context.Background(), srv.URL, "test-token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dests) != 2 {
		t.Fatalf("expected 2 destinations, got %d", len(dests))
	}
	if dests[0]["Name"] != "dest-one" {
		t.Errorf("expected dest-one, got %s", dests[0]["Name"])
	}
	// Sensitive field should be redacted
	if _, ok := dests[0]["ClientSecret"]; ok {
		t.Error("ClientSecret should have been redacted")
	}
}

// TestListInstanceDestinationsFull verifies that sensitive fields are NOT
// redacted when the Full variant is used.
func TestListInstanceDestinationsFull(t *testing.T) {
	raw := []map[string]interface{}{
		{"Name": "dest-one", "Password": "secret-pw"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/destination-configuration/v1/instanceDestinations" {
			http.Error(w, "unexpected path", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(raw) //nolint:errcheck
	}))
	defer srv.Close()
	overrideHTTPClient(t, srv)

	dests, err := ListInstanceDestinationsFull(context.Background(), srv.URL, "test-token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dests[0]["Password"] != "secret-pw" {
		t.Errorf("expected Password to be retained in Full mode, got %q", dests[0]["Password"])
	}
}

// TestListSubaccountDestinations verifies subaccount-level destination fetching.
func TestListSubaccountDestinations(t *testing.T) {
	raw := []map[string]interface{}{
		{"Name": "sub-dest", "URL": "https://example.com"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/destination-configuration/v1/subaccountDestinations" {
			http.Error(w, "unexpected path: "+r.URL.Path, 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(raw) //nolint:errcheck
	}))
	defer srv.Close()
	overrideHTTPClient(t, srv)

	dests, err := ListSubaccountDestinations(context.Background(), srv.URL, "test-token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dests) != 1 || dests[0]["Name"] != "sub-dest" {
		t.Errorf("unexpected destinations: %v", dests)
	}
}

// TestDeleteCountDeleted verifies the body-parsing helper used by delete responses.
func TestDeleteCountDeleted(t *testing.T) {
	cases := []struct {
		body    string
		want    bool
	}{
		{`{"Count":"1"}`, true},
		{`{"Count":1}`, true},
		{`{"Count":"0"}`, false},
		{`{"Count":0}`, false},
		{``, true},  // empty body → success
		{`{}`, true}, // no Count field → assume deleted
	}
	for _, tc := range cases {
		got := deleteCountDeleted([]byte(tc.body))
		if got != tc.want {
			t.Errorf("deleteCountDeleted(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}
