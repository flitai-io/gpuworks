package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func TestProviderCredentialDefaultsWireContract(t *testing.T) {
	h := apiHandlers{d: Deps{Providers: provider.NewService(nil, nil)}}
	rec := httptest.NewRecorder()
	h.providerKinds(rec, httptest.NewRequest(http.MethodGet, "/api/v1/providers/kinds", nil))
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var response struct {
		Items []struct {
			Kind   string                       `json:"kind"`
			Fields []map[string]json.RawMessage `json:"credential_fields"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{"autodl": "https://api.autodl.com", "compshare": "https://api.compshare.cn", "digitalocean": "https://api.digitalocean.com", "runpod": "https://api.runpod.io/v2", "vast": "https://console.vast.ai/api/v0"}
	seen := map[string]bool{}
	seenTemplate := false
	for _, kind := range response.Items {
		for _, field := range kind.Fields {
			var name string
			var secret bool
			if err := json.Unmarshal(field["name"], &name); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(field["secret"], &secret); err != nil {
				t.Fatal(err)
			}
			if kind.Kind == "zstack" && name == "endpoint" {
				template := field["default_template"]
				if string(template) != `"http://MANAGEMENT_NODE_IP:8080/zstack"` {
					t.Errorf("ZStack endpoint template = %s", template)
				}
				if secret {
					t.Error("ZStack endpoint template must not be secret")
				}
				seenTemplate = true
			}
			value, hasDefault := field["default_value"]
			want, hasEndpoint := expected[kind.Kind]
			if name == "endpoint" && hasEndpoint {
				if string(value) != `"`+want+`"` {
					t.Errorf("%s endpoint default = %s, want %q", kind.Kind, value, want)
				}
				if secret {
					t.Errorf("%s endpoint incorrectly secret", kind.Kind)
				}
				seen[kind.Kind] = true
			} else if hasDefault {
				t.Errorf("%s.%s must not invent a default: %s", kind.Kind, name, value)
			}
		}
	}
	if !seenTemplate {
		t.Error("missing ZStack endpoint template")
	}
	for kind := range expected {
		if !seen[kind] {
			t.Errorf("missing endpoint default for %s", kind)
		}
	}
}
