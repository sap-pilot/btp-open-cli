package cf

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetAppEnvVars(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/apps/app1/environment_variables" {
			http.Error(w, "unexpected path: "+r.URL.Path, 404)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "unexpected method: "+r.Method, 405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(appEnvVarsResponse{ //nolint:errcheck
			Var: map[string]string{"FOO": "bar"},
		})
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	vars, err := client.GetAppEnvVars(context.Background(), "app1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vars["FOO"] != "bar" {
		t.Errorf("expected FOO=bar, got %+v", vars)
	}
}

func TestUpdateAppEnvVars(t *testing.T) {
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/apps/app1/environment_variables" {
			http.Error(w, "unexpected path: "+r.URL.Path, 404)
			return
		}
		if r.Method != http.MethodPatch {
			http.Error(w, "unexpected method: "+r.Method, 405)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(appEnvVarsResponse{ //nolint:errcheck
			Var: map[string]string{"FOO": "bar", "BAZ": "qux"},
		})
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	vars, err := client.UpdateAppEnvVars(context.Background(), "app1", map[string]interface{}{
		"FOO": "bar",
		"BAZ": "qux",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vars["FOO"] != "bar" || vars["BAZ"] != "qux" {
		t.Errorf("expected FOO=bar, BAZ=qux in response, got %+v", vars)
	}

	sent, ok := gotBody["var"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected request body to have a \"var\" object, got %+v", gotBody)
	}
	if sent["FOO"] != "bar" || sent["BAZ"] != "qux" {
		t.Errorf("expected request body var to be {FOO: bar, BAZ: qux}, got %+v", sent)
	}
}

func TestUpdateAppEnvVars_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"errors":[{"detail":"App not found"}]}`, http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	_, err := client.UpdateAppEnvVars(context.Background(), "missing-app", map[string]interface{}{"FOO": "bar"})
	if err == nil {
		t.Fatal("expected an error for a 404 response")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", apiErr.StatusCode)
	}
}
