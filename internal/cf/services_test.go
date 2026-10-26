package cf

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func init() {
	// Keep job polling fast in tests regardless of the production default.
	serviceCredentialBindingPollInterval = time.Millisecond
}

// TestCreateServiceCredentialBinding_Synchronous verifies the 201 Created
// path (broker binds synchronously) returns the binding GUID directly.
func TestCreateServiceCredentialBinding_Synchronous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/service_credential_bindings" || r.Method != http.MethodPost {
			http.Error(w, "unexpected request: "+r.Method+" "+r.URL.Path, 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"guid":"binding-1"}`)) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	guid, err := client.CreateServiceCredentialBinding(context.Background(), "my-key", "inst-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if guid != "binding-1" {
		t.Errorf("expected binding-1, got %s", guid)
	}
}

// TestCreateServiceCredentialBinding_AsyncCompletes verifies the 202
// Accepted path: the job is polled until COMPLETE, then the binding is
// looked up by name to obtain its GUID.
func TestCreateServiceCredentialBinding_AsyncCompletes(t *testing.T) {
	var jobPolls int
	var srvURL string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/service_credential_bindings" && r.Method == http.MethodPost:
			w.Header().Set("Location", srvURL+"/v3/jobs/job-1")
			w.WriteHeader(http.StatusAccepted)

		case r.URL.Path == "/v3/jobs/job-1":
			jobPolls++
			if jobPolls < 2 {
				w.Write([]byte(`{"state":"PROCESSING"}`)) //nolint:errcheck
				return
			}
			w.Write([]byte(`{"state":"COMPLETE"}`)) //nolint:errcheck

		case r.URL.Path == "/v3/service_credential_bindings" && r.Method == http.MethodGet:
			w.Write([]byte(`{"pagination":{"total_pages":1},"resources":[{"guid":"binding-2","name":"my-key"}]}`)) //nolint:errcheck

		default:
			http.Error(w, "unexpected request: "+r.Method+" "+r.URL.Path, 404)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	client := NewClient(srv.URL, "test-token")
	guid, err := client.CreateServiceCredentialBinding(context.Background(), "my-key", "inst-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if guid != "binding-2" {
		t.Errorf("expected binding-2, got %s", guid)
	}
	if jobPolls < 2 {
		t.Errorf("expected at least 2 job polls, got %d", jobPolls)
	}
}

// TestCreateServiceCredentialBinding_AsyncFails verifies a FAILED job surfaces
// the job's own error detail.
func TestCreateServiceCredentialBinding_AsyncFails(t *testing.T) {
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v3/service_credential_bindings" && r.Method == http.MethodPost:
			w.Header().Set("Location", srvURL+"/v3/jobs/job-1")
			w.WriteHeader(http.StatusAccepted)
		case r.URL.Path == "/v3/jobs/job-1":
			w.Write([]byte(`{"state":"FAILED","errors":[{"detail":"broker rejected binding"}]}`)) //nolint:errcheck
		default:
			http.Error(w, "unexpected request: "+r.Method+" "+r.URL.Path, 404)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	client := NewClient(srv.URL, "test-token")
	_, err := client.CreateServiceCredentialBinding(context.Background(), "my-key", "inst-1")
	if err == nil {
		t.Fatal("expected an error for a failed job")
	}
	if !strings.Contains(err.Error(), "broker rejected binding") {
		t.Errorf("expected error to include job detail, got: %v", err)
	}
}

// TestDeleteServiceCredentialBinding_Synchronous verifies a 204 No Content
// response is treated as success with no job to poll.
func TestDeleteServiceCredentialBinding_Synchronous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "unexpected method: "+r.Method, 404)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	if err := client.DeleteServiceCredentialBinding(context.Background(), "binding-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDeleteServiceCredentialBinding_Async verifies the 202 Accepted path
// polls the job to completion before returning.
func TestDeleteServiceCredentialBinding_Async(t *testing.T) {
	var jobPolls int
	var srvURL string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete:
			w.Header().Set("Location", srvURL+"/v3/jobs/del-job-1")
			w.WriteHeader(http.StatusAccepted)
		case r.URL.Path == "/v3/jobs/del-job-1":
			jobPolls++
			if jobPolls < 2 {
				w.Write([]byte(`{"state":"PROCESSING"}`)) //nolint:errcheck
				return
			}
			w.Write([]byte(`{"state":"COMPLETE"}`)) //nolint:errcheck
		default:
			http.Error(w, "unexpected request: "+r.Method+" "+r.URL.Path, 404)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	client := NewClient(srv.URL, "test-token")
	if err := client.DeleteServiceCredentialBinding(context.Background(), "binding-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if jobPolls < 2 {
		t.Errorf("expected at least 2 job polls, got %d", jobPolls)
	}
}

// TestDeleteServiceInstance_Synchronous verifies a 204 No Content response
// (user-provided instance) is treated as success with no job to poll.
func TestDeleteServiceInstance_Synchronous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v3/service_instances/inst-1" {
			http.Error(w, "unexpected request: "+r.Method+" "+r.URL.Path, 404)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	if err := client.DeleteServiceInstance(context.Background(), "inst-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDeleteServiceInstance_Async verifies the 202 Accepted path (managed
// instance) polls the job to completion before returning.
func TestDeleteServiceInstance_Async(t *testing.T) {
	var jobPolls int
	var srvURL string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/v3/service_instances/inst-1":
			w.Header().Set("Location", srvURL+"/v3/jobs/del-inst-job-1")
			w.WriteHeader(http.StatusAccepted)
		case r.URL.Path == "/v3/jobs/del-inst-job-1":
			jobPolls++
			if jobPolls < 2 {
				w.Write([]byte(`{"state":"PROCESSING"}`)) //nolint:errcheck
				return
			}
			w.Write([]byte(`{"state":"COMPLETE"}`)) //nolint:errcheck
		default:
			http.Error(w, "unexpected request: "+r.Method+" "+r.URL.Path, 404)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	client := NewClient(srv.URL, "test-token")
	if err := client.DeleteServiceInstance(context.Background(), "inst-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if jobPolls < 2 {
		t.Errorf("expected at least 2 job polls, got %d", jobPolls)
	}
}

// TestDeleteServiceInstance_Failure verifies a non-2xx/202 response surfaces
// as an *APIError (e.g. CF refusing because bindings still exist).
func TestDeleteServiceInstance_Failure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"errors":[{"detail":"instance still has bindings"}]}`)) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	err := client.DeleteServiceInstance(context.Background(), "inst-1")
	if err == nil {
		t.Fatal("expected an error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
}

// TestListServiceCredentialBindingsByInstances verifies bindings are listed
// and correctly attributed to their owning service instance via
// relationships.service_instance.
func TestListServiceCredentialBindingsByInstances(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/service_credential_bindings" || r.Method != http.MethodGet {
			http.Error(w, "unexpected request: "+r.Method+" "+r.URL.Path, 404)
			return
		}
		guids := r.URL.Query().Get("service_instance_guids")
		if !strings.Contains(guids, "inst-1") || !strings.Contains(guids, "inst-2") {
			http.Error(w, "expected both instance guids in query, got: "+guids, 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"pagination":{"total_pages":1},"resources":[` + //nolint:errcheck
			`{"guid":"b1","name":"key1","type":"key","relationships":{"service_instance":{"data":{"guid":"inst-1"}}}},` +
			`{"guid":"b2","name":"app-binding","type":"app","relationships":{"service_instance":{"data":{"guid":"inst-2"}}}}` +
			`]}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	bindings, err := client.ListServiceCredentialBindingsByInstances(context.Background(), []string{"inst-1", "inst-2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bindings) != 2 {
		t.Fatalf("expected 2 bindings, got %d", len(bindings))
	}
	byGUID := make(map[string]ServiceCredentialBindingFull, len(bindings))
	for _, b := range bindings {
		byGUID[b.GUID] = b
	}
	if byGUID["b1"].Relationships.ServiceInstance.Data.GUID != "inst-1" || byGUID["b1"].Type != "key" {
		t.Errorf("binding b1 attributed incorrectly: %+v", byGUID["b1"])
	}
	if byGUID["b2"].Relationships.ServiceInstance.Data.GUID != "inst-2" || byGUID["b2"].Type != "app" {
		t.Errorf("binding b2 attributed incorrectly: %+v", byGUID["b2"])
	}
}
