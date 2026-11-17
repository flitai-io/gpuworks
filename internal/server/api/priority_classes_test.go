package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestPriorityClassesAPI(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	signer, err := auth.NewTokenSigner("platform-token-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	user, err := managementSessionTestToken(t, rig.st, signer, auth.Claims{UserID: 2, Username: "user", Role: "user"})
	if err != nil {
		t.Fatal(err)
	}

	// Anyone signed in reads the classes with the bounds and policies.
	rec := rig.do(user, http.MethodGet, "/api/v1/priority-classes", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"min_value":-100`, `"max_value":100`, `"policies":["within_ceiling","fixed","down_only"]`, `"default_policy":"within_ceiling"`, `"ceiling_policies":["within_ceiling"]`,
		`{"name":"critical","display_name":"","description":"","value":40,"builtin":true,"default":false,"deletable":false,"created_at":`,
		`{"name":"normal","display_name":"","description":"","value":0,"builtin":true,"default":true,"deletable":false,"created_at":`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("list lacks %s: %s", want, body)
		}
	}
	if rec := rig.do("", http.MethodGet, "/api/v1/priority-classes", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list = %d", rec.Code)
	}

	// Only an administrator changes them.
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/priority-classes", `{"name":"gold","value":30}`},
		{http.MethodPut, "/api/v1/priority-classes/high", `{"display_name":"x"}`},
		{http.MethodDelete, "/api/v1/priority-classes/high", ""},
		{http.MethodPut, "/api/v1/priority-classes/high/default", ""},
		{http.MethodPut, "/api/v1/apikeys/gw-x/priority", `{"class":"high"}`},
	} {
		if rec := rig.do(user, tc.method, tc.path, tc.body); rec.Code != http.StatusForbidden {
			t.Fatalf("user %s %s = %d", tc.method, tc.path, rec.Code)
		}
		if rec := rig.do("", tc.method, tc.path, tc.body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s %s = %d", tc.method, tc.path, rec.Code)
		}
	}

	expect := func(rec interface {
		Result() *http.Response
	}, status int, code string) {
		t.Helper()
		res := rec.Result()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		if res.StatusCode != status || (code != "" && out["code"] != code) {
			t.Fatalf("status = %d body = %v, want %d %s", res.StatusCode, out, status, code)
		}
	}
	expect(rig.do(rig.session, http.MethodPost, "/api/v1/priority-classes", `{"name":"gold","display_name":"Gold","value":30}`), http.StatusCreated, "")
	// deletable says what DELETE would do: a class of one's own, unbound.
	deletable := func(name string) bool {
		t.Helper()
		var out struct {
			Items []struct {
				Name      string `json:"name"`
				Deletable bool   `json:"deletable"`
			} `json:"items"`
		}
		_ = json.Unmarshal(rig.do(rig.session, http.MethodGet, "/api/v1/priority-classes", "").Body.Bytes(), &out)
		for _, c := range out.Items {
			if c.Name == name {
				return c.Deletable
			}
		}
		t.Fatalf("no class %s", name)
		return false
	}
	if !deletable("gold") || deletable("low") {
		t.Fatalf("deletable: gold %v, low %v", deletable("gold"), deletable("low"))
	}
	expect(rig.do(rig.session, http.MethodPost, "/api/v1/priority-classes", `{"name":"gold","value":30}`), http.StatusConflict, messages.ErrorPriorityClassExists)
	expect(rig.do(rig.session, http.MethodPost, "/api/v1/priority-classes", `{"name":"Gold","value":30}`), http.StatusBadRequest, messages.ErrorPriorityClassInvalid)
	expect(rig.do(rig.session, http.MethodPost, "/api/v1/priority-classes", `{"name":"x","value":101}`), http.StatusBadRequest, messages.ErrorPriorityClassInvalid)
	expect(rig.do(rig.session, http.MethodPost, "/api/v1/priority-classes", `{"name":"x"}`), http.StatusBadRequest, messages.ErrorPriorityClassInvalid)
	expect(rig.do(rig.session, http.MethodPost, "/api/v1/priority-classes", `{"name":"x","value":"high"}`), http.StatusBadRequest, "")
	expect(rig.do(rig.session, http.MethodPut, "/api/v1/priority-classes/high", `{"value":5}`), http.StatusConflict, messages.ErrorPriorityClassBuiltin)
	expect(rig.do(rig.session, http.MethodPut, "/api/v1/priority-classes/high", `{"display_name":"高"}`), http.StatusOK, "")
	expect(rig.do(rig.session, http.MethodPut, "/api/v1/priority-classes/nope", `{"display_name":"x"}`), http.StatusNotFound, messages.ErrorPriorityClassNotFound)
	expect(rig.do(rig.session, http.MethodDelete, "/api/v1/priority-classes/low", ""), http.StatusConflict, messages.ErrorPriorityClassBuiltin)

	// A key bound to gold keeps it from being deleted.
	created := rig.do(rig.session, http.MethodPost, "/api/v1/apikeys", `{"name":"svc","priority":{"class":"gold","policy":"fixed"}}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create key = %d %s", created.Code, created.Body)
	}
	var key struct{ Key string }
	_ = json.Unmarshal(created.Body.Bytes(), &key)
	prefix := auth.PrefixOf(key.Key)
	list := rig.do(rig.session, http.MethodGet, "/api/v1/apikeys", "").Body.String()
	if !strings.Contains(list, `"priority":{"class":"gold","ceiling":"","policy":"fixed"}`) {
		t.Fatalf("key list = %s", list)
	}
	expect(rig.do(rig.session, http.MethodDelete, "/api/v1/priority-classes/gold", ""), http.StatusConflict, messages.ErrorPriorityClassInUse)
	if deletable("gold") {
		t.Fatal("a class a key is bound to reads as deletable")
	}
	// A user may still create a key, but not with a priority.
	expect(rig.do(user, http.MethodPost, "/api/v1/apikeys", `{"name":"mine","priority":{"class":"critical"}}`), http.StatusForbidden, "")
	expect(rig.do(rig.session, http.MethodPost, "/api/v1/apikeys", `{"name":"bad","priority":{"class":"platinum"}}`), http.StatusBadRequest, messages.ErrorPriorityClassInvalid)
	expect(rig.do(rig.session, http.MethodPost, "/api/v1/apikeys", `{"name":"bad2","priority":{"policy":"strict"}}`), http.StatusBadRequest, messages.ErrorPriorityPolicyInvalid)
	expect(rig.do(rig.session, http.MethodPost, "/api/v1/apikeys", `{"name":"bad3","priority":{"class":"high","ceiling":"low"}}`), http.StatusBadRequest, messages.ErrorPriorityClassInvalid)
	// A refused binding leaves no key behind.
	if list := rig.do(rig.session, http.MethodGet, "/api/v1/apikeys", "").Body.String(); strings.Contains(list, `"name":"bad`) || strings.Contains(list, `"name":"mine"`) {
		t.Fatalf("a key refused for its priority exists: %s", list)
	}

	rec = rig.do(rig.session, http.MethodPut, "/api/v1/apikeys/"+prefix+"/priority", `{"class":"normal","ceiling":"critical","policy":"within_ceiling"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"priority":{"class":"normal","ceiling":"critical","policy":"within_ceiling"}`) {
		t.Fatalf("bind = %d %s", rec.Code, rec.Body)
	}
	expect(rig.do(rig.session, http.MethodPut, "/api/v1/apikeys/gw-none/priority", `{}`), http.StatusNotFound, ErrorAPIKeyNotFound)
	expect(rig.do(rig.session, http.MethodPut, "/api/v1/apikeys/"+prefix+"/priority", `{"policy":"strict"}`), http.StatusBadRequest, messages.ErrorPriorityPolicyInvalid)

	// Gold is free now: make it the default, which then cannot be deleted.
	rec = rig.do(rig.session, http.MethodPut, "/api/v1/priority-classes/gold/default", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"gold","display_name":"Gold","description":"","value":30,"builtin":false,"default":true`) ||
		strings.Contains(rec.Body.String(), `"name":"normal","display_name":"","description":"","value":0,"builtin":true,"default":true`) {
		t.Fatalf("default = %d %s", rec.Code, rec.Body)
	}
	expect(rig.do(rig.session, http.MethodDelete, "/api/v1/priority-classes/gold", ""), http.StatusConflict, messages.ErrorPriorityClassInUse)
	if deletable("gold") {
		t.Fatal("the default reads as deletable")
	}
	expect(rig.do(rig.session, http.MethodPut, "/api/v1/priority-classes/nope/default", ""), http.StatusNotFound, messages.ErrorPriorityClassNotFound)
	expect(rig.do(rig.session, http.MethodPut, "/api/v1/priority-classes/normal/default", ""), http.StatusOK, "")
	if rec := rig.do(rig.session, http.MethodDelete, "/api/v1/priority-classes/gold", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	expect(rig.do(rig.session, http.MethodDelete, "/api/v1/priority-classes/gold", ""), http.StatusNotFound, messages.ErrorPriorityClassNotFound)
}
