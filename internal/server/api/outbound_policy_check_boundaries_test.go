package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

func TestOutboundPolicyCheckRequiresAnAdministratorAndPOST(t *testing.T) {
	router, admin, member, _ := outboundCheckRouter(t)
	const path = "/api/v1/server/outbound-policy/check"
	for _, tc := range []struct {
		token string
		want  int
	}{{"", 401}, {member, 403}, {admin, 200}} {
		reply := serverSettingsCall(router, http.MethodPost, path, tc.token, `{"hosts":[]}`)
		if reply.Code != tc.want {
			t.Fatalf("role status=%d body=%s want=%d", reply.Code, reply.Body, tc.want)
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodTrace} {
		if reply := serverSettingsCall(router, method, path, admin, `{"hosts":[]}`); reply.Code != 405 {
			t.Errorf("%s status=%d body=%s", method, reply.Code, reply.Body)
		}
	}
}

func TestOutboundPolicyCheckAcceptsAnEmptyOrMaximumList(t *testing.T) {
	router, admin, _, _ := outboundCheckRouter(t)
	const path = "/api/v1/server/outbound-policy/check"
	if reply := serverSettingsCall(router, http.MethodPost, path, admin, `{"hosts":[]}`); reply.Code != 200 || strings.TrimSpace(reply.Body.String()) != `{"items":[],"total":0}` {
		t.Fatalf("empty list status=%d body=%s", reply.Code, reply.Body)
	}
	hosts := make([]string, 32)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("8.8.8.%d", i+1)
	}
	body, err := json.Marshal(dto.ServerOutboundPolicyCheck{Hosts: hosts})
	if err != nil {
		t.Fatal(err)
	}
	reply := serverSettingsCall(router, http.MethodPost, path, admin, string(body))
	var checked dto.List[dto.ServerOutboundHostCheck]
	if reply.Code != 200 || json.Unmarshal(reply.Body.Bytes(), &checked) != nil || len(checked.Items) != 32 || checked.Total != 32 || checked.Items[31].Host != "8.8.8.32" {
		t.Fatalf("maximum list status=%d body=%s", reply.Code, reply.Body)
	}
	for _, item := range checked.Items {
		if item.Status != "allowed" {
			t.Errorf("public literal=%+v", item)
		}
	}
}

func TestOutboundPolicyCheckValidatesEveryHostBeforeResolvingAny(t *testing.T) {
	queries := outboundDNSFixture(t, map[string][]netip.Addr{"public.gwks.test": {netip.MustParseAddr("8.8.8.8")}})
	router, admin, _, _ := outboundCheckRouter(t)
	invalid := []string{"", " ", "source:443", "[::1]", "[::1]:80", "http://host", "host/path", "host?token=private-token", "user@host", "10.0.0.0/8", "*.corp", "a_b.corp", "bücher.example", "fe80::1%eth0", "127.0.0.01", "1234", "host..corp", "-host", "host-", strings.Repeat("a", 64) + ".corp", strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 62)}
	for _, host := range invalid {
		body, err := json.Marshal(dto.ServerOutboundPolicyCheck{Hosts: []string{"public.gwks.test", host}})
		if err != nil {
			t.Fatal(err)
		}
		reply := serverSettingsCall(router, http.MethodPost, "/api/v1/server/outbound-policy/check", admin, string(body))
		if reply.Code != 400 || !strings.Contains(reply.Body.String(), ErrorServerOutboundPolicyInvalid) || strings.Contains(reply.Body.String(), "private-token") {
			t.Errorf("invalid host %q: status=%d body=%s", host, reply.Code, reply.Body)
		}
	}
	if queries.Load() != 0 {
		t.Fatalf("invalid batch resolved %d DNS questions", queries.Load())
	}
}

func TestOutboundPolicyCheckCanonicalizesHostsAndKeepsDNSFailurePrivate(t *testing.T) {
	longHost := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	outboundDNSFixture(t, map[string][]netip.Addr{
		"canonical.gwks.test": {netip.MustParseAddr("10.1.2.3")},
		longHost:              {netip.MustParseAddr("192.168.1.10")},
	})
	router, admin, _, _ := outboundCheckRouter(t)
	body, err := json.Marshal(dto.ServerOutboundPolicyCheck{Hosts: []string{" CANONICAL.GWKS.TEST. ", "::ffff:127.0.0.1", "2001:4860:4860::8888", "FD00:EC2::254", "missing.gwks.test", longHost}})
	if err != nil {
		t.Fatal(err)
	}
	reply := serverSettingsCall(router, http.MethodPost, "/api/v1/server/outbound-policy/check", admin, string(body))
	want := `{"items":[{"host":"canonical.gwks.test","status":"needs_allow"},{"host":"127.0.0.1","status":"needs_allow"},{"host":"2001:4860:4860::8888","status":"allowed"},{"host":"fd00:ec2::254","status":"blocked"},{"host":"missing.gwks.test","status":"unresolved"},{"host":"` + longHost + `","status":"needs_allow"}],"total":6}`
	if reply.Code != 200 || strings.TrimSpace(reply.Body.String()) != want {
		t.Fatalf("normalized list status=%d body=%s, want=%s", reply.Code, reply.Body, want)
	}
}

func TestOutboundPolicyCheckReflectsOnlyExplicitPolicyWrites(t *testing.T) {
	router, admin, _, settings := outboundCheckRouter(t)
	const policy = "/api/v1/server/outbound-policy"
	const hosts = `{"hosts":["127.0.0.1","10.1.2.3","169.254.169.254","fd00:ec2::254"]}`
	check := func(want string) {
		t.Helper()
		reply := serverSettingsCall(router, http.MethodPost, policy+"/check", admin, hosts)
		if reply.Code != 200 || strings.TrimSpace(reply.Body.String()) != want {
			t.Fatalf("policy check status=%d body=%s want=%s", reply.Code, reply.Body, want)
		}
	}
	check(`{"items":[{"host":"127.0.0.1","status":"needs_allow"},{"host":"10.1.2.3","status":"needs_allow"},{"host":"169.254.169.254","status":"blocked"},{"host":"fd00:ec2::254","status":"blocked"}],"total":4}`)
	write := serverSettingsCall(router, http.MethodPut, policy, admin, `{"restrict_private_hosts":true,"allow_private_hosts":["127.0.0.1"],"revision":"initial"}`)
	if write.Code != 200 {
		t.Fatalf("explicit allow status=%d body=%s", write.Code, write.Body)
	}
	check(`{"items":[{"host":"127.0.0.1","status":"allowed"},{"host":"10.1.2.3","status":"needs_allow"},{"host":"169.254.169.254","status":"blocked"},{"host":"fd00:ec2::254","status":"blocked"}],"total":4}`)
	before := serverSettingsCall(router, http.MethodGet, policy, admin, "")
	if reply := serverSettingsCall(router, http.MethodPut, policy, admin, `{"restrict_private_hosts":false,"allow_private_hosts":[],"revision":"initial"}`); reply.Code != 409 {
		t.Fatalf("stale revision status=%d body=%s", reply.Code, reply.Body)
	}
	after := serverSettingsCall(router, http.MethodGet, policy, admin, "")
	if after.Code != 200 || after.Body.String() != before.Body.String() {
		t.Fatalf("stale write changed policy: before=%s after=%s", before.Body, after.Body)
	}
	settings.value.Editable = false
	if reply := serverSettingsCall(router, http.MethodPut, policy, admin, `{"restrict_private_hosts":false,"allow_private_hosts":[],"revision":"updated-1"}`); reply.Code != 409 {
		t.Fatalf("read-only write status=%d body=%s", reply.Code, reply.Body)
	}
	check(`{"items":[{"host":"127.0.0.1","status":"allowed"},{"host":"10.1.2.3","status":"needs_allow"},{"host":"169.254.169.254","status":"blocked"},{"host":"fd00:ec2::254","status":"blocked"}],"total":4}`)
	settings.value.Editable = true
	if reply := serverSettingsCall(router, http.MethodPut, policy, admin, `{"restrict_private_hosts":false,"allow_private_hosts":["127.0.0.1"],"revision":"updated-1"}`); reply.Code != 200 {
		t.Fatalf("open mode write status=%d body=%s", reply.Code, reply.Body)
	}
	check(`{"items":[{"host":"127.0.0.1","status":"allowed"},{"host":"10.1.2.3","status":"allowed"},{"host":"169.254.169.254","status":"blocked"},{"host":"fd00:ec2::254","status":"blocked"}],"total":4}`)
	got := serverSettingsCall(router, http.MethodGet, policy, admin, "")
	if !strings.Contains(got.Body.String(), `"allow_private_hosts":["127.0.0.1"]`) {
		t.Fatalf("disabling strict mode erased configured hosts: %s", got.Body)
	}
}

func TestOutboundPolicyCheckNeverContactsTheUpstreamHTTPProxy(t *testing.T) {
	var requests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, "controlled HTTP proxy response")
	}))
	t.Cleanup(proxy.Close)
	router, admin, _, _ := outboundCheckRouter(t)
	setting := serverSettingsCall(router, http.MethodPost, "/api/v1/pull/proxies", admin, fmt.Sprintf(`{"http_proxy":%q,"control_plane":true,"node_requirements":{}}`, proxy.URL))
	if setting.Code != 201 {
		t.Fatalf("proxy setup status=%d body=%s", setting.Code, setting.Body)
	}
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(proxyURL)
	t.Cleanup(transport.CloseIdleConnections)
	positive, err := (&http.Client{Transport: transport}).Get("http://8.8.8.8/controlled-positive")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(positive.Body)
	positive.Body.Close()
	if err != nil || string(body) != "controlled HTTP proxy response" || requests.Load() != 1 {
		t.Fatalf("HTTP probe fixture ineffective: body=%s err=%v requests=%d", body, err, requests.Load())
	}
	checked := serverSettingsCall(router, http.MethodPost, "/api/v1/server/outbound-policy/check", admin, `{"hosts":["8.8.8.8","127.0.0.1"]}`)
	if checked.Code != 200 || strings.TrimSpace(checked.Body.String()) != `{"items":[{"host":"8.8.8.8","status":"allowed"},{"host":"127.0.0.1","status":"needs_allow"}],"total":2}` || requests.Load() != 1 {
		t.Fatalf("check contacted upstream: status=%d body=%s requests=%d", checked.Code, checked.Body, requests.Load())
	}
}
