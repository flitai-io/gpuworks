package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/community"
)

func TestCommunityReposNameTheConfiguredTagAPIHostWithoutItsSecrets(t *testing.T) {
	st := managementSessionTestStore(t)
	signer, err := auth.NewTokenSigner("community-access-host", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := managementSessionTestToken(t, st, signer, auth.Claims{Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	router := Router(Deps{Store: st, AuthSigner: signer})
	config := `{"repos":[{"repo":"registry.corp/team/image","registry":"oci","engine":"vllm","upstream_api":"https://tags.corp/private/path?token=synthetic"}]}`
	written := serverSettingsCall(router, http.MethodPut, "/api/v1/settings/image_community_catalog", admin, config)
	if written.Code != http.StatusOK {
		t.Fatalf("catalog setup status=%d body=%s", written.Code, written.Body)
	}
	listed := serverSettingsCall(router, http.MethodGet, "/api/v1/images/community/repos", admin, "")
	if listed.Code != http.StatusOK {
		t.Fatalf("repo list status=%d body=%s", listed.Code, listed.Body)
	}
	var rows []map[string]json.RawMessage
	if err := unmarshalList(listed.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if string(row["repo"]) != `"registry.corp/team/image"` {
			continue
		}
		found = true
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		const want = `{"access_host":"tags.corp","engine":"vllm","registry":"oci","repo":"registry.corp/team/image"}`
		if string(encoded) != want {
			t.Fatalf("configured repository wire=%s, want=%s", encoded, want)
		}
	}
	if !found {
		t.Fatal("configured repository is absent")
	}
	for _, secret := range []string{"upstream_api", "private/path", "synthetic", "?token="} {
		if strings.Contains(listed.Body.String(), secret) {
			t.Fatalf("community list leaked configuration details: %s", listed.Body)
		}
	}
}

func TestCommunityReposHonorRegistryDialectsAndNormalizeOnlyTheActualHost(t *testing.T) {
	st := managementSessionTestStore(t)
	signer, admin := roleTestToken(t, st, "admin")
	member, err := managementSessionTestToken(t, st, signer, auth.Claims{Username: "member", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	router := Router(Deps{Store: st, AuthSigner: signer})
	config := `{"repos":[
		{"repo":"dialect.corp/team/hub","registry":"docker-hub","engine":"vllm"},
		{"repo":"dialect.corp/team/quay","registry":"quay","engine":"vllm"},
		{"repo":"docker.io/team/oci","registry":"oci","engine":"vllm"},
		{"repo":"registry.corp:7443/team/image","registry":"oci","engine":"vllm"},
		{"repo":"ipv6.corp/team/image","engine":"vllm","upstream_api":"https://[FD00:EC2::254]:8443/private?token=synthetic-ipv6"},
		{"repo":"mapped.corp/team/image","engine":"vllm","upstream_api":"https://[::ffff:127.0.0.1]:8443/tags"},
		{"repo":"credentialed.corp/team/image","engine":"vllm","upstream_api":"https://synthetic-user:synthetic-password@tags.corp/private?token=synthetic-credential"}
	]}`
	if reply := serverSettingsCall(router, http.MethodPut, "/api/v1/settings/image_community_catalog", admin, config); reply.Code != 200 {
		t.Fatalf("catalog setup status=%d body=%s", reply.Code, reply.Body)
	}
	const path = "/api/v1/images/community/repos"
	if reply := serverSettingsCall(router, http.MethodGet, path, "", ""); reply.Code != 401 {
		t.Fatalf("anonymous list status=%d", reply.Code)
	}
	if reply := serverSettingsCall(router, http.MethodPost, path, admin, `{}`); reply.Code != 405 {
		t.Fatalf("unsupported method status=%d", reply.Code)
	}
	listed := serverSettingsCall(router, http.MethodGet, path, member, "")
	if listed.Code != 200 {
		t.Fatalf("member list status=%d body=%s", listed.Code, listed.Body)
	}
	var rows []map[string]json.RawMessage
	if err := unmarshalList(listed.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"dialect.corp/team/hub": "hub.docker.com", "dialect.corp/team/quay": "quay.io", "docker.io/team/oci": "docker.io",
		"registry.corp:7443/team/image": "registry.corp", "ipv6.corp/team/image": "fd00:ec2::254", "mapped.corp/team/image": "127.0.0.1", "credentialed.corp/team/image": "",
	}
	for _, row := range rows {
		var repo string
		if err := json.Unmarshal(row["repo"], &repo); err != nil {
			t.Fatal(err)
		}
		expected, found := want[repo]
		if !found {
			continue
		}
		delete(want, repo)
		if expected == "" {
			if _, exists := row["access_host"]; exists {
				t.Errorf("unusable target must not guess a host: %v", row)
			}
			continue
		}
		if string(row["access_host"]) != `"`+expected+`"` {
			t.Errorf("%s access_host=%s want=%q", repo, row["access_host"], expected)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing configured repositories: %v", want)
	}
	for _, secret := range []string{"upstream_api", "synthetic-", "private?", ":8443"} {
		if strings.Contains(listed.Body.String(), secret) {
			t.Fatalf("community list leaked source configuration: %s", listed.Body)
		}
	}
}

func TestCommunityBrowseHostFollowsItsResolvedRepositoryAndSibling(t *testing.T) {
	st := managementSessionTestStore(t)
	signer, admin := roleTestToken(t, st, "admin")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch {
		case r.URL.Host == "tags.corp" && r.URL.Path == "/private/path" && r.URL.Query().Get("token") == "synthetic":
			body = `{"tags":["v1.0.0"]}`
		case r.URL.Host == "quay.io" && r.URL.Path == "/api/v1/repository/team/sibling/tag/" && r.URL.Query().Get("limit") == "100":
			body = `{"tags":[{"name":"v1.0.0"}],"has_additional":false}`
		default:
			return nil, fmt.Errorf("unexpected community fixture request")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	router := Router(Deps{Store: st, AuthSigner: signer, Outside: OutsideSources{Community: community.NewBrowser(client)}})
	config := `{"repos":[{"repo":"registry.corp/team/image","registry":"oci","engine":"vllm","upstream_api":"https://tags.corp/private/path?token=synthetic","siblings":["quay.io/team/sibling"]}]}`
	if reply := serverSettingsCall(router, http.MethodPut, "/api/v1/settings/image_community_catalog", admin, config); reply.Code != 200 {
		t.Fatalf("catalog setup status=%d body=%s", reply.Code, reply.Body)
	}
	for _, tc := range []struct{ repo, host string }{{"registry.corp/team/image", "tags.corp"}, {"quay.io/team/sibling", "quay.io"}} {
		browsed := serverSettingsCall(router, http.MethodGet, "/api/v1/images/community/browse?repo="+tc.repo, admin, "")
		var row map[string]json.RawMessage
		if browsed.Code != 200 || json.Unmarshal(browsed.Body.Bytes(), &row) != nil || string(row["access_host"]) != `"`+tc.host+`"` || string(row["repo"]) != `"`+tc.repo+`"` {
			t.Errorf("browse=%d %s, expected repo=%q host=%q", browsed.Code, browsed.Body, tc.repo, tc.host)
		}
		if strings.Contains(browsed.Body.String(), "synthetic") || strings.Contains(browsed.Body.String(), "upstream_api") {
			t.Fatalf("browse leaked upstream configuration: %s", browsed.Body)
		}
	}
}
