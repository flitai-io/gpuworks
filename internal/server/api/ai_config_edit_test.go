package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// aiConfigRig is a router over an in-memory store with a real secretbox, so a
// key is encrypted the way production encrypts it, plus a token to call it.
type aiConfigRig struct {
	router http.Handler
	store  *store.Store
	box    *auth.Secretbox
	signer *auth.TokenSigner
	token  string
}

func newAIConfigRig(t *testing.T, allowPrivateHosts ...string) *aiConfigRig {
	t.Helper()
	st, err := store.Open(":memory:", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	signer, err := auth.NewTokenSigner("ai-config-edit", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("master")
	if err != nil {
		t.Fatal(err)
	}
	rig := &aiConfigRig{store: st, box: box, signer: signer, token: token}
	rig.router = rig.routerAllowing(append([]string{"127.0.0.1"}, allowPrivateHosts...))
	return rig
}

// routerAllowing rebuilds the routes with one outbound allowlist, so a test can
// show what an address the policy refuses looks like.
func (r *aiConfigRig) routerAllowing(allowPrivateHosts []string) http.Handler {
	return Router(Deps{
		Store: r.store, AuthSigner: r.signer, AISecretbox: r.box,
		Outside: OutsideSources{AllowPrivateHosts: allowPrivateHosts},
	})
}

func (r *aiConfigRig) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+r.token)
	rec := httptest.NewRecorder()
	r.router.ServeHTTP(rec, req)
	return rec
}

// create stores one service through the API and answers its id.
func (r *aiConfigRig) create(t *testing.T, body string) uint {
	t.Helper()
	rec := r.do(t, http.MethodPost, "/api/v1/ai/configs", body)
	var created struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); rec.Code != http.StatusCreated || err != nil || created.ID == 0 {
		t.Fatalf("create = %d %s (err %v)", rec.Code, rec.Body.String(), err)
	}
	return created.ID
}

// config reads one row as the list reports it.
func (r *aiConfigRig) config(t *testing.T, id uint) dtoRow {
	t.Helper()
	rec := r.do(t, http.MethodGet, "/api/v1/ai/configs", "")
	var rows []dtoRow
	if err := unmarshalList(rec.Body.Bytes(), &rows); rec.Code != http.StatusOK || err != nil {
		t.Fatalf("list = %d %s (err %v)", rec.Code, rec.Body.String(), err)
	}
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("config %d missing from %+v", id, rows)
	return dtoRow{}
}

// dtoRow is one wire row: exactly the fields the console shows.
type dtoRow struct {
	ID        uint   `json:"id"`
	Name      string `json:"name"`
	Preset    string `json:"preset"`
	BaseURL   string `json:"base_url"`
	Model     string `json:"model"`
	IsDefault bool   `json:"is_default"`
}

// storedKey answers the key as persisted, so a test can tell a kept key from a
// rewritten one.
func (r *aiConfigRig) storedKey(t *testing.T, id uint) string {
	t.Helper()
	c, err := r.store.AIConfigByID(id)
	if err != nil {
		t.Fatal(err)
	}
	return c.APIKey
}

// The edit dialog sends only the fields it changed; the rest must survive, and
// a blank key must keep the stored one.
func TestUpdateAIConfigAppliesEachFieldAlone(t *testing.T) {
	rig := newAIConfigRig(t)
	id := rig.create(t, `{"name":"primary","preset":"deepseek","base_url":"https://api.deepseek.com","api_key":"sk-original","model":"deepseek-v4-flash","is_default":true}`)
	before := rig.config(t, id)
	keyBefore := rig.storedKey(t, id)
	if keyBefore == "sk-original" || !strings.HasPrefix(keyBefore, "enc:v1:") {
		t.Fatalf("key was not encrypted: %q", keyBefore)
	}
	for _, tc := range []struct {
		name  string
		body  string
		check func(t *testing.T, row dtoRow)
	}{
		{"name", `{"name":"renamed"}`, func(t *testing.T, row dtoRow) {
			if row.Name != "renamed" {
				t.Fatalf("name = %q", row.Name)
			}
		}},
		{"preset", `{"preset":"custom"}`, func(t *testing.T, row dtoRow) {
			if row.Preset != "custom" {
				t.Fatalf("preset = %q", row.Preset)
			}
		}},
		{"base_url", `{"base_url":"https://api.deepseek.com/v1"}`, func(t *testing.T, row dtoRow) {
			if row.BaseURL != "https://api.deepseek.com/v1" {
				t.Fatalf("base_url = %q", row.BaseURL)
			}
		}},
		{"model", `{"model":"deepseek-v4"}`, func(t *testing.T, row dtoRow) {
			if row.Model != "deepseek-v4" {
				t.Fatalf("model = %q", row.Model)
			}
		}},
		{"is_default", `{"is_default":true}`, func(t *testing.T, row dtoRow) {
			if !row.IsDefault {
				t.Fatal("is_default not kept")
			}
		}},
	} {
		rec := rig.do(t, http.MethodPatch, fmt.Sprintf("/api/v1/ai/configs/%d", id), tc.body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: patch = %d %s", tc.name, rec.Code, rec.Body.String())
		}
		tc.check(t, rig.config(t, id))
		// Every other field is untouched, and the key is never returned.
		after := rig.config(t, id)
		if after.ID != before.ID || after.Model == "" || after.Name == "" || after.BaseURL == "" {
			t.Fatalf("%s: other fields changed: %+v -> %+v", tc.name, before, after)
		}
		if strings.Contains(rec.Body.String(), "api_key") || strings.Contains(rec.Body.String(), "sk-original") {
			t.Fatalf("%s: the response carried a key: %s", tc.name, rec.Body.String())
		}
		if got := rig.storedKey(t, id); got != keyBefore {
			t.Fatalf("%s: the stored key changed: %q -> %q", tc.name, keyBefore, got)
		}
	}
}

// A blank key in the patch keeps the stored ciphertext byte for byte; a filled
// one replaces it with a fresh encryption of the new key.
func TestUpdateAIConfigKeyBlankKeepsStoredFilledReplaces(t *testing.T) {
	rig := newAIConfigRig(t)
	id := rig.create(t, `{"name":"primary","base_url":"https://api.deepseek.com/v1","api_key":"sk-original","model":"m"}`)
	kept := rig.storedKey(t, id)
	if rec := rig.do(t, http.MethodPatch, fmt.Sprintf("/api/v1/ai/configs/%d", id), `{"api_key":""}`); rec.Code != http.StatusOK {
		t.Fatalf("blank key patch = %d %s", rec.Code, rec.Body.String())
	}
	if got := rig.storedKey(t, id); got != kept {
		t.Fatalf("a blank key rewrote the stored one: %q -> %q", kept, got)
	}
	if rec := rig.do(t, http.MethodPatch, fmt.Sprintf("/api/v1/ai/configs/%d", id), `{"api_key":"sk-replaced"}`); rec.Code != http.StatusOK {
		t.Fatalf("filled key patch = %d %s", rec.Code, rec.Body.String())
	}
	replaced := rig.storedKey(t, id)
	if replaced == kept {
		t.Fatal("a filled key did not replace the stored one")
	}
	plain, err := store.DecryptAIKey(rig.box, replaced)
	if err != nil || plain != "sk-replaced" {
		t.Fatalf("stored key decrypts to %q (%v)", plain, err)
	}
}

// A config stored before keys were encrypted keeps its plaintext value when an
// edit does not name the key: the dialog never saw the key, so it cannot send
// it back, and re-encrypting would need the operator to retype it.
func TestUpdateAIConfigKeepsLegacyPlaintextKey(t *testing.T) {
	rig := newAIConfigRig(t)
	c, err := rig.store.CreateAIConfig(&store.AIConfig{Name: "legacy", BaseURL: "https://api.deepseek.com", APIKey: "sk-plaintext", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	rec := rig.do(t, http.MethodPatch, fmt.Sprintf("/api/v1/ai/configs/%d", c.ID), `{"name":"renamed","api_key":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body.String())
	}
	if got := rig.storedKey(t, c.ID); got != "sk-plaintext" {
		t.Fatalf("legacy key changed to %q", got)
	}
}

// Marking a service the default clears the mark on the others, so the console
// and the assistant agree on which one is in use.
func TestUpdateAIConfigKeepsOneDefault(t *testing.T) {
	rig := newAIConfigRig(t)
	first := rig.create(t, `{"name":"first","base_url":"https://api.deepseek.com","model":"m"}`)
	second := rig.create(t, `{"name":"second","base_url":"https://api.deepseek.com","model":"m"}`)
	if !rig.config(t, first).IsDefault {
		t.Fatal("the first service should be the default")
	}
	rec := rig.do(t, http.MethodPatch, fmt.Sprintf("/api/v1/ai/configs/%d", second), `{"is_default":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body.String())
	}
	if rig.config(t, first).IsDefault || !rig.config(t, second).IsDefault {
		t.Fatalf("two defaults: first=%+v second=%+v", rig.config(t, first), rig.config(t, second))
	}
}

func TestUpdateAIConfigUnknownID(t *testing.T) {
	rig := newAIConfigRig(t)
	rec := rig.do(t, http.MethodPatch, "/api/v1/ai/configs/404", `{"name":"gone"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("patch of a missing service = %d %s", rec.Code, rec.Body.String())
	}
}

// A rejected value must name the field, so the dialog marks the input the
// operator has to fix instead of showing a banner.
func TestUpdateAIConfigRejectsInvalidValues(t *testing.T) {
	rig := newAIConfigRig(t)
	id := rig.create(t, `{"name":"primary","base_url":"https://api.deepseek.com","model":"m"}`)
	path := fmt.Sprintf("/api/v1/ai/configs/%d", id)
	long := strings.Repeat("a", 65)
	for _, tc := range []struct {
		name  string
		body  string
		field string
	}{
		{"a blank name", `{"name":"   "}`, "name"},
		{"a name with control characters", `{"name":"bad\u0000name"}`, "name"},
		{"a model longer than its column", fmt.Sprintf(`{"model":%q}`, long), "model"},
		{"a blank model", `{"model":"  "}`, "model"},
		{"a blank address", `{"base_url":"   "}`, "base_url"},
		{"an address without a scheme", `{"base_url":"api.deepseek.com"}`, "base_url"},
		{"an address of another scheme", `{"base_url":"file:///etc/passwd"}`, "base_url"},
		{"an address that is only the call path", `{"base_url":"/chat/completions"}`, "base_url"},
		{"a malformed body", `{"name":`, "body"},
	} {
		rec := rig.do(t, http.MethodPatch, path, tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: patch = %d %s", tc.name, rec.Code, rec.Body.String())
		}
		var answer struct {
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
			t.Fatalf("%s: %v in %s", tc.name, err, rec.Body.String())
		}
		if answer.Code != ErrorFieldInvalid || answer.Params["field"] != tc.field {
			t.Fatalf("%s: code=%s params=%v", tc.name, answer.Code, answer.Params)
		}
	}
	// None of the rejections changed the stored service.
	row := rig.config(t, id)
	if row.Name != "primary" || row.BaseURL != "https://api.deepseek.com" || row.Model != "m" {
		t.Fatalf("a rejected patch changed the service: %+v", row)
	}
}

// An address pasted with its call path is stored as the base address, so the
// assistant's own call does not double it - the 404 the bug reported.
func TestAIConfigStoresNormalizedAddress(t *testing.T) {
	rig := newAIConfigRig(t)
	for body, want := range map[string]string{
		`{"name":"pasted","base_url":"http://198.51.100.122:8800/v1/chat/completions","model":"m"}`: "http://198.51.100.122:8800/v1",
		`{"name":"trailing","base_url":"https://api.deepseek.com/v1///","model":"m"}`:               "https://api.deepseek.com/v1",
		`{"name":"bare","base_url":"https://api.deepseek.com","model":"m"}`:                         "https://api.deepseek.com",
	} {
		if got := rig.config(t, rig.create(t, body)).BaseURL; got != want {
			t.Fatalf("%s: stored %q, want %q", body, got, want)
		}
	}
	// A create that names no address is not rejected: a preset fills it in,
	// and a service stored without one is a configuration the assistant
	// reports as unusable rather than a write that failed.
	if got := rig.config(t, rig.create(t, `{"name":"none","model":"m"}`)).BaseURL; got != "" {
		t.Fatalf("an address the create did not name was invented: %q", got)
	}
	// A preset's address is stored in the normalized form too.
	if got := rig.config(t, rig.create(t, `{"name":"preset","preset":"deepseek"}`)).BaseURL; got != "https://api.deepseek.com" {
		t.Fatalf("preset address = %q", got)
	}
}

// The connection test makes the assistant's call against the values a form
// holds and stores nothing; each answer an operator must act on differently is
// its own outcome.
func TestAIConfigTestReportsEachOutcome(t *testing.T) {
	const served = "served-model"
	var calls int
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"detail":"Not Found"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-right" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"detail":"invalid or missing API key"}`)
			return
		}
		var body aihelper.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != served {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"detail":"no deployment serves model %q"}`, body.Model)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}]}`)
	}))
	defer endpoint.Close()
	rig := newAIConfigRig(t)
	for _, tc := range []struct {
		name    string
		body    string
		outcome string
	}{
		{"a served model answers", fmt.Sprintf(`{"base_url":%q,"api_key":"sk-right","model":%q}`, endpoint.URL+"/v1", served), string(aihelper.ProbeOK)},
		{"a wrong key", fmt.Sprintf(`{"base_url":%q,"api_key":"sk-wrong","model":%q}`, endpoint.URL+"/v1", served), string(aihelper.ProbeAuth)},
		{"no key", fmt.Sprintf(`{"base_url":%q,"model":%q}`, endpoint.URL+"/v1", served), string(aihelper.ProbeAuth)},
		// The address the bug report recorded is normalized to the base
		// address before it is called, so it now answers instead of 404ing.
		{"a pasted call path is normalized and answers", fmt.Sprintf(`{"base_url":%q,"api_key":"sk-right","model":%q}`, endpoint.URL+"/v1/chat/completions", served), string(aihelper.ProbeOK)},
		{"an address of the wrong path", fmt.Sprintf(`{"base_url":%q,"api_key":"sk-right","model":%q}`, endpoint.URL+"/wrong", served), string(aihelper.ProbePath)},
		{"a model the endpoint does not serve", fmt.Sprintf(`{"base_url":%q,"api_key":"sk-right","model":"missing"}`, endpoint.URL+"/v1"), string(aihelper.ProbeModel)},
		{"an address that does not answer", `{"base_url":"http://127.0.0.1:1/v1","api_key":"sk-right","model":"m"}`, string(aihelper.ProbeConnection)},
	} {
		rec := rig.do(t, http.MethodPost, "/api/v1/ai/configs/test", tc.body)
		var got struct {
			Outcome string `json:"outcome"`
			BaseURL string `json:"base_url"`
			Detail  string `json:"detail"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); rec.Code != http.StatusOK || err != nil {
			t.Fatalf("%s: test = %d %s (err %v)", tc.name, rec.Code, rec.Body.String(), err)
		}
		if got.Outcome != tc.outcome {
			t.Fatalf("%s: outcome = %q (%s), want %q", tc.name, got.Outcome, rec.Body.String(), tc.outcome)
		}
		if strings.Contains(rec.Body.String(), "sk-") {
			t.Fatalf("%s: the answer echoed a key: %s", tc.name, rec.Body.String())
		}
	}
	// A test stores nothing: the list is still empty afterwards.
	rec := rig.do(t, http.MethodGet, "/api/v1/ai/configs", "")
	var rows []dtoRow
	if err := unmarshalList(rec.Body.Bytes(), &rows); err != nil || len(rows) != 0 {
		t.Fatalf("a connection test stored a service: %s (err %v)", rec.Body.String(), err)
	}
	if calls == 0 {
		t.Fatal("the probe never reached the endpoint")
	}
}

// An address the platform's outbound policy refuses is reported as such: the
// operator fixes it in the allowlist, not at the provider.
func TestAIConfigTestReportsBlockedAddress(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer endpoint.Close()
	// No allowPrivateHosts: the endpoint is a private address.
	rig := newAIConfigRig(t)
	rig.router = rig.routerAllowing(nil)
	rec := rig.do(t, http.MethodPost, "/api/v1/ai/configs/test",
		fmt.Sprintf(`{"base_url":%q,"api_key":"sk-right","model":"m"}`, endpoint.URL))
	var got struct {
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); rec.Code != http.StatusOK || err != nil {
		t.Fatalf("test = %d %s (err %v)", rec.Code, rec.Body.String(), err)
	}
	if got.Outcome != string(aihelper.ProbeBlocked) {
		t.Fatalf("outcome = %q, want blocked: %s", got.Outcome, rec.Body.String())
	}
}

// The edit dialog sends the stored service's id with a blank key to mean
// "keep the stored key". That key is used while the address is still the
// stored one, and never for an address the caller substituted.
func TestAIConfigTestUsesStoredKeyOnlyForItsOwnAddress(t *testing.T) {
	var seen []string
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`)
	}))
	defer endpoint.Close()
	rig := newAIConfigRig(t)
	id := rig.create(t, fmt.Sprintf(`{"name":"primary","base_url":%q,"api_key":"sk-stored","model":"m"}`, endpoint.URL))
	// The stored address: the stored key is used.
	if rec := rig.do(t, http.MethodPost, "/api/v1/ai/configs/test", fmt.Sprintf(`{"id":%d,"base_url":%q,"model":"m"}`, id, endpoint.URL)); rec.Code != http.StatusOK {
		t.Fatalf("test = %d %s", rec.Code, rec.Body.String())
	}
	if len(seen) != 1 || seen[0] != "Bearer sk-stored" {
		t.Fatalf("the stored key was not used for its own address: %v", seen)
	}
	// Another address: a blank key stays blank rather than following the id.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "other "+r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer other.Close()
	if rec := rig.do(t, http.MethodPost, "/api/v1/ai/configs/test", fmt.Sprintf(`{"id":%d,"base_url":%q,"model":"m"}`, id, other.URL)); rec.Code != http.StatusOK {
		t.Fatalf("test = %d %s", rec.Code, rec.Body.String())
	}
	for _, header := range seen[1:] {
		if strings.Contains(header, "sk-stored") {
			t.Fatalf("the stored key followed the id to another address: %v", seen)
		}
	}
}

// A test rejects the values a save would reject, naming the field, and never
// calls the endpoint for them.
func TestAIConfigTestRejectsInvalidValues(t *testing.T) {
	rig := newAIConfigRig(t)
	for _, tc := range []struct {
		name  string
		body  string
		field string
	}{
		{"a blank address", `{"base_url":"  ","model":"m"}`, "base_url"},
		{"an address without a scheme", `{"base_url":"api.deepseek.com","model":"m"}`, "base_url"},
		{"an address of another scheme", `{"base_url":"ftp://host/v1","model":"m"}`, "base_url"},
		{"a blank model", `{"base_url":"https://api.deepseek.com/v1","model":" "}`, "model"},
	} {
		rec := rig.do(t, http.MethodPost, "/api/v1/ai/configs/test", tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: test = %d %s", tc.name, rec.Code, rec.Body.String())
		}
		var answer struct {
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil || answer.Code != ErrorFieldInvalid || answer.Params["field"] != tc.field {
			t.Fatalf("%s: %s (err %v)", tc.name, rec.Body.String(), err)
		}
	}
}

// Both routes are management writes: they need a token and refuse the wrong
// method rather than answering a router default.
func TestAIConfigEditRoutesRequireAuthentication(t *testing.T) {
	rig := newAIConfigRig(t)
	id := rig.create(t, `{"name":"primary","base_url":"https://api.deepseek.com","model":"m"}`)
	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodPatch, fmt.Sprintf("/api/v1/ai/configs/%d", id), `{"name":"x"}`},
		{http.MethodPost, "/api/v1/ai/configs/test", `{"base_url":"https://api.deepseek.com","model":"m"}`},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		rec := httptest.NewRecorder()
		rig.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without a token = %d", tc.method, tc.path, rec.Code)
		}
	}
	if rec := rig.do(t, http.MethodPut, fmt.Sprintf("/api/v1/ai/configs/%d", id), `{"name":"x"}`); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT on a service = %d", rec.Code)
	}
	if rec := rig.do(t, http.MethodPatch, "/api/v1/ai/configs", `{"name":"x"}`); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PATCH on the collection = %d", rec.Code)
	}
}

// A name the API accepts on create is a name it accepts on edit: the two paths
// once held different bounds, so a name within the create limit (256 runes) and
// over the edit limit (64) was stored and then refused by every edit - the edit
// dialog always sends the name back, so that service could not be changed again
// through the console. Both paths now apply aiConfigNameMaxRunes, and the
// boundary is the same on each.
func TestAIConfigNameBoundIsTheSameOnCreateAndEdit(t *testing.T) {
	rig := newAIConfigRig(t)
	// The longest accepted name, created through the API.
	name := strings.Repeat("n", aiConfigNameMaxRunes)
	rec := rig.do(t, http.MethodPost, "/api/v1/ai/configs",
		fmt.Sprintf(`{"name":%q,"base_url":"https://api.deepseek.com","model":"m"}`, name))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with a %d-character name = %d %s", aiConfigNameMaxRunes, rec.Code, rec.Body.String())
	}
	var created struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == 0 {
		t.Fatalf("create answer = %s (err %v)", rec.Body.String(), err)
	}
	// The edit dialog sends every field it shows, the name included: this is
	// the request the console makes when the operator only fixes the model.
	path := fmt.Sprintf("/api/v1/ai/configs/%d", created.ID)
	rec = rig.do(t, http.MethodPatch, path,
		fmt.Sprintf(`{"name":%q,"preset":"deepseek","base_url":"https://api.deepseek.com","model":"deepseek-v4"}`, name))
	if rec.Code != http.StatusOK {
		t.Fatalf("edit of a service created with that name = %d %s", rec.Code, rec.Body.String())
	}
	if row := rig.config(t, created.ID); row.Model != "deepseek-v4" || row.Name != name {
		t.Fatalf("the edit did not apply: %+v", row)
	}
	// One character over is refused by both paths, with the same field named:
	// neither path can store what the other refuses.
	longer := strings.Repeat("x", aiConfigNameMaxRunes+1)
	if rec := rig.do(t, http.MethodPost, "/api/v1/ai/configs",
		fmt.Sprintf(`{"name":%q,"base_url":"https://api.deepseek.com","model":"m"}`, longer)); rec.Code != http.StatusBadRequest {
		t.Fatalf("create with a %d-character name = %d %s", aiConfigNameMaxRunes+1, rec.Code, rec.Body.String())
	}
	rec = rig.do(t, http.MethodPatch, path, fmt.Sprintf(`{"name":%q}`, longer))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("edit to a %d-character name = %d %s", aiConfigNameMaxRunes+1, rec.Code, rec.Body.String())
	}
	var answer struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil || answer.Params["field"] != "name" {
		t.Fatalf("rejection = %s (err %v)", rec.Body.String(), err)
	}
}

// The bound counts characters, not bytes: a 30-character Chinese name is 90
// bytes, was refused by a byte comparison whose message claimed characters, and
// is a name the API now accepts on both paths.
func TestAIConfigNameBoundCountsCharacters(t *testing.T) {
	rig := newAIConfigRig(t)
	name := strings.Repeat("模", 30)
	if len(name) <= aiConfigNameMaxRunes {
		t.Fatalf("the fixture is only %d bytes; it does not exercise the bound", len(name))
	}
	id := rig.create(t, fmt.Sprintf(`{"name":%q,"base_url":"https://api.deepseek.com","model":"m"}`, name))
	if rec := rig.do(t, http.MethodPatch, fmt.Sprintf("/api/v1/ai/configs/%d", id), fmt.Sprintf(`{"name":%q}`, name)); rec.Code != http.StatusOK {
		t.Fatalf("edit keeping a 30-character Chinese name = %d %s", rec.Code, rec.Body.String())
	}
	// Over the bound in characters is still refused.
	over := strings.Repeat("模", aiConfigNameMaxRunes+1)
	rec := rig.do(t, http.MethodPatch, fmt.Sprintf("/api/v1/ai/configs/%d", id), fmt.Sprintf(`{"name":%q}`, over))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("edit to %d Chinese characters = %d %s", aiConfigNameMaxRunes+1, rec.Code, rec.Body.String())
	}
	var answer struct {
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if detail, _ := answer.Params["detail"].(string); !strings.Contains(detail, "characters") {
		t.Fatalf("the rejection does not name the rule: %v", answer.Params)
	}
}

// A name stored by an older build with a wider limit stays editable: the
// unchanged name is not re-validated against the bound, so the operator can
// still fix the address or the model.
func TestUpdateAIConfigKeepsAPreExistingOverlongNameEditable(t *testing.T) {
	rig := newAIConfigRig(t)
	// Written straight to the store, the way a build with the wider create
	// bound left it behind.
	legacy := strings.Repeat("L", 120)
	row, err := rig.store.CreateAIConfig(&store.AIConfig{
		Name: legacy, BaseURL: "https://api.deepseek.com", Model: "m",
	})
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/ai/configs/%d", row.ID)
	// The dialog sends the stored name back; the model change must apply.
	rec := rig.do(t, http.MethodPatch, path,
		fmt.Sprintf(`{"name":%q,"base_url":"https://api.deepseek.com","model":"deepseek-v4"}`, legacy))
	if rec.Code != http.StatusOK {
		t.Fatalf("edit of a pre-existing long name = %d %s", rec.Code, rec.Body.String())
	}
	if got := rig.config(t, row.ID); got.Model != "deepseek-v4" || got.Name != legacy {
		t.Fatalf("the edit did not apply: %+v", got)
	}
	// Changing the name is still held to the bound.
	shorter := strings.Repeat("s", 10)
	if rec := rig.do(t, http.MethodPatch, path, fmt.Sprintf(`{"name":%q}`, shorter)); rec.Code != http.StatusOK {
		t.Fatalf("renaming a legacy row to a valid name = %d %s", rec.Code, rec.Body.String())
	}
	if got := rig.config(t, row.ID); got.Name != shorter {
		t.Fatalf("rename did not apply: %+v", got)
	}
}

// A connection test whose blank key means "use the stored one" works for a row
// stored before addresses were normalized on write. Comparing the normalized
// request base with the RAW stored value ran the probe keyless, the endpoint
// answered 401, and the dialog told the operator to check a key that was
// correct - which is the report the ticket started from.
func TestAIConfigTestUsesStoredKeyForARawStoredAddress(t *testing.T) {
	var seen []string
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization")+" "+r.URL.RequestURI())
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`)
	}))
	defer endpoint.Close()
	rig := newAIConfigRig(t)
	// The address as an older build stored it: verbatim, call path included.
	raw := endpoint.URL + "/v1/chat/completions"
	row, err := rig.store.CreateAIConfig(&store.AIConfig{
		Name: "legacy", BaseURL: raw, APIKey: "enc:v1:unused", Model: "m",
	})
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := store.EncryptAIKey(rig.box, "sk-stored")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.store.UpdateAIConfig(row.ID, store.AIConfigPatch{APIKey: &encrypted}); err != nil {
		t.Fatal(err)
	}
	// The dialog sends the base address it derived from the form, with no key.
	rec := rig.do(t, http.MethodPost, "/api/v1/ai/configs/test",
		fmt.Sprintf(`{"id":%d,"base_url":%q,"model":"m"}`, row.ID, endpoint.URL+"/v1"))
	var got struct {
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); rec.Code != http.StatusOK || err != nil {
		t.Fatalf("test = %d %s (err %v)", rec.Code, rec.Body.String(), err)
	}
	if got.Outcome != string(aihelper.ProbeOK) {
		t.Fatalf("outcome = %q, want ok: %s", got.Outcome, rec.Body.String())
	}
	if len(seen) != 1 || !strings.HasPrefix(seen[0], "Bearer sk-stored ") {
		t.Fatalf("the stored key was not used for a raw stored address: %v", seen)
	}
	if !strings.Contains(seen[0], "/v1/chat/completions") {
		t.Fatalf("the probe did not call the stored address's call path: %v", seen)
	}
	// Another host still gets no stored key.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "other "+r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer other.Close()
	if rec := rig.do(t, http.MethodPost, "/api/v1/ai/configs/test",
		fmt.Sprintf(`{"id":%d,"base_url":%q,"model":"m"}`, row.ID, other.URL+"/v1")); rec.Code != http.StatusOK {
		t.Fatalf("test = %d %s", rec.Code, rec.Body.String())
	}
	for _, header := range seen[1:] {
		if strings.Contains(header, "sk-stored") {
			t.Fatalf("the stored key followed an address substitution: %v", seen)
		}
	}
}

// A delete of a service that is already gone answers 404, so a stale console is
// told what happened instead of being shown a success for a row that was never
// there.
func TestDeleteAIConfigMissingRowAnswers404(t *testing.T) {
	rig := newAIConfigRig(t)
	rec := rig.do(t, http.MethodDelete, "/api/v1/ai/configs/404", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete of a missing service = %d %s", rec.Code, rec.Body.String())
	}
	rec = rig.do(t, http.MethodDelete, "/api/v1/ai/configs/not-a-number", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete of an unparseable id = %d %s", rec.Code, rec.Body.String())
	}
}

// Deleting the service the assistant uses answers which one took over, so the
// console can say where the assistant went instead of letting the egress notice
// change host unannounced.
func TestDeleteAIConfigAnswersThePromotedService(t *testing.T) {
	rig := newAIConfigRig(t)
	first := rig.create(t, `{"name":"first","base_url":"https://api.deepseek.com","model":"m"}`)
	second := rig.create(t, `{"name":"second","base_url":"https://api.deepseek.com","model":"m"}`)
	rec := rig.do(t, http.MethodDelete, fmt.Sprintf("/api/v1/ai/configs/%d", first), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	var answer struct {
		Status     string `json:"status"`
		PromotedID uint   `json:"promoted_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("delete answer = %s (err %v)", rec.Body.String(), err)
	}
	if answer.Status != "deleted" || answer.PromotedID != second {
		t.Fatalf("delete answer = %+v, want the promoted id %d", answer, second)
	}
	// And the assistant now reads the promoted service.
	list := rig.do(t, http.MethodGet, "/api/v1/ai/configs", "")
	var rows []dtoRow
	if err := unmarshalList(list.Body.Bytes(), &rows); err != nil || len(rows) != 1 || !rows[0].IsDefault {
		t.Fatalf("after the delete the remaining service is not the default: %s (err %v)", list.Body.String(), err)
	}
	// Deleting a service that is not the default promotes nothing.
	third := rig.create(t, `{"name":"third","base_url":"https://api.deepseek.com","model":"m"}`)
	rec = rig.do(t, http.MethodDelete, fmt.Sprintf("/api/v1/ai/configs/%d", third), "")
	answer = struct {
		Status     string `json:"status"`
		PromotedID uint   `json:"promoted_id"`
	}{}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil || answer.PromotedID != 0 || answer.Status != "deleted" {
		t.Fatalf("delete of a non-default answered %+v (err %v)", answer, err)
	}
}

// Clearing the switch on the service the assistant uses is refused with a code
// the console can translate and a field it can mark, in keeping with the rule
// that a stored service set always has exactly one default.
func TestUpdateAIConfigRefusesToClearTheOnlyDefault(t *testing.T) {
	rig := newAIConfigRig(t)
	id := rig.create(t, `{"name":"only","base_url":"https://api.deepseek.com","model":"m"}`)
	rec := rig.do(t, http.MethodPatch, fmt.Sprintf("/api/v1/ai/configs/%d", id), `{"is_default":false}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("clearing the only default = %d %s", rec.Code, rec.Body.String())
	}
	var answer struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil || answer.Code != ErrorAIDefaultRequired || answer.Params["field"] != "is_default" {
		t.Fatalf("refusal = %s (err %v)", rec.Body.String(), err)
	}
	// The stored service is unchanged, and it is still the one in use.
	if row := rig.config(t, id); !row.IsDefault {
		t.Fatalf("a refused patch cleared the default: %+v", row)
	}
}

// A patch that names only a preset brings that provider's address and model,
// the way a create fills them: a row labelled Alibaba Cloud must not keep
// calling api.deepseek.com.
func TestUpdateAIConfigPresetFillsMissingAddressAndModel(t *testing.T) {
	rig := newAIConfigRig(t)
	id := rig.create(t, `{"name":"primary","preset":"deepseek","base_url":"https://api.deepseek.com","model":"deepseek-v4-flash"}`)
	preset, ok := aihelper.Presets["bailian"]
	if !ok {
		t.Skip("the bailian preset is not defined")
	}
	rec := rig.do(t, http.MethodPatch, fmt.Sprintf("/api/v1/ai/configs/%d", id), `{"preset":"bailian"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("preset-only patch = %d %s", rec.Code, rec.Body.String())
	}
	row := rig.config(t, id)
	wantBase, _ := aihelper.NormalizeBaseURL(preset.BaseURL)
	if row.Preset != "bailian" || row.BaseURL != wantBase || row.Model != preset.Model {
		t.Fatalf("preset-only patch stored %+v, want base %q model %q", row, wantBase, preset.Model)
	}
	// An explicit value in the same patch wins over the preset's fill.
	rec = rig.do(t, http.MethodPatch, fmt.Sprintf("/api/v1/ai/configs/%d", id),
		`{"preset":"deepseek","model":"deepseek-v4"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("preset patch with an explicit model = %d %s", rec.Code, rec.Body.String())
	}
	row = rig.config(t, id)
	deepseek, _ := aihelper.NormalizeBaseURL(aihelper.Presets["deepseek"].BaseURL)
	if row.BaseURL != deepseek || row.Model != "deepseek-v4" {
		t.Fatalf("an explicit model was overwritten: %+v", row)
	}
}
