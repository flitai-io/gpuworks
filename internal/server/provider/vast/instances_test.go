package vast

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func writeInstancePage(w http.ResponseWriter, first, count, total int, next any) {
	instances := make([]map[string]any, 0, count)
	for id := first; id < first+count; id++ {
		instances = append(instances, map[string]any{"id": id, "actual_status": "running"})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true, "instances": instances, "instances_found": count,
		"total_instances": total, "next_token": next,
	})
}

func TestListInstancesV1ReadsEveryPage(t *testing.T) {
	for _, total := range []int{0, 1, 25, 26, 51} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			calls := 0
			d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/instances/" {
					http.Error(w, "deprecated endpoint", http.StatusGone)
					return
				}
				if r.Header.Get("Authorization") != "Bearer test-key" || r.URL.Query().Get("limit") != "25" {
					t.Errorf("unexpected authentication or page limit")
				}
				offset := calls * 25
				if calls > 0 && r.URL.Query().Get("after_token") != fmt.Sprintf("cursor+/%d=", offset) {
					t.Errorf("cursor was not round-tripped: %q", r.URL.Query().Get("after_token"))
				}
				calls++
				count := min(25, total-offset)
				var next any
				if offset+count < total {
					next = fmt.Sprintf("cursor+/%d=", offset+count)
				}
				writeInstancePage(w, offset+1, count, total, next)
			})
			got, err := d.ListInstances(t.Context(), provider.Credentials{APIKey: "test-key"})
			if err != nil || len(got) != total || calls != max(1, (total+24)/25) {
				t.Fatalf("instances=%d, calls=%d, err=%v", len(got), calls, err)
			}
			for i, in := range got {
				if in.ExternalID != strconv.Itoa(i+1) {
					t.Fatalf("instance %d = %q", i, in.ExternalID)
				}
			}
		})
	}
}

func TestListInstancesV1NeverReturnsPartialResults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"http failure", 502, `{"msg":"unavailable"}`},
		{"rate limited", 429, `{"detail":"too frequent"}`},
		{"api failure", 200, `{"success":false,"instances":[],"next_token":null,"instances_found":0,"total_instances":26}`},
		{"malformed", 200, `{`},
		{"missing instances", 200, `{"success":true,"next_token":null,"instances_found":0,"total_instances":26}`},
		{"missing cursor", 200, `{"success":true,"instances":[],"instances_found":0,"total_instances":26}`},
		{"short listing", 200, `{"success":true,"instances":[],"next_token":null,"instances_found":0,"total_instances":26}`},
		{"repeated cursor", 200, `{"success":true,"instances":[{"id":26}],"next_token":"next","instances_found":1,"total_instances":27}`},
		{"repeated instance", 200, `{"success":true,"instances":[{"id":1}],"next_token":null,"instances_found":1,"total_instances":26}`},
		{"missing id", 200, `{"success":true,"instances":[{}],"next_token":null,"instances_found":1,"total_instances":26}`},
		{"wrong page count", 200, `{"success":true,"instances":[{"id":26}],"next_token":null,"instances_found":0,"total_instances":26}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					writeInstancePage(w, 1, 25, 26, "next")
					return
				}
				if calls > 2 {
					t.Error("pagination continued after an invalid second page")
					http.Error(w, "stop", 500)
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			got, err := d.ListInstances(t.Context(), provider.Credentials{APIKey: "test-key"})
			if err == nil || got != nil || calls != 2 {
				t.Fatalf("instances=%v, calls=%d, err=%v", got, calls, err)
			}
		})
	}
}

func TestListInstancesV1CancellationDiscardsFirstPage(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			writeInstancePage(w, 1, 25, 26, "next")
			return
		}
		cancel()
		<-r.Context().Done()
	})
	got, err := d.ListInstances(ctx, provider.Credentials{APIKey: "test-key"})
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("instances=%v, err=%v", got, err)
	}
}

func TestVastInstanceQueriesKeepOtherOperationsAndEndpoint(t *testing.T) {
	for _, tc := range []struct {
		endpointPath string
		listPath     string
	}{
		{"/api/v0", "/api/v1/instances/"},
		{"/api/v0/", "/api/v1/instances/"},
		{"/proxy/api/v0", "/proxy/api/v1/instances/"},
		{"/api/v1", "/api/v1/instances/"},
		{"/api/v2", "/api/v2/instances/"},
		{"/api/v2/", "/api/v2/instances/"},
		{"/proxy/api/v2", "/proxy/api/v2/instances/"},
		{"/api/v01", "/api/v01/instances/"},
		{"/gateway", "/gateway/instances/"},
		{"", "/instances/"},
	} {
		t.Run(tc.endpointPath, func(t *testing.T) {
			paths := []string{}
			d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				if r.Header.Get("Authorization") != "Bearer test-key" {
					t.Error("missing authorization")
				}
				switch {
				case r.Method == http.MethodGet && r.URL.Path == tc.listPath:
					if r.URL.Query().Get("limit") != "1" {
						t.Error("credential check should request only one instance")
					}
					writeInstancePage(w, 1, 1, 26, "another-page")
				case r.Method == http.MethodDelete && r.URL.Path == strings.TrimRight(tc.endpointPath, "/")+"/instances/7/":
					_, _ = w.Write([]byte(`{"success":true}`))
				default:
					http.Error(w, "unexpected route", 404)
				}
			})
			creds := provider.Credentials{Endpoint: strings.TrimSuffix(d.baseURL, "/api/v0") + tc.endpointPath, APIKey: "test-key"}
			d.baseURL = "https://must-not-be-used.invalid/api/v0"
			if err := d.ValidateCredentials(t.Context(), creds); err != nil {
				t.Fatal(err)
			}
			if err := d.DeleteInstance(t.Context(), creds, "7"); err != nil {
				t.Fatal(err)
			}
			if len(paths) != 2 {
				t.Fatalf("credential check followed extra pages: %v", paths)
			}
		})
	}
}

// The v1 API filters on the server; returning an entire fixture for an ID
// query would hide a missing filter or missing-instance handling in the driver.
func writeFilteredInstanceFixture(t *testing.T, w http.ResponseWriter, r *http.Request, body string) {
	t.Helper()
	var filter struct {
		ID struct {
			Eq json.Number `json:"eq"`
		} `json:"id"`
	}
	if r.URL.Path != "/api/v1/instances/" || r.URL.Query().Get("limit") != "1" || json.Unmarshal([]byte(r.URL.Query().Get("select_filters")), &filter) != nil || filter.ID.Eq == "" {
		t.Error("expected a single-instance query with a numeric ID filter")
		http.Error(w, "invalid filter", 400)
		return
	}
	var fixture struct {
		Instances []map[string]json.RawMessage `json:"instances"`
	}
	if err := json.Unmarshal([]byte(body), &fixture); err != nil {
		t.Error(err)
		http.Error(w, "bad fixture", 500)
		return
	}
	matches := make([]map[string]json.RawMessage, 0)
	for _, in := range fixture.Instances {
		if string(in["id"]) == filter.ID.Eq.String() {
			matches = append(matches, in)
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true, "instances": matches, "instances_found": len(matches),
		"total_instances": len(matches), "next_token": nil,
	})
}

func TestInstanceByIDRejectsUnfilteredResponse(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		writeInstancePage(w, 7, 1, 1, nil)
	})
	if _, err := d.PowerState(t.Context(), provider.Credentials{APIKey: "test-key"}, "8"); err == nil {
		t.Fatal("accepted an unrelated instance when the server ignored the filter")
	}
}
