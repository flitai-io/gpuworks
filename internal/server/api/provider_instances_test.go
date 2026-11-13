package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/localization"
)

// apiInventoryDriver models a vendor account whose instances exist until a
// successful release; releasing and listing can be made to fail.
type apiInventoryDriver struct {
	mu        sync.Mutex
	instances map[string]provider.Instance
	listErr   error
	deleteErr error
}

func (d *apiInventoryDriver) Name() string { return "api-inventory-provider" }

func (d *apiInventoryDriver) Capabilities() provider.Capabilities {
	return provider.Capabilities{CanListInstances: true, CanDelete: true}
}

func (d *apiInventoryDriver) ValidateCredentials(context.Context, provider.Credentials) error {
	return nil
}

func (d *apiInventoryDriver) ListInstances(context.Context, provider.Credentials) ([]provider.Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.listErr != nil {
		return nil, d.listErr
	}
	out := make([]provider.Instance, 0, len(d.instances))
	for _, inst := range d.instances {
		out = append(out, inst)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExternalID < out[j].ExternalID })
	return out, nil
}

func (d *apiInventoryDriver) PowerState(context.Context, provider.Credentials, string) (provider.PowerState, error) {
	return provider.PowerUnknown, provider.ErrUnsupported
}

func (d *apiInventoryDriver) PowerOn(context.Context, provider.Credentials, string) error {
	return provider.ErrUnsupported
}

func (d *apiInventoryDriver) PowerOff(context.Context, provider.Credentials, string) error {
	return provider.ErrUnsupported
}

func (d *apiInventoryDriver) CreateInstance(context.Context, provider.Credentials, provider.CreateInstanceRequest) (provider.Instance, error) {
	return provider.Instance{}, provider.ErrUnsupported
}

func (d *apiInventoryDriver) DeleteInstance(_ context.Context, _ provider.Credentials, externalID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.deleteErr != nil {
		return d.deleteErr
	}
	delete(d.instances, externalID)
	return nil
}

func (d *apiInventoryDriver) ResizeInstance(context.Context, provider.Credentials, string, provider.ResizeInstanceRequest) error {
	return provider.ErrUnsupported
}

func (d *apiInventoryDriver) InstallAgent(context.Context, provider.Credentials, provider.InstallRequest) error {
	return provider.ErrUnsupported
}

func (d *apiInventoryDriver) set(fn func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn()
}

func inventoryInstance(id, name, ip string) provider.Instance {
	return provider.Instance{
		ExternalID: id, Name: name, GPUModel: "4090", GPUCount: 2, Power: provider.PowerRunning,
		PricePerHr: 2.5, Region: "cn-wlcb", PublicIP: ip, SSHPort: 22, SSHUser: "root", Kind: "vm",
		SSHConnections: []provider.SSHConnection{{
			ID: "direct", Type: "direct", Label: localization.Text{localization.English: "Direct"},
			Host: ip, Port: 22, User: "root", InstallSupported: true,
		}},
	}
}

func TestProviderInstanceReleaseAndSyncDropReleasedRows(t *testing.T) {
	drv := &apiInventoryDriver{instances: map[string]provider.Instance{
		"uhost-a": inventoryInstance("uhost-a", "gpu-a", "203.0.113.9"),
		"uhost-b": inventoryInstance("uhost-b", "gpu-b", "203.0.113.10"),
	}}
	provider.Register(drv)
	st, err := store.Open("file:api_provider_instance_release?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("api-provider-instance-release")
	if err != nil {
		t.Fatal(err)
	}
	svc := provider.NewService(st, box)
	p, err := svc.Add(context.Background(), drv.Name(), "cloud", provider.Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner("provider-instance-release-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	router := Router(Deps{Store: st, Providers: svc, AuthSigner: signer})
	base := fmt.Sprintf("/api/v1/providers/%d", p.ID)
	call := func(method, path, bearer string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	expect := func(rec *httptest.ResponseRecorder, code int, body string) {
		t.Helper()
		if rec.Code != code || strings.TrimSpace(rec.Body.String()) != body {
			t.Fatalf("status=%d body=%s\nwant status=%d body=%s", rec.Code, strings.TrimSpace(rec.Body.String()), code, body)
		}
	}
	const rowA = `{"external_id":"uhost-a","name":"gpu-a","gpu_model":"4090","gpu_count":2,"power":"running","price_per_hr":2.5,"region":"cn-wlcb","public_ip":"203.0.113.9","ssh_port":22,"ssh_user":"root","ssh_connections":[{"id":"direct","type":"direct","label":{"en":"Direct"},"host":"203.0.113.9","port":22,"user":"root","install_supported":true}],"kind":"vm","node_id":0,"actions":["install","delete"]}`
	const rowB = `{"external_id":"uhost-b","name":"gpu-b","gpu_model":"4090","gpu_count":2,"power":"running","price_per_hr":2.5,"region":"cn-wlcb","public_ip":"203.0.113.10","ssh_port":22,"ssh_user":"root","ssh_connections":[{"id":"direct","type":"direct","label":{"en":"Direct"},"host":"203.0.113.10","port":22,"user":"root","install_supported":true}],"kind":"vm","node_id":0,"actions":["install","delete"]}`

	if rec := call(http.MethodPost, base+"/sync", token); rec.Code != http.StatusOK {
		t.Fatalf("sync status=%d body=%s", rec.Code, rec.Body.String())
	}
	expect(call(http.MethodGet, base+"/instances", token), http.StatusOK, `{"items":[`+rowA+","+rowB+`],"total":2}`)

	// Unauthenticated release is rejected before reaching the vendor.
	if rec := call(http.MethodDelete, base+"/instances/uhost-a", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated delete status=%d body=%s", rec.Code, rec.Body.String())
	}

	// A vendor failure surfaces as 502 and keeps the row.
	drv.set(func() { drv.deleteErr = errors.New("vendor: instance is locked") })
	expect(call(http.MethodDelete, base+"/instances/uhost-a", token), http.StatusFailedDependency, `{"error":"vendor: instance is locked"}`)
	expect(call(http.MethodGet, base+"/instances", token), http.StatusOK, `{"items":[`+rowA+","+rowB+`],"total":2}`)

	// A confirmed release drops the row immediately, without a sync.
	drv.set(func() { drv.deleteErr = nil })
	expect(call(http.MethodDelete, base+"/instances/uhost-a", token), http.StatusOK, `{"status":"deleted"}`)
	expect(call(http.MethodGet, base+"/instances", token), http.StatusOK, `{"items":[`+rowB+`],"total":1}`)
	if rec := call(http.MethodGet, "/api/v1/providers", token); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"instance_count":1,`) {
		t.Fatalf("provider list status=%d body=%s, want instance_count 1", rec.Code, rec.Body.String())
	}

	// Released outside gpuworks: a failed listing changes nothing, the next
	// successful (now empty) listing removes the row.
	drv.set(func() {
		delete(drv.instances, "uhost-b")
		drv.listErr = errors.New("vendor: HTTP 503")
	})
	expect(call(http.MethodPost, base+"/sync", token), http.StatusFailedDependency, `{"error":"vendor: HTTP 503"}`)
	expect(call(http.MethodGet, base+"/instances", token), http.StatusOK, `{"items":[`+rowB+`],"total":1}`)
	drv.set(func() { drv.listErr = nil })
	expect(call(http.MethodPost, base+"/sync", token), http.StatusOK, `{"count":0,"instances":[]}`)
	expect(call(http.MethodGet, base+"/instances", token), http.StatusOK, `{"items":[],"total":0}`)
}

// clusterNodeDriver is a vendor account that controls power and lists one
// node of a managed Kubernetes cluster beside a plain instance, counting what
// reaches the vendor.
type clusterNodeDriver struct {
	apiInventoryDriver
	calls []string
}

func (d *clusterNodeDriver) Name() string { return "api-cluster-node-provider" }

func (d *clusterNodeDriver) Capabilities() provider.Capabilities {
	return provider.Capabilities{CanListInstances: true, CanPowerControl: true, CanDelete: true}
}

func (d *clusterNodeDriver) PowerOn(_ context.Context, _ provider.Credentials, id string) error {
	d.calls = append(d.calls, "on:"+id)
	return nil
}

func (d *clusterNodeDriver) PowerOff(_ context.Context, _ provider.Credentials, id string) error {
	d.calls = append(d.calls, "off:"+id)
	return nil
}

func (d *clusterNodeDriver) DeleteInstance(_ context.Context, _ provider.Credentials, id string) error {
	d.calls = append(d.calls, "delete:"+id)
	return nil
}

func (d *clusterNodeDriver) ResizeInstance(_ context.Context, _ provider.Credentials, id string, _ provider.ResizeInstanceRequest) error {
	d.calls = append(d.calls, "resize:"+id)
	return nil
}

// A node of a managed Kubernetes cluster is the cluster's: the server says it
// allows no action, and powering it, releasing it or resizing it is refused
// with PROVIDER_INSTANCE_IN_CLUSTER before the vendor hears of it, while a
// plain instance of the same account keeps its actions.
func TestProviderClusterNodeAllowsNoActionAndRefusesThem(t *testing.T) {
	node := inventoryInstance("i-node", "ack-node", "203.0.113.20")
	node.Cluster = "c-1"
	drv := &clusterNodeDriver{apiInventoryDriver: apiInventoryDriver{instances: map[string]provider.Instance{
		"i-node": node, "i-plain": inventoryInstance("i-plain", "plain", "203.0.113.21"),
	}}}
	provider.Register(drv)
	st, err := store.Open("file:api_provider_cluster_node?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("api-provider-cluster-node")
	if err != nil {
		t.Fatal(err)
	}
	svc := provider.NewService(st, box)
	p, err := svc.Add(context.Background(), drv.Name(), "cloud", provider.Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner("provider-cluster-node-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	router := Router(Deps{Store: st, Providers: svc, AuthSigner: signer})
	base := fmt.Sprintf("/api/v1/providers/%d", p.ID)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	if rec := call(http.MethodPost, base+"/sync", ""); rec.Code != http.StatusOK {
		t.Fatalf("sync = %d %s", rec.Code, rec.Body)
	}
	rec := call(http.MethodGet, base+"/instances", "")
	var list struct {
		Items []struct {
			ExternalID string   `json:"external_id"`
			Actions    []string `json:"actions"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Items) != 2 {
		t.Fatalf("instances = %s err=%v", rec.Body, err)
	}
	for _, it := range list.Items {
		want := `["power_off","install","delete"]`
		if it.ExternalID == "i-node" {
			want = `[]`
		}
		if got, _ := json.Marshal(it.Actions); string(got) != want {
			t.Fatalf("%s actions = %s, want %s (body %s)", it.ExternalID, got, want, rec.Body)
		}
	}
	if !strings.Contains(rec.Body.String(), `"managed_cluster":"c-1"`) {
		t.Fatalf("the cluster node lost managed_cluster: %s", rec.Body)
	}

	for _, req := range []struct{ method, path, body string }{
		{http.MethodPost, base + "/instances/i-node/power", `{"action":"off"}`},
		{http.MethodPost, base + "/instances/i-node/power", `{"action":"on"}`},
		{http.MethodDelete, base + "/instances/i-node", ""},
		{http.MethodPost, base + "/instances/i-node/resize", `{"gpu_count":1}`},
	} {
		rec := call(req.method, req.path, req.body)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"PROVIDER_INSTANCE_IN_CLUSTER"`) || !strings.Contains(rec.Body.String(), `"cluster":"c-1"`) {
			t.Fatalf("%s %s = %d %s", req.method, req.path, rec.Code, rec.Body)
		}
	}
	if len(drv.calls) != 0 {
		t.Fatalf("a refused operation reached the vendor: %v", drv.calls)
	}
	if rec := call(http.MethodPost, base+"/instances/i-plain/power", `{"action":"off"}`); rec.Code != http.StatusOK {
		t.Fatalf("power off a plain instance = %d %s", rec.Code, rec.Body)
	}
	if rec := call(http.MethodDelete, base+"/instances/i-plain", ""); rec.Code != http.StatusOK {
		t.Fatalf("release a plain instance = %d %s", rec.Code, rec.Body)
	}
	if strings.Join(drv.calls, ",") != "off:i-plain,delete:i-plain" {
		t.Fatalf("vendor calls = %v", drv.calls)
	}
}
