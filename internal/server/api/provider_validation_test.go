package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestZStackProviderValidationErrorsPreserveStorageAndWireContract(t *testing.T) {
	var reject atomic.Bool
	reject.Store(true)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/hosts" || !strings.HasPrefix(r.Header.Get("Authorization"), "ZStack private-ak:") {
			t.Error("validation lost its endpoint or signing")
		}
		if reject.Load() {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":{"code":"SYS.1006","details":"access key id: private-ak does not existed or disabled"}}`)
			return
		}
		fmt.Fprint(w, `{"inventories":[]}`)
	}))
	defer remote.Close()
	st, err := store.Open(t.TempDir()+"/providers.db", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	box, err := auth.NewSecretbox("provider-validation-test")
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, Providers: provider.NewService(st, box)}}
	request := func(method, path, name string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"kind": "zstack", "name": name, "endpoint": remote.URL, "public_key": "private-ak", "private_key": "private-sk"})
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		w := httptest.NewRecorder()
		if method == http.MethodPost {
			h.addProvider(w, r)
		} else {
			router := chi.NewRouter()
			router.Put("/api/v1/providers/{id}", h.updateProvider)
			router.ServeHTTP(w, r)
		}
		return w
	}
	assertFailure := func(w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body) != 2 || body["code"] != "ZSTACK_ACCESS_KEY_INVALID" || body["error"] != "credential validation failed: The ZStack AK does not exist or is disabled. Check the AccessKey ID and its state." {
			t.Fatalf("unexpected wire error: %#v", body)
		}
		for _, secret := range []string{"private-ak", "private-sk", "does not existed"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("remote credential detail leaked")
			}
		}
	}
	assertFailure(request(http.MethodPost, "/api/v1/providers", "cloud"))
	providers, err := st.ListProviders()
	if err != nil || len(providers) != 0 {
		t.Fatalf("failed addition persisted: %v %v", providers, err)
	}
	reject.Store(false)
	w := request(http.MethodPost, "/api/v1/providers", "cloud")
	if w.Code != http.StatusCreated {
		t.Fatalf("valid cloud rejected: %d %s", w.Code, w.Body)
	}
	providers, err = st.ListProviders()
	if err != nil || len(providers) != 1 {
		t.Fatalf("successful addition not persisted: %v", err)
	}
	before := providers[0]
	reject.Store(true)
	assertFailure(request(http.MethodPut, fmt.Sprintf("/api/v1/providers/%d", before.ID), "changed"))
	after, err := st.GetProvider(before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != before.Name || after.CredsCipher != before.CredsCipher {
		t.Fatal("failed edit changed saved provider credentials")
	}
}
