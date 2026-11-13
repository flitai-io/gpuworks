package digitalocean

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

const testToken = "dop_v1_testtoken"

var testCreds = provider.Credentials{APIKey: testToken}

// request is one call the fake received.
type request struct {
	Method string
	Path   string
	Query  url.Values
	Body   map[string]any
}

// fakeDO is a DigitalOcean API: it checks the bearer token, the method, the
// path and the query, pages its lists as the API does (per_page up to 200,
// links.pages.next, meta.total), validates what a create takes and answers
// errors in the API's own {"id","message"} shape.
type fakeDO struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	droplets    []map[string]any
	sizes       []map[string]any
	keys        []map[string]any
	clusters    []map[string]any
	kubeconfigs map[string]string
	requests    []request
	// forced are statuses the next requests are answered with before any
	// route is served (rate limits, outages).
	forced []forcedResponse
	nextID int64
	// metaTotal, when set, replaces meta.total in lists; emptyNext makes
	// a list answer an empty page that still names a next one; pageFailures
	// answers a page number of a list with a status.
	metaTotal    int
	emptyNext    bool
	pageFailures map[string]int
}

type forcedResponse struct {
	// only, when set, limits the response to requests of that method.
	only   string
	status int
	header map[string]string
	body   string
}

func newFake(t *testing.T) (*Driver, *fakeDO) {
	t.Helper()
	f := &fakeDO{t: t, kubeconfigs: map[string]string{}, nextID: 400000000}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	d := New()
	d.baseURL = f.srv.URL
	d.sleep = func(context.Context, time.Duration) error { return nil }
	return d, f
}

func (f *fakeDO) calls(method, path string) []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []request
	for _, r := range f.requests {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeDO) reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func (f *fakeDO) apiError(w http.ResponseWriter, status int, id, message string) {
	f.reply(w, status, map[string]any{"id": id, "message": message, "request_id": "req-" + id})
}

func (f *fakeDO) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			f.t.Errorf("%s %s: request body is not JSON: %v", r.Method, r.URL.Path, err)
		}
	}
	f.requests = append(f.requests, request{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Body: body})
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		f.apiError(w, http.StatusUnauthorized, "Unauthorized", "Unable to authenticate you")
		return
	}
	if len(f.forced) > 0 && (f.forced[0].only == "" || f.forced[0].only == r.Method) {
		forced := f.forced[0]
		f.forced = f.forced[1:]
		for k, v := range forced.header {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(forced.status)
		_, _ = io.WriteString(w, forced.body)
		return
	}
	if r.Method == http.MethodPost && r.Header.Get("Content-Type") != "application/json" {
		f.t.Errorf("POST %s: Content-Type = %q", r.URL.Path, r.Header.Get("Content-Type"))
	}
	segs := strings.Split(strings.Trim(r.URL.EscapedPath(), "/"), "/")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v2/account":
		f.reply(w, 200, map[string]any{"account": map[string]any{"droplet_limit": 25, "email": "ops@example.com", "uuid": "acct-1", "status": "active"}})
	case r.Method == http.MethodGet && r.URL.Path == "/v2/droplets":
		f.list(w, r, "droplets", f.droplets)
	case r.Method == http.MethodPost && r.URL.Path == "/v2/droplets":
		f.createDroplet(w, body)
	case len(segs) == 3 && segs[1] == "droplets" && (r.Method == http.MethodGet || r.Method == http.MethodDelete):
		f.dropletByID(w, r.Method, segs[2])
	case len(segs) == 4 && segs[1] == "droplets" && segs[3] == "actions" && r.Method == http.MethodPost:
		f.dropletAction(w, segs[2], body)
	case r.Method == http.MethodGet && r.URL.Path == "/v2/sizes":
		f.list(w, r, "sizes", f.sizes)
	case r.Method == http.MethodGet && r.URL.Path == "/v2/account/keys":
		f.list(w, r, "ssh_keys", f.keys)
	case r.Method == http.MethodPost && r.URL.Path == "/v2/account/keys":
		f.createKey(w, body)
	case r.Method == http.MethodGet && r.URL.Path == "/v2/kubernetes/clusters":
		f.list(w, r, "kubernetes_clusters", f.clusters)
	case len(segs) == 5 && segs[1] == "kubernetes" && segs[2] == "clusters" && segs[4] == "kubeconfig" && r.Method == http.MethodGet:
		id, _ := url.PathUnescape(segs[3])
		f.kubeconfig(w, r, id)
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		f.apiError(w, http.StatusNotFound, "not_found", "The resource you requested could not be found.")
	}
}

// list serves one page of a collection the way the API does.
func (f *fakeDO) list(w http.ResponseWriter, r *http.Request, key string, items []map[string]any) {
	q := r.URL.Query()
	perPage, err1 := strconv.Atoi(q.Get("per_page"))
	page, err2 := strconv.Atoi(q.Get("page"))
	if err1 != nil || err2 != nil || perPage != 200 || page < 1 {
		f.t.Errorf("%s: per_page=%q page=%q, want per_page=200 and a page number", r.URL.Path, q.Get("per_page"), q.Get("page"))
		f.apiError(w, http.StatusBadRequest, "bad_request", "invalid paging")
		return
	}
	if status, ok := f.pageFailures[q.Get("page")]; ok {
		f.apiError(w, status, "forbidden", "You are not authorized to perform this operation")
		return
	}
	start, end := (page-1)*perPage, min(page*perPage, len(items))
	if start > len(items) {
		start = len(items)
	}
	pageItems := items[start:end]
	if pageItems == nil {
		pageItems = []map[string]any{}
	}
	pages := map[string]any{}
	if end < len(items) {
		next := url.Values{"page": {strconv.Itoa(page + 1)}, "per_page": {strconv.Itoa(perPage)}}
		pages["next"] = "https://api.digitalocean.com" + r.URL.Path + "?" + next.Encode()
		pages["last"] = "https://api.digitalocean.com" + r.URL.Path + "?page=" + strconv.Itoa((len(items)+perPage-1)/perPage)
	}
	if page > 1 {
		pages["first"] = "https://api.digitalocean.com" + r.URL.Path + "?page=1&per_page=200"
		pages["prev"] = "https://api.digitalocean.com" + r.URL.Path + "?page=" + strconv.Itoa(page-1) + "&per_page=200"
	}
	links := map[string]any{}
	if len(pages) > 0 {
		links["pages"] = pages
	}
	total := len(items)
	if f.metaTotal > 0 {
		total = f.metaTotal
	}
	if f.emptyNext {
		pageItems, links = []map[string]any{}, map[string]any{"pages": map[string]any{"next": "https://api.digitalocean.com" + r.URL.Path + "?page=2&per_page=200"}}
	}
	f.reply(w, 200, map[string]any{key: pageItems, "links": links, "meta": map[string]any{"total": total}})
}

func (f *fakeDO) findDroplet(id string) (int, map[string]any) {
	for i, d := range f.droplets {
		if fmt.Sprint(int64(d["id"].(int64))) == id {
			return i, d
		}
	}
	return -1, nil
}

func (f *fakeDO) dropletByID(w http.ResponseWriter, method, id string) {
	i, d := f.findDroplet(id)
	if d == nil {
		f.apiError(w, http.StatusNotFound, "not_found", "The resource you were accessing could not be found.")
		return
	}
	if method == http.MethodDelete {
		f.droplets = append(f.droplets[:i], f.droplets[i+1:]...)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	f.reply(w, 200, map[string]any{"droplet": d})
}

func (f *fakeDO) dropletAction(w http.ResponseWriter, id string, body map[string]any) {
	_, d := f.findDroplet(id)
	if d == nil {
		f.apiError(w, http.StatusNotFound, "not_found", "The resource you were accessing could not be found.")
		return
	}
	switch body["type"] {
	case "power_on":
		d["status"] = "active"
	case "power_off":
		d["status"] = "off"
	default:
		f.apiError(w, http.StatusUnprocessableEntity, "unprocessable_entity", fmt.Sprintf("%v is not a valid action type", body["type"]))
		return
	}
	f.reply(w, http.StatusCreated, map[string]any{"action": map[string]any{"id": 9001, "status": "in-progress", "type": body["type"], "resource_type": "droplet"}})
}

// createDroplet validates like the API: name, region, size and image are
// required, an ssh key must exist in the account (by id or fingerprint), and
// the size must be one the account may create.
func (f *fakeDO) createDroplet(w http.ResponseWriter, body map[string]any) {
	for _, field := range []string{"name", "region", "size", "image"} {
		if s, _ := body[field].(string); s == "" {
			f.apiError(w, http.StatusUnprocessableEntity, "unprocessable_entity", "You specified an invalid "+field+" for Droplet creation.")
			return
		}
	}
	var size map[string]any
	for _, s := range f.sizes {
		if s["slug"] == body["size"] {
			size = s
		}
	}
	if size == nil {
		f.apiError(w, http.StatusUnprocessableEntity, "unprocessable_entity", "You specified an invalid size for Droplet creation.")
		return
	}
	if keys, ok := body["ssh_keys"].([]any); ok {
		for _, k := range keys {
			if !f.hasKey(k) {
				f.apiError(w, http.StatusUnprocessableEntity, "unprocessable_entity", "You specified an invalid SSH Key.")
				return
			}
		}
	}
	f.nextID++
	tags, _ := body["tags"].([]any)
	d := dropletJSON(f.nextID, body["name"].(string), "new", size, body["region"].(string), "", "")
	d["tags"] = tags
	f.droplets = append(f.droplets, d)
	f.reply(w, http.StatusAccepted, map[string]any{"droplet": d, "links": map[string]any{"actions": []any{map[string]any{"id": 9100, "rel": "create"}}}})
}

func (f *fakeDO) hasKey(ref any) bool {
	for _, k := range f.keys {
		switch v := ref.(type) {
		case float64:
			if float64(k["id"].(int64)) == v {
				return true
			}
		case string:
			if k["fingerprint"] == v {
				return true
			}
		}
	}
	return false
}

func (f *fakeDO) createKey(w http.ResponseWriter, body map[string]any) {
	name, _ := body["name"].(string)
	public, _ := body["public_key"].(string)
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(public))
	if name == "" || err != nil {
		f.apiError(w, http.StatusUnprocessableEntity, "unprocessable_entity", "SSH Key is invalid or incorrectly formatted")
		return
	}
	fingerprint := ssh.FingerprintLegacyMD5(pub)
	for _, k := range f.keys {
		if k["fingerprint"] == fingerprint {
			f.apiError(w, http.StatusUnprocessableEntity, "unprocessable_entity", "SSH Key is already in use on your account")
			return
		}
	}
	f.nextID++
	key := map[string]any{"id": f.nextID, "name": name, "fingerprint": fingerprint, "public_key": strings.TrimSpace(public)}
	f.keys = append(f.keys, key)
	f.reply(w, http.StatusCreated, map[string]any{"ssh_key": key})
}

func (f *fakeDO) kubeconfig(w http.ResponseWriter, r *http.Request, id string) {
	cfg, ok := f.kubeconfigs[id]
	if !ok {
		f.apiError(w, http.StatusNotFound, "not_found", "The resource you requested could not be found.")
		return
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("expiry_seconds")); err != nil || n < 0 {
		f.apiError(w, http.StatusUnprocessableEntity, "unprocessable_entity", "expiry_seconds must be a number")
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(200)
	_, _ = io.WriteString(w, cfg)
}

// sizeJSON is shaped like an item of /v2/sizes (2026-10-22, trimmed). A
// non-empty model makes it a GPU size.
func sizeJSON(slug string, price float64, model string, count int, vramGiB int) map[string]any {
	s := map[string]any{
		"slug": slug, "memory": 32768, "vcpus": 8, "disk": 500, "transfer": 10.0,
		"price_monthly": price * 720, "price_hourly": price, "regions": []string{"nyc2", "tor1"}, "available": true,
		"description": slug, "networking_throughput": 10000,
		"disk_info": []any{map[string]any{"type": "local", "size": map[string]any{"amount": 500, "unit": "gib"}}},
	}
	if model != "" {
		s["gpu_info"] = map[string]any{"count": count, "vram": map[string]any{"amount": vramGiB, "unit": "gib"}, "model": model}
	}
	return s
}

// dropletJSON is shaped like a droplet object. With a publicIP the droplet
// has its addresses (a droplet in status "new" has none yet).
func dropletJSON(id int64, name, status string, size map[string]any, region, publicIP, privateIP string, tags ...string) map[string]any {
	v4 := []any{}
	if privateIP != "" {
		v4 = append(v4, map[string]any{"ip_address": privateIP, "netmask": "255.255.240.0", "gateway": "", "type": "private"})
	}
	if publicIP != "" {
		v4 = append(v4, map[string]any{"ip_address": publicIP, "netmask": "255.255.240.0", "gateway": "203.0.113.1", "type": "public"})
	}
	if tags == nil {
		tags = []string{}
	}
	return map[string]any{
		"id": id, "name": name, "memory": size["memory"], "vcpus": size["vcpus"], "disk": size["disk"], "locked": false, "status": status,
		"created_at": "2026-10-22T08:00:00Z", "features": []string{"droplet_agent"}, "backup_ids": []any{}, "snapshot_ids": []any{}, "volume_ids": []any{},
		"image": map[string]any{"id": 191000000, "name": "NVIDIA AI/ML Ready Image", "distribution": "Ubuntu", "slug": "gpu-h100x1-base", "public": true, "type": "base"},
		"size":  size, "size_slug": size["slug"],
		"networks": map[string]any{"v4": v4, "v6": []any{}},
		"region":   map[string]any{"name": region, "slug": region, "features": []string{"metadata"}, "available": true, "sizes": []string{}},
		"tags":     tags, "vpc_uuid": "vpc-1",
	}
}

// nodePoolJSON and clusterJSON are shaped like DOKS objects.
func nodePoolJSON(id, name, size string, count int) map[string]any {
	nodes := []any{}
	for i := 0; i < count; i++ {
		nodes = append(nodes, map[string]any{"id": fmt.Sprintf("%s-n%d", id, i), "name": fmt.Sprintf("%s-%d", name, i), "status": map[string]any{"state": "running"}, "droplet_id": "400100"})
	}
	return map[string]any{"id": id, "name": name, "size": size, "count": count, "tags": []string{"k8s", "k8s:worker"}, "labels": map[string]any{"doks.digitalocean.com/node-pool-id": id}, "taints": []any{}, "auto_scale": false, "nodes": nodes}
}

func clusterJSON(id, name, region, state, endpoint string, pools ...map[string]any) map[string]any {
	if pools == nil {
		pools = []map[string]any{}
	}
	return map[string]any{
		"id": id, "name": name, "region": region, "version": "1.34.1-do.0", "cluster_subnet": "10.244.0.0/16", "service_subnet": "10.245.0.0/16",
		"vpc_uuid": "vpc-1", "ipv4": "", "endpoint": endpoint, "tags": []string{"k8s", "k8s:" + id}, "node_pools": pools,
		"status": map[string]any{"state": state, "message": ""}, "created_at": "2026-10-22T08:00:00Z", "auto_upgrade": false,
	}
}

// newTestKey makes an SSH key pair's public half as the authorized_keys line
// and the fingerprint DigitalOcean shows for it.
func newTestKey(t *testing.T, comment string) (line, fingerprint string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	line = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	if comment != "" {
		line += " " + comment
	}
	return line, ssh.FingerprintLegacyMD5(sshPub)
}

// addKey puts an SSH key in the fake account and returns its id.
func (f *fakeDO) addKey(t *testing.T, name, line string) int64 {
	t.Helper()
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.keys = append(f.keys, map[string]any{"id": f.nextID, "name": name, "fingerprint": ssh.FingerprintLegacyMD5(pub), "public_key": line})
	return f.nextID
}

func (f *fakeDO) failPage(page string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pageFailures == nil {
		f.pageFailures = map[string]int{}
	}
	f.pageFailures[page] = status
}

// pageOf is a one-page list body, for forced responses.
func pageOf(key string, items []map[string]any) string {
	raw, _ := json.Marshal(map[string]any{key: items, "links": map[string]any{}, "meta": map[string]any{"total": len(items)}})
	return string(raw)
}
