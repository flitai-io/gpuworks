package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// proxyRow is a wire row with the proxy and image fields.
type proxyRow struct {
	ID         uint   `json:"id"`
	Model      string `json:"model"`
	ProxyMode  string `json:"proxy_mode"`
	ProxyURL   string `json:"proxy_url"`
	ImageInput bool   `json:"image_input"`
}

func (r *aiConfigRig) proxyRow(t *testing.T, id uint) (proxyRow, map[string]any) {
	t.Helper()
	rec := r.do(t, http.MethodGet, "/api/v1/ai/configs", "")
	var rows []json.RawMessage
	if err := unmarshalList(rec.Body.Bytes(), &rows); rec.Code != http.StatusOK || err != nil {
		t.Fatalf("list = %d %s (err %v)", rec.Code, rec.Body.String(), err)
	}
	for _, raw := range rows {
		var row proxyRow
		var wire map[string]any
		if json.Unmarshal(raw, &row) != nil || json.Unmarshal(raw, &wire) != nil {
			t.Fatalf("row %s", raw)
		}
		if row.ID == id {
			return row, wire
		}
	}
	t.Fatalf("config %d missing", id)
	return proxyRow{}, nil
}

func (r *aiConfigRig) stored(t *testing.T, id uint) *store.AIConfig {
	t.Helper()
	c, err := r.store.AIConfigByID(id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// wantFieldInvalid asserts a 400 naming one field, whose message never repeats
// the proxy's password.
func wantFieldInvalid(t *testing.T, rec *httptest.ResponseRecorder, field string) {
	t.Helper()
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"`+field+`"`) {
		t.Fatalf("want 400 on %s, got %d %s", field, rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "s3cret") {
		t.Fatalf("error leaks the proxy password: %s", rec.Body.String())
	}
}

// The dictionary is the server's preset table, in order, ending with custom.
func TestAIPresetDictionaryWire(t *testing.T) {
	rig := newAIConfigRig(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/ai/presets", nil)
	rec := httptest.NewRecorder()
	rig.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d", rec.Code)
	}
	rec = rig.do(t, http.MethodGet, "/api/v1/ai/presets", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("presets = %d %s", rec.Code, rec.Body.String())
	}
	want := `{"presets":[` +
		`{"name":"deepseek","base_url":"https://api.deepseek.com","model":"deepseek-flash","image_input":true},` +
		`{"name":"bailian","base_url":"https://dashscope.aliyuncs.com/compatible-mode/v1","model":"qwen-plus","image_input":false},` +
		`{"name":"siliconflow","base_url":"https://api.siliconflow.cn/v1","model":"deepseek-ai/DeepSeek-V3","image_input":false},` +
		`{"name":"openrouter","base_url":"https://openrouter.ai/api/v1","model":"deepseek/deepseek-chat","image_input":false},` +
		`{"name":"poe","base_url":"https://api.poe.com/v1","model":"GPT-5","image_input":true},` +
		`{"name":"custom","base_url":"","model":"","image_input":false}],` +
		`"proxy_modes":["management","direct","custom"]}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("presets wire\n got %s\nwant %s", got, want)
	}
	if rec := rig.do(t, http.MethodPost, "/api/v1/ai/presets", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST presets = %d", rec.Code)
	}
}

func TestCreateAIConfigProxyAndImageInput(t *testing.T) {
	rig := newAIConfigRig(t)
	base := `"name":"svc","preset":"custom","base_url":"https://llm.example.com/v1","api_key":"sk","model":"m"`
	wantFieldInvalid(t, rig.do(t, http.MethodPost, "/api/v1/ai/configs", `{`+base+`,"proxy_mode":"platform"}`), "proxy_mode")
	wantFieldInvalid(t, rig.do(t, http.MethodPost, "/api/v1/ai/configs", `{`+base+`,"proxy_mode":"custom"}`), "proxy_url")
	wantFieldInvalid(t, rig.do(t, http.MethodPost, "/api/v1/ai/configs", `{`+base+`,"proxy_mode":"direct","proxy_url":"http://p:3128"}`), "proxy_url")
	wantFieldInvalid(t, rig.do(t, http.MethodPost, "/api/v1/ai/configs", `{`+base+`,"proxy_mode":"custom","proxy_url":"ftp://u:s3cret@p:21"}`), "proxy_url")
	wantFieldInvalid(t, rig.do(t, http.MethodPost, "/api/v1/ai/configs", `{`+base+`,"proxy_mode":"custom","proxy_url":"http://u:s3cret@p:3128/path"}`), "proxy_url")

	// Nothing named: management, no image input for a custom address.
	plain := rig.create(t, `{`+base+`}`)
	if row, wire := rig.proxyRow(t, plain); row.ProxyMode != "management" || row.ImageInput || wire["proxy_url"] != nil {
		t.Fatalf("defaults = %+v wire=%v", row, wire)
	}

	// A custom proxy is sealed at rest and listed with its login masked.
	custom := rig.create(t, `{`+base+`,"proxy_mode":"custom","proxy_url":"socks5h://alice:s3cret@proxy.example:1080","image_input":true}`)
	stored := rig.stored(t, custom)
	if !strings.HasPrefix(stored.ProxyURL, "enc:v1:") || strings.Contains(stored.ProxyURL, "s3cret") {
		t.Fatalf("proxy stored as %q", stored.ProxyURL)
	}
	if plainURL, err := store.DecryptAIKey(rig.box, stored.ProxyURL); err != nil || plainURL != "socks5h://alice:s3cret@proxy.example:1080" {
		t.Fatalf("unsealed = %q err=%v", plainURL, err)
	}
	row, _ := rig.proxyRow(t, custom)
	if row.ProxyMode != "custom" || row.ProxyURL != "socks5h://xxxxx:xxxxx@proxy.example:1080" || !row.ImageInput {
		t.Fatalf("custom row = %+v", row)
	}

	// A preset that fills the model fills its image switch; an explicit
	// switch, or an explicit model, does not take the preset's.
	filled := rig.create(t, `{"name":"ds","preset":"deepseek","api_key":"sk"}`)
	if row, _ := rig.proxyRow(t, filled); row.Model != "deepseek-flash" || !row.ImageInput {
		t.Fatalf("preset fill = %+v", row)
	}
	off := rig.create(t, `{"name":"ds-off","preset":"deepseek","api_key":"sk","image_input":false}`)
	if row, _ := rig.proxyRow(t, off); row.ImageInput {
		t.Fatalf("explicit false lost: %+v", row)
	}
	pro := rig.create(t, `{"name":"ds-pro","preset":"deepseek","api_key":"sk","model":"deepseek-v4-pro"}`)
	if row, _ := rig.proxyRow(t, pro); row.ImageInput {
		t.Fatalf("an explicit model took the preset's switch: %+v", row)
	}
}

func TestUpdateAIConfigProxyAndImageInput(t *testing.T) {
	rig := newAIConfigRig(t)
	id := rig.create(t, `{"name":"svc","preset":"custom","base_url":"https://llm.example.com/v1","api_key":"sk","model":"m"}`)
	patch := func(body string) *httptest.ResponseRecorder {
		return rig.do(t, http.MethodPatch, fmt.Sprintf("/api/v1/ai/configs/%d", id), body)
	}
	// custom needs a proxy when none is stored to keep; a URL alone is refused
	// while the stored mode is not custom.
	wantFieldInvalid(t, patch(`{"proxy_mode":"custom"}`), "proxy_url")
	wantFieldInvalid(t, patch(`{"proxy_url":"http://p:3128"}`), "proxy_url")

	if rec := patch(`{"proxy_mode":"custom","proxy_url":"http://bob:s3cret@proxy.example:3128","image_input":true}`); rec.Code != http.StatusOK {
		t.Fatalf("set custom = %d %s", rec.Code, rec.Body.String())
	}
	sealed := rig.stored(t, id).ProxyURL
	if row, _ := rig.proxyRow(t, id); row.ProxyMode != "custom" || row.ProxyURL != "http://xxxxx:xxxxx@proxy.example:3128" || !row.ImageInput {
		t.Fatalf("after custom = %+v", row)
	}
	// What the edit dialog sends back: the mode and an empty URL keep the
	// stored proxy, and a name-only patch touches neither.
	for _, body := range []string{`{"proxy_mode":"custom","proxy_url":""}`, `{"name":"renamed"}`} {
		if rec := patch(body); rec.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", body, rec.Code, rec.Body.String())
		}
		if got := rig.stored(t, id); got.ProxyURL != sealed || got.ProxyMode != "custom" || !got.ImageInput {
			t.Fatalf("%s changed the proxy: %+v", body, got)
		}
	}
	// A URL alone replaces it while the mode is custom.
	if rec := patch(`{"proxy_url":"http://proxy2.example:3128"}`); rec.Code != http.StatusOK {
		t.Fatalf("replace = %d %s", rec.Code, rec.Body.String())
	}
	if row, _ := rig.proxyRow(t, id); row.ProxyURL != "http://proxy2.example:3128" {
		t.Fatalf("replaced = %+v", row)
	}
	// Leaving custom drops the stored proxy.
	if rec := patch(`{"proxy_mode":"direct"}`); rec.Code != http.StatusOK {
		t.Fatalf("direct = %d %s", rec.Code, rec.Body.String())
	}
	if got := rig.stored(t, id); got.ProxyMode != "direct" || got.ProxyURL != "" {
		t.Fatalf("direct kept a proxy: %+v", got)
	}
	// Back to custom with nothing stored needs a URL again.
	wantFieldInvalid(t, patch(`{"proxy_mode":"custom","proxy_url":""}`), "proxy_url")

	// A preset-only patch brings the preset's image switch with its model.
	if rec := patch(`{"preset":"bailian"}`); rec.Code != http.StatusOK {
		t.Fatalf("preset = %d", rec.Code)
	}
	if row, _ := rig.proxyRow(t, id); row.Model != aihelper.Presets["bailian"].Model || row.ImageInput {
		t.Fatalf("bailian = %+v", row)
	}
	if rec := patch(`{"preset":"deepseek"}`); rec.Code != http.StatusOK {
		t.Fatalf("preset = %d", rec.Code)
	}
	if row, _ := rig.proxyRow(t, id); row.Model != "deepseek-flash" || !row.ImageInput {
		t.Fatalf("deepseek = %+v", row)
	}
	if rec := rig.do(t, http.MethodPatch, "/api/v1/ai/configs/9999", `{"proxy_mode":"direct"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("missing service = %d", rec.Code)
	}
}

// completionFixture answers text like an OpenAI-compatible endpoint, and an
// image the way a text-only vLLM model does unless vision is set.
func completionFixture(t *testing.T, vision bool, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(raw), `"image_url"`) && !vision {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"object":"error","message":"m is not a multimodal model","type":"BadRequestError","param":null,"code":400}`)
			return
		}
		content := "ok"
		if strings.Contains(string(raw), `"image_url"`) {
			content = "Red"
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`, content)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// authProxy is a CONNECT proxy to origin that counts tunnels and records the
// login each one presented.
func authProxy(t *testing.T, origin string, tunnels *atomic.Int64, login *atomic.Value) *httptest.Server {
	t.Helper()
	inner := newTunnelingProxy(t, origin, func() { tunnels.Add(1) })
	inner.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login.Store(r.Header.Get("Proxy-Authorization"))
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testAI(t *testing.T, rig *aiConfigRig, body string) map[string]any {
	t.Helper()
	rec := rig.do(t, http.MethodPost, "/api/v1/ai/configs/test", body)
	var out map[string]any
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("test = %d %s", rec.Code, rec.Body.String())
	}
	return out
}

// The connection test goes through the proxy the form names - the management
// node's, or the service's own with its login - and checks images only when
// asked and only after text works.
func TestAIConfigTestFollowsTheProxyAndChecksImages(t *testing.T) {
	rig := newAIConfigRig(t)
	var hits, mgmtTunnels, ownTunnels atomic.Int64
	var mgmtLogin, ownLogin atomic.Value
	origin := completionFixture(t, false, &hits)
	mgmt := authProxy(t, origin.URL, &mgmtTunnels, &mgmtLogin)
	own := authProxy(t, origin.URL, &ownTunnels, &ownLogin)
	useManagementProxy(t, rig.store, mgmt.URL)
	target := viaProxy(origin.URL) + "/v1"

	out := testAI(t, rig, `{"base_url":"`+target+`","model":"m"}`)
	if out["outcome"] != "ok" || mgmtTunnels.Load() != 1 || ownTunnels.Load() != 0 {
		t.Fatalf("management = %v tunnels mgmt=%d own=%d", out, mgmtTunnels.Load(), ownTunnels.Load())
	}
	if _, ok := out["image"]; ok {
		t.Fatalf("image checked without being asked: %v", out)
	}

	ownURL := strings.Replace(own.URL, "http://", "http://carol:s3cret@", 1)
	out = testAI(t, rig, `{"base_url":"`+target+`","model":"m","proxy_mode":"custom","proxy_url":"`+ownURL+`","check_image":true}`)
	if out["outcome"] != "ok" || out["image"] != "rejected" || out["image_status"] != float64(400) || ownTunnels.Load() != 2 || mgmtTunnels.Load() != 1 {
		t.Fatalf("custom = %v tunnels mgmt=%d own=%d", out, mgmtTunnels.Load(), ownTunnels.Load())
	}
	if got := ownLogin.Load(); got != "Basic "+base64.StdEncoding.EncodeToString([]byte("carol:s3cret")) {
		t.Fatalf("proxy login = %v", got)
	}

	// An edit dialog's empty URL uses the stored proxy.
	id := rig.create(t, `{"name":"svc","preset":"custom","base_url":"`+target+`","model":"m","proxy_mode":"custom","proxy_url":"`+ownURL+`"}`)
	out = testAI(t, rig, fmt.Sprintf(`{"id":%d,"base_url":"%s","model":"m","proxy_mode":"custom"}`, id, target))
	if out["outcome"] != "ok" || ownTunnels.Load() != 3 {
		t.Fatalf("stored proxy = %v own=%d", out, ownTunnels.Load())
	}
	// Without a stored proxy, custom needs one.
	rec := rig.do(t, http.MethodPost, "/api/v1/ai/configs/test", `{"base_url":"`+target+`","model":"m","proxy_mode":"custom"}`)
	wantFieldInvalid(t, rec, "proxy_url")

	// A failed text call leaves the image unchecked: its failure would be
	// blamed on the image.
	before := hits.Load()
	out = testAI(t, rig, `{"base_url":"http://127.0.0.1:1/v1","model":"m","check_image":true}`)
	if out["outcome"] != "connection" || out["image"] != nil || hits.Load() != before {
		t.Fatalf("failed text = %v", out)
	}
}

// A vision model answers accepted.
func TestAIConfigTestImageAccepted(t *testing.T) {
	rig := newAIConfigRig(t)
	var hits atomic.Int64
	origin := completionFixture(t, true, &hits)
	out := testAI(t, rig, `{"base_url":"`+origin.URL+`/v1","model":"m","proxy_mode":"direct","check_image":true}`)
	if out["outcome"] != "ok" || out["image"] != "accepted" || hits.Load() != 2 {
		t.Fatalf("vision = %v hits=%d", out, hits.Load())
	}
}
