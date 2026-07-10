package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeleteOrgUsers_MissingUsersFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setupTestEnv(t, "http://fake-cf.example.com")
	_, _, err := runCmd(t, "delete-org-space-users")
	if err == nil {
		t.Fatal("expected error when CSV argument is not provided")
	}
}

func TestDeleteOrgUsers_InvalidUsersCSV(t *testing.T) {
	setupTestEnv(t, "http://fake-cf.example.com")

	// Write a file with the 9-column header but zero data rows.
	f := writeOspUsersCSV(t) // zero rows — just the header
	_, _, err := runCmd(t, "delete-org-space-users", f, "--yes")
	if err == nil {
		t.Fatal("expected error for CSV with no data rows")
	}
}

// TestDeleteOrgUsers_AutoConfirm_OrgLevel tests deletion of an org-level row
// (space_id empty). The cfuser_id ("u1") is present → FindCfUser is skipped.
// The 5-second sleep is skipped because there are no space rows alongside the org row.
func TestDeleteOrgUsers_AutoConfirm_OrgLevel(t *testing.T) {
	deleteCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/roles" && r.Method == "GET":
			// ListOrganizationUserRoles
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources": []map[string]interface{}{
					{"guid": "role1", "type": "organization_manager",
						"relationships": map[string]interface{}{
							"user":         map[string]interface{}{"data": map[string]string{"guid": "u1"}},
							"organization": map[string]interface{}{"data": map[string]string{"guid": "org1"}},
							"space":        map[string]interface{}{"data": nil},
						}},
				},
			})))
		case strings.HasPrefix(r.URL.Path, "/v3/roles/") && r.Method == "DELETE":
			deleteCount++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "no route: "+r.URL.Path+" "+r.Method, 404)
		}
	}))
	defer srv.Close()
	setupTestEnv(t, srv.URL)

	// Org-level row: space_id empty; cfuser_id="u1" → skip FindCfUser.
	usersFile := writeOspUsersCSV(t,
		[]string{srv.URL, "org1", "my-org", "", "", "u1", "alice@example.com", "sap.ids", "organization_manager"},
	)

	_, _, err := runCmd(t, "delete-org-space-users", usersFile, "--yes")
	if err != nil {
		t.Fatalf("delete-org-space-users --yes (org-level) failed: %v", err)
	}
	if deleteCount == 0 {
		t.Error("expected at least one DELETE to /v3/roles/{guid}")
	}
}

// TestDeleteOrgUsers_AutoConfirm_SpaceLevel tests deletion of a space-level row
// (space_id non-empty). cfuser_id is provided → FindCfUser is skipped.
// No org rows are present so no 5-second sleep occurs.
func TestDeleteOrgUsers_AutoConfirm_SpaceLevel(t *testing.T) {
	deleteCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/roles" && r.Method == "GET":
			// ListSpaceUserRoles
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources": []map[string]interface{}{
					{"guid": "role2", "type": "space_developer",
						"relationships": map[string]interface{}{
							"user":         map[string]interface{}{"data": map[string]string{"guid": "u1"}},
							"organization": map[string]interface{}{"data": nil},
							"space":        map[string]interface{}{"data": map[string]string{"guid": "sp1"}},
						}},
				},
			})))
		case strings.HasPrefix(r.URL.Path, "/v3/roles/") && r.Method == "DELETE":
			deleteCount++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "no route: "+r.URL.Path+" "+r.Method, 404)
		}
	}))
	defer srv.Close()
	setupTestEnv(t, srv.URL)

	// Space-level row: space_id = "sp1"; cfuser_id = "u1" → skip FindCfUser.
	usersFile := writeOspUsersCSV(t,
		[]string{srv.URL, "org1", "my-org", "sp1", "dev", "u1", "alice@example.com", "sap.ids", "space_developer"},
	)

	_, _, err := runCmd(t, "delete-org-space-users", usersFile, "--yes")
	if err != nil {
		t.Fatalf("delete-org-space-users --yes (space-level) failed: %v", err)
	}
	if deleteCount == 0 {
		t.Error("expected at least one DELETE to /v3/roles/{guid}")
	}
}

// TestDeleteOrgUsers_Broadcast_OrgLevel tests the 3-column broadcast CSV format
// where region/org_id are empty. The command discovers orgs and removes org roles.
func TestDeleteOrgUsers_Broadcast_OrgLevel(t *testing.T) {
	orgListCalled := false
	findUserCalled := false
	deleteCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/organizations" && r.Method == "GET":
			orgListCalled = true
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1, "next": nil},
				"resources":  []map[string]interface{}{{"guid": "org1", "name": "my-org"}},
			})))
		case r.URL.Path == "/v3/users" && r.Method == "GET":
			findUserCalled = true
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources":  []map[string]interface{}{{"guid": "u1", "username": "alice@example.com", "origin": "sap.ids"}},
			})))
		case r.URL.Path == "/v3/roles" && r.Method == "GET":
			w.Write([]byte(mustJSONStr(map[string]interface{}{ //nolint:errcheck
				"pagination": map[string]interface{}{"total_pages": 1},
				"resources": []map[string]interface{}{
					{"guid": "role1", "type": "organization_manager",
						"relationships": map[string]interface{}{
							"user":         map[string]interface{}{"data": map[string]string{"guid": "u1"}},
							"organization": map[string]interface{}{"data": map[string]string{"guid": "org1"}},
							"space":        map[string]interface{}{"data": nil},
						}},
				},
			})))
		case strings.HasPrefix(r.URL.Path, "/v3/roles/") && r.Method == "DELETE":
			deleteCount++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "no route: "+r.URL.Path+" "+r.Method, 404)
		}
	}))
	defer srv.Close()
	setupTestEnv(t, srv.URL)

	// 3-column broadcast CSV — no region/org targeting; cfuser_id absent → FindCfUser called.
	usersFile := writeBroadcastUsersCSV(t,
		[]string{"alice@example.com", "sap.ids", "organization_manager"},
	)

	_, _, err := runCmd(t, "delete-org-space-users", usersFile, "--yes")
	if err != nil {
		t.Fatalf("delete-org-space-users broadcast failed: %v", err)
	}
	if !orgListCalled {
		t.Error("expected GET /v3/organizations to be called for broadcast")
	}
	if !findUserCalled {
		t.Error("expected GET /v3/users to be called (no cfuser_id in broadcast CSV)")
	}
	if deleteCount == 0 {
		t.Error("expected at least one DELETE to /v3/roles/{guid}")
	}
}
