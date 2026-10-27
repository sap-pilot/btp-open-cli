package cf

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func init() {
	// Keep build polling fast in tests regardless of the production default.
	buildPollInterval = time.Millisecond
}

// TestCreateDeployment verifies the request body omits "droplet" when no
// dropletGUID is given (redeploying the app's current droplet in place).
func TestCreateDeployment(t *testing.T) {
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/deployments" || r.Method != http.MethodPost {
			http.Error(w, "unexpected request: "+r.Method+" "+r.URL.Path, 404)
			return
		}
		json.NewDecoder(r.Body).Decode(&gotBody) //nolint:errcheck
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"guid":"deployment-1"}`)) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	guid, err := client.CreateDeployment(context.Background(), "app-1", "rolling", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if guid != "deployment-1" {
		t.Errorf("expected deployment-1, got %s", guid)
	}
	if gotBody["strategy"] != "rolling" {
		t.Errorf("expected strategy rolling, got %v", gotBody["strategy"])
	}
	if _, ok := gotBody["droplet"]; ok {
		t.Errorf("expected no droplet field when dropletGUID is empty, got %v", gotBody["droplet"])
	}
	rel, _ := gotBody["relationships"].(map[string]interface{})
	app, _ := rel["app"].(map[string]interface{})
	data, _ := app["data"].(map[string]interface{})
	if data["guid"] != "app-1" {
		t.Errorf("expected app guid app-1, got %v", data["guid"])
	}
}

// TestCreateDeployment_WithDroplet verifies a non-empty dropletGUID is sent
// as the "droplet" field (used to roll out a specific, newly-built droplet).
func TestCreateDeployment_WithDroplet(t *testing.T) {
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody) //nolint:errcheck
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"guid":"deployment-2"}`)) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	if _, err := client.CreateDeployment(context.Background(), "app-1", "canary", "droplet-9"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	droplet, _ := gotBody["droplet"].(map[string]interface{})
	if droplet["guid"] != "droplet-9" {
		t.Errorf("expected droplet guid droplet-9, got %v", droplet["guid"])
	}
}

// TestRestageAppWithStrategy_Success walks the full build-then-deploy flow:
// fetch the current droplet's package, create a build, poll it until
// STAGED, then deploy the resulting droplet with the given strategy.
func TestRestageAppWithStrategy_Success(t *testing.T) {
	var srvURL string
	var buildPolls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/apps/app-1/droplets/current" && r.Method == http.MethodGet:
			w.Write([]byte(`{"guid":"droplet-old","state":"STAGED","links":{"package":{"href":"` + srvURL + `/v3/packages/pkg-1"}}}`)) //nolint:errcheck

		case r.URL.Path == "/v3/builds" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"guid":"build-1","state":"STAGING"}`)) //nolint:errcheck

		case r.URL.Path == "/v3/builds/build-1" && r.Method == http.MethodGet:
			buildPolls++
			if buildPolls < 2 {
				w.Write([]byte(`{"guid":"build-1","state":"STAGING"}`)) //nolint:errcheck
				return
			}
			w.Write([]byte(`{"guid":"build-1","state":"STAGED","droplet":{"guid":"droplet-new"}}`)) //nolint:errcheck

		case r.URL.Path == "/v3/deployments" && r.Method == http.MethodPost:
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
			droplet, _ := body["droplet"].(map[string]interface{})
			if droplet["guid"] != "droplet-new" {
				t.Errorf("expected deployment to use the new droplet, got %v", droplet)
			}
			if body["strategy"] != "rolling" {
				t.Errorf("expected strategy rolling, got %v", body["strategy"])
			}
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"guid":"deployment-1"}`)) //nolint:errcheck

		default:
			http.Error(w, "unexpected request: "+r.Method+" "+r.URL.Path, 404)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	client := NewClient(srv.URL, "test-token")
	if err := client.RestageAppWithStrategy(context.Background(), "app-1", "rolling"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if buildPolls < 2 {
		t.Errorf("expected at least 2 build polls, got %d", buildPolls)
	}
}

// TestRestageApp_Success walks the no-strategy restage flow: fetch the
// current droplet's package, create a build, poll it until STAGED, assign
// the new droplet as current, then restart the app.
func TestRestageApp_Success(t *testing.T) {
	var srvURL string
	var buildPolls int
	var patchedDroplet, restarted string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/apps/app-1/droplets/current" && r.Method == http.MethodGet:
			w.Write([]byte(`{"guid":"droplet-old","state":"STAGED","links":{"package":{"href":"` + srvURL + `/v3/packages/pkg-1"}}}`)) //nolint:errcheck

		case r.URL.Path == "/v3/builds" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"guid":"build-1","state":"STAGING"}`)) //nolint:errcheck

		case r.URL.Path == "/v3/builds/build-1" && r.Method == http.MethodGet:
			buildPolls++
			if buildPolls < 2 {
				w.Write([]byte(`{"guid":"build-1","state":"STAGING"}`)) //nolint:errcheck
				return
			}
			w.Write([]byte(`{"guid":"build-1","state":"STAGED","droplet":{"guid":"droplet-new"}}`)) //nolint:errcheck

		case r.URL.Path == "/v3/apps/app-1/relationships/current_droplet" && r.Method == http.MethodPatch:
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
			data, _ := body["data"].(map[string]interface{})
			patchedDroplet, _ = data["guid"].(string)
			w.Write([]byte(`{}`)) //nolint:errcheck

		case r.URL.Path == "/v3/apps/app-1/actions/restart" && r.Method == http.MethodPost:
			restarted = "yes"
			w.Write([]byte(`{"guid":"app-1","state":"STARTED"}`)) //nolint:errcheck

		default:
			http.Error(w, "unexpected request: "+r.Method+" "+r.URL.Path, 404)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	client := NewClient(srv.URL, "test-token")
	app, err := client.RestageApp(context.Background(), "app-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if app.State != "STARTED" {
		t.Errorf("expected returned app state STARTED, got %s", app.State)
	}
	if patchedDroplet != "droplet-new" {
		t.Errorf("expected current_droplet patched to droplet-new, got %q", patchedDroplet)
	}
	if restarted != "yes" {
		t.Error("expected the app to be restarted after assigning the new droplet")
	}
	if buildPolls < 2 {
		t.Errorf("expected at least 2 build polls, got %d", buildPolls)
	}
}

// TestRestageAppWithStrategy_BuildFails verifies a FAILED build surfaces an
// error instead of proceeding to deploy.
func TestRestageAppWithStrategy_BuildFails(t *testing.T) {
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/apps/app-1/droplets/current":
			w.Write([]byte(`{"guid":"droplet-old","links":{"package":{"href":"` + srvURL + `/v3/packages/pkg-1"}}}`)) //nolint:errcheck
		case r.URL.Path == "/v3/builds" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"guid":"build-1","state":"FAILED"}`)) //nolint:errcheck
		default:
			http.Error(w, "unexpected request: "+r.Method+" "+r.URL.Path, 404)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	client := NewClient(srv.URL, "test-token")
	if err := client.RestageAppWithStrategy(context.Background(), "app-1", "rolling"); err == nil {
		t.Fatal("expected an error for a failed build")
	}
}

func TestPackageGUIDFromHref(t *testing.T) {
	guid, err := packageGUIDFromHref("https://api.example.org/v3/packages/abc-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if guid != "abc-123" {
		t.Errorf("expected abc-123, got %s", guid)
	}

	if _, err := packageGUIDFromHref("no-slash"); err == nil {
		t.Error("expected an error for a href with no slash")
	}
	if _, err := packageGUIDFromHref("https://api.example.org/v3/packages/"); err == nil {
		t.Error("expected an error for a href ending in a slash")
	}
}
