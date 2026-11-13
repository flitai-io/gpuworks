package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestProviderEditReadUsesNonSecretSchema(t *testing.T) {
	for _, tc := range []struct {
		kind                string
		endpoint, publicKey string
		extra               map[string]string
	}{
		{"runpod", "https://user:proxy-pass@proxy.example.test:8443/v2?route=one", "", map[string]string{}},
		{"autodl", "https://proxy.example.test/autodl", "", map[string]string{}},
		{"compshare", "https://proxy.example.test/compshare", "public-id", map[string]string{}},
		{"aliyun", "", "public-id", map[string]string{"role_arn": "role", "regions": "cn-a,cn-b"}},
		{"zstack", "https://proxy.example.test/zstack", "public-id", map[string]string{}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			box, err := auth.NewSecretbox("edit-test")
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(provider.Credentials{Endpoint: tc.endpoint, APIKey: "api-secret", PublicKey: "public-id", PrivateKey: "private-secret", Extra: map[string]string{"ssh_password": "ssh-secret", "role_arn": "role", "regions": "cn-a,cn-b", "unknown": "unknown-secret"}})
			cipher, err := box.Encrypt(raw)
			if err != nil {
				t.Fatal(err)
			}
			p, err := st.CreateProvider(&store.Provider{Name: "saved", Kind: tc.kind, CredsCipher: cipher})
			if err != nil {
				t.Fatal(err)
			}
			h := apiHandlers{d: Deps{Store: st, Providers: provider.NewService(st, box)}}
			rec := readProviderForEdit(&h, p.ID)
			if rec.Code != 200 {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			expected, _ := json.Marshal(map[string]any{"id": p.ID, "name": "saved", "kind": tc.kind, "endpoint": tc.endpoint, "public_key": tc.publicKey, "extra": tc.extra})
			var want map[string]any
			_ = json.Unmarshal(expected, &want)
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("wire=%s want=%s", gotJSON, wantJSON)
			}
			for _, secret := range []string{"api-secret", "private-secret", "ssh-secret", "unknown-secret", "api_key", "private_key", "creds_cipher"} {
				if strings.Contains(rec.Body.String(), secret) {
					t.Fatalf("credential leaked: %s", secret)
				}
			}
		})
	}
}

func readProviderForEdit(h *apiHandlers, id uint) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/providers/%d", id), nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("id", fmt.Sprint(id))
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
	rec := httptest.NewRecorder()
	h.getProvider(rec, req)
	return rec
}

func TestProviderEditReadFailuresDoNotDiscloseCredentials(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	box, _ := auth.NewSecretbox("edit-test")
	h := apiHandlers{d: Deps{Store: st, Providers: provider.NewService(st, box)}}
	invalidJSON, _ := box.Encrypt([]byte(`{"api_key":"secret", BROKEN`))
	for _, cipher := range []string{"secret-invalid-cipher", invalidJSON} {
		p, err := st.CreateProvider(&store.Provider{Name: "broken", Kind: "runpod", CredsCipher: cipher})
		if err != nil {
			t.Fatal(err)
		}
		rec := readProviderForEdit(&h, p.ID)
		if rec.Code != 500 || strings.TrimSpace(rec.Body.String()) != `{"error":"provider configuration unavailable"}` {
			t.Fatalf("unsafe read failure: %d %s", rec.Code, rec.Body)
		}
	}
	// A damaged credential must not break account inventory or deletion access.
	rec := httptest.NewRecorder()
	h.listProviders(rec, httptest.NewRequest("GET", "/api/v1/providers", nil))
	if rec.Code != 200 {
		t.Fatalf("list=%d", rec.Code)
	}
	if rec := readProviderForEdit(&h, 99999); rec.Code != 404 {
		t.Fatalf("missing=%d", rec.Code)
	}
	h.d.Providers = nil
	if rec := readProviderForEdit(&h, 1); rec.Code != 503 {
		t.Fatalf("unavailable=%d", rec.Code)
	}
}

func TestProviderEditReadAccessBoundary(t *testing.T) {
	rig := newTokenRig(t)
	defer rig.st.Close()
	for _, tc := range []struct {
		token, method, path string
		status              int
	}{
		{"", "GET", "/api/v1/providers/1", 401},
		{rig.session, "GET", "/api/v1/providers/1", 503},
		{rig.mint("read"), "GET", "/api/v1/providers/1", 403},
		{rig.mint("write"), "GET", "/api/v1/providers/1", 403},
		{rig.session, "PATCH", "/api/v1/providers/1", 405},
	} {
		if rec := rig.do(tc.token, tc.method, tc.path, ""); rec.Code != tc.status {
			t.Errorf("%s %s=%d want %d", tc.method, tc.path, rec.Code, tc.status)
		}
	}
	if aihelper.Readable("/api/v1/providers/{id}") {
		t.Fatal("AI/MCP must not read account edit configuration")
	}
}

func TestProviderEditReadUpdateRoundTripRetainsSecretAndOverrides(t *testing.T) {
	var validations atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || (r.URL.Path != "/proxy-a/pods" && r.URL.Path != "/proxy-b/pods") {
			http.Error(w, "unsupported request", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer saved-api-key" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		validations.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer upstream.Close()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	box, _ := auth.NewSecretbox("edit-roundtrip")
	svc := provider.NewService(st, box)
	p, err := svc.Add(context.Background(), "runpod", "saved", provider.Credentials{Endpoint: upstream.URL + "/proxy-a", APIKey: "saved-api-key"})
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, Providers: svc}}
	route := chi.NewRouteContext()
	route.URLParams.Add("id", fmt.Sprint(p.ID))
	for _, tc := range []struct{ body, endpoint string }{
		{`{"name":"renamed","api_key":""}`, upstream.URL + "/proxy-a"},
		{fmt.Sprintf(`{"name":"renamed","endpoint":%q}`, upstream.URL+"/proxy-b"), upstream.URL + "/proxy-b"},
	} {
		req := httptest.NewRequest("PUT", fmt.Sprintf("/api/v1/providers/%d", p.ID), strings.NewReader(tc.body))
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
		rec := httptest.NewRecorder()
		h.updateProvider(rec, req)
		if rec.Code != 200 {
			t.Fatalf("update=%d body=%s", rec.Code, rec.Body)
		}
		read := readProviderForEdit(&h, p.ID)
		var got map[string]any
		_ = json.Unmarshal(read.Body.Bytes(), &got)
		if read.Code != 200 || got["endpoint"] != tc.endpoint || got["name"] != "renamed" || strings.Contains(read.Body.String(), "saved-api-key") {
			t.Fatalf("readback=%s", read.Body)
		}
	}
	if validations.Load() != 3 {
		t.Fatalf("authenticated validations=%d", validations.Load())
	}
}
