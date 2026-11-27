package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

func TestKubePrecheckOverTheWire(t *testing.T) {
	rig := newTokenRig(t)
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/precheck",
		`{"server_url":"https://gw.example:8800/","node_selector":"gpuworks.ai/pool=a","image_pull_secret":"regcred"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("precheck = %d %s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"file_name", "image", "script", "server"}) {
		t.Fatalf("keys %v", keys)
	}
	// An empty registry and namespace take what a new entry takes.
	if out["file_name"] != "gpuworks-precheck.sh" || out["server"] != "gw.example:8800" ||
		out["image"] != "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-agent:r1" {
		t.Fatalf("precheck = %v", out)
	}
	script := out["script"].(string)
	for _, want := range []string{"#!/usr/bin/env bash\n", "\nMODE='precheck'\n", "\nNS='gpuworks-system'\n", "\nPULL_SECRET='regcred'\n",
		`"gpuworks.ai/pool": "a"`, `image: "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-agent:r1"`} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	if strings.Contains(script, "token") {
		t.Error("the pre-check carries a token")
	}
	// Nothing was stored.
	if rec := rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters", ""); !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("list after a precheck = %s", rec.Body)
	}
}

func TestKubePrecheckChecksTheImageTheEntryWouldRun(t *testing.T) {
	rig := newTokenRig(t)
	for body, want := range map[string]string{
		`{"server_url":"http://10.0.0.5:8800","agent_image_source":"dockerhub"}`:                          "gpuworks/gpuworks-agent:r1",
		`{"server_url":"http://10.0.0.5:8800","agent_image_source":"quay","agent_image":" reg.lan/a:1 "}`: "reg.lan/a:1",
	} {
		rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/precheck", body)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusOK || out["image"] != want || out["server"] != "10.0.0.5:8800" {
			t.Errorf("%s = %d %v", body, rec.Code, out)
		}
	}
}

func TestKubePrecheckValidation(t *testing.T) {
	rig := newTokenRig(t)
	for body, field := range map[string]string{
		`{}`:                             "server_url",
		`{"server_url":"gw.example:80"}`: "server_url",
		`{"server_url":"https://"}`:      "server_url",
		`{"server_url":"https://gw.example","node_selector":"no pairs here"}`:  "node_selector",
		`{"server_url":"https://gw.example","image_pull_secret":"Not A Name"}`: "image_pull_secret",
		`{"server_url":"https://gw.example","agent_image_source":"ghcr"}`:      "agent_image_source",
		`{"server_url":"https://gw.example","namespace":"Bad_Namespace"}`:      "manifest",
	} {
		rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/precheck", body)
		var out struct {
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusBadRequest || out.Code != ErrorKubeClusterInvalid || out.Params["field"] != field {
			t.Errorf("%s = %d %s, want field %s", body, rec.Code, rec.Body, field)
		}
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/precheck", `{"server_url":`); rec.Code != http.StatusBadRequest {
		t.Errorf("broken JSON = %d", rec.Code)
	}
	if rec := rig.do(rig.session, http.MethodGet, "/api/v1/kube/precheck", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d", rec.Code)
	}
}

func TestKubePrecheckIsForAdministrators(t *testing.T) {
	rig := newTokenRig(t)
	body := `{"server_url":"https://gw.example"}`
	if rec := rig.do("", http.MethodPost, "/api/v1/kube/precheck", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", rec.Code)
	}
	signer, err := auth.NewTokenSigner("platform-token-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	user, err := managementSessionTestToken(t, rig.st, signer, auth.Claims{UserID: 2, Username: "user", Role: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if rec := rig.do(user, http.MethodPost, "/api/v1/kube/precheck", body); rec.Code != http.StatusForbidden {
		t.Fatalf("user = %d", rec.Code)
	}
}

func TestKubeVerifyCommandServesTheCheckByToken(t *testing.T) {
	rig := newTokenRig(t)
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters",
		`{"name":"prod","server_url":"https://gw.example:8800/","namespace":"gpu-a"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	verify, _ := created["verify_command"].(string)
	const prefix, suffix = "curl -fsSL 'https://gw.example:8800/kube/check.sh?token=", "' | bash"
	if !strings.HasPrefix(verify, prefix) || !strings.HasSuffix(verify, suffix) {
		t.Fatalf("verify_command = %q", verify)
	}
	token, err := url.QueryUnescape(strings.TrimSuffix(strings.TrimPrefix(verify, prefix), suffix))
	if err != nil || !strings.Contains(created["apply_command"].(string), url.QueryEscape(token)) {
		t.Fatalf("the check's token %q is not the apply command's (%v)", token, err)
	}
	rec = rig.do("", http.MethodGet, "/kube/check.sh?token="+url.QueryEscape(token), "")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/x-shellscript; charset=utf-8" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("check by token = %d %v", rec.Code, rec.Header())
	}
	for _, want := range []string{"\nMODE='verify'\n", "\nNS='gpu-a'\n", "\nDS='gpuworks-agent'\n"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("check lacks %q", want)
		}
	}
	if strings.Contains(rec.Body.String(), token) {
		t.Error("the check repeats the token")
	}
	if rec := rig.do("", http.MethodGet, "/kube/check.sh?token=nope", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("wrong token = %d", rec.Code)
	}
	if rec := rig.do("", http.MethodGet, "/kube/check.sh", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("no token = %d", rec.Code)
	}
	// A revoked entry has neither command, and its token serves nothing.
	uuid := created["uuid"].(string)
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters/"+uuid+"/revoke", ""); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do("", http.MethodGet, "/kube/check.sh?token="+url.QueryEscape(token), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("revoked token = %d", rec.Code)
	}
	if rec := rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters", ""); strings.Contains(rec.Body.String(), "verify_command") {
		t.Fatalf("revoked entry lists a verify command: %s", rec.Body)
	}
}

func TestKubeVerifyCommandIsForAdministrators(t *testing.T) {
	rig := newTokenRig(t)
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", kubeCreateBody); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body)
	}
	signer, err := auth.NewTokenSigner("platform-token-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	user, err := managementSessionTestToken(t, rig.st, signer, auth.Claims{UserID: 2, Username: "user", Role: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if rec := rig.do(user, http.MethodGet, "/api/v1/kube/clusters", ""); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "verify_command") {
		t.Fatalf("user list = %d %s", rec.Code, rec.Body)
	}
}
