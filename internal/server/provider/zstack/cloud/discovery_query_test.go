package cloud

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
)

func TestQueryAllPreservesFiltersAcrossPages(t *testing.T) {
	for _, failSecondPage := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail_second_page=%v", failSecondPage), func(t *testing.T) {
			var requests atomic.Int32
			filters := []string{"hypervisorType=KVM", "name=gpu host"}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/hosts" || !validTestAccessKey(r) {
					t.Errorf("unexpected Cloud request: %s %s", r.Method, r.URL)
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				start := int(requests.Add(1)-1) * 100
				query := r.URL.Query()
				if !reflect.DeepEqual(query["q"], filters) || query.Get("start") != strconv.Itoa(start) ||
					query.Get("limit") != "100" || query.Get("sort") != "uuid" || start > 100 {
					t.Errorf("page query = %v, want both filters and start=%d, limit=100, sort=uuid", query, start)
					http.Error(w, "invalid query", http.StatusBadRequest)
					return
				}
				if start == 100 && failSecondPage {
					http.Error(w, "second page rejected", http.StatusBadRequest)
					return
				}
				inventories := make([]map[string]string, 0)
				for i := start; i < min(start+100, 101); i++ {
					inventories = append(inventories, map[string]string{
						"uuid": fmt.Sprintf("host-%03d", i), "hypervisorType": "KVM", "name": "gpu host",
					})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"inventories": inventories})
			}))
			defer server.Close()
			client, err := apiclient.New(provider.Credentials{
				Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret",
			})
			if err != nil {
				t.Fatal(err)
			}
			path := "/hosts?" + url.Values{"q": filters}.Encode()
			hosts, err := apiclient.QueryAll[hostInventory](t.Context(), client, path)
			if requests.Load() != 2 {
				t.Fatalf("page requests = %d, want 2", requests.Load())
			}
			if failSecondPage {
				if err == nil || hosts != nil {
					t.Fatalf("failed listing returned partial inventory: hosts=%v err=%v", hosts, err)
				}
				return
			}
			if err != nil || len(hosts) != 101 {
				t.Fatalf("listing: hosts=%d err=%v, want all 101 hosts", len(hosts), err)
			}
			for i, host := range hosts {
				if host.UUID != fmt.Sprintf("host-%03d", i) {
					t.Fatalf("host %d = %q, listing skipped or repeated a host", i, host.UUID)
				}
			}
		})
	}
}
