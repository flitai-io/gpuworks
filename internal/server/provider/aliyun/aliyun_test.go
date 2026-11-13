package aliyun

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

// The vector was computed independently (Python, from the V3 signature
// spec) for the same inputs; a mismatch means the canonical request drifted.
func TestSignMatchesIndependentVector(t *testing.T) {
	query := map[string]string{"RegionId": "cn-shanghai", "InstanceIds": `["i-abc"]`, "MaxResults": "100", "Name": "a b*c~"}
	if got, want := canonicalQuery(query), "InstanceIds=%5B%22i-abc%22%5D&MaxResults=100&Name=a%20b%2Ac~&RegionId=cn-shanghai"; got != want {
		t.Fatalf("canonical query = %s\nwant %s", got, want)
	}
	got := sign(signedRequest{
		method: http.MethodPost, host: "ecs.cn-shanghai.aliyuncs.com", query: query,
		headers: map[string]string{
			"x-acs-action": "DescribeInstances", "x-acs-version": "2014-05-26", "x-acs-date": "2026-10-14T16:00:00Z",
			"x-acs-signature-nonce": "0123456789abcdef", "x-acs-content-sha256": emptyPayloadHash, "x-acs-security-token": "tok",
		},
	}, "LTAIexample", "secret-key")
	want := "ACS3-HMAC-SHA256 Credential=LTAIexample,SignedHeaders=host;x-acs-action;x-acs-content-sha256;x-acs-date;x-acs-security-token;x-acs-signature-nonce;x-acs-version,Signature=2e9031448c7e7f8d5362a78abc530403b4bc17cf80f0e2e11d13ce9012604f8c"
	if got != want {
		t.Fatalf("authorization = %s\nwant %s", got, want)
	}
}

// fakeAliyun answers RPC calls like the real API: it checks the signature
// against the secret it knows, routes on x-acs-action, and returns the
// documented error body for anything it refuses.
type fakeAliyun struct {
	t       *testing.T
	secrets map[string]string // AccessKey ID → secret
	mu      sync.Mutex
	calls   []fakeCall
	handle  func(call fakeCall) (int, any)
}

type fakeCall struct {
	host, action, token string
	method, path        string
	query               map[string]string
}

func (f *fakeAliyun) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	query := map[string]string{}
	for k, v := range r.URL.Query() {
		query[k] = v[0]
	}
	headers := map[string]string{}
	for name := range r.Header {
		if lower := strings.ToLower(name); strings.HasPrefix(lower, "x-acs-") {
			headers[lower] = r.Header.Get(name)
		}
	}
	auth := r.Header.Get("Authorization")
	keyID := strings.TrimPrefix(strings.SplitN(auth, ",", 2)[0], signAlgorithm+" Credential=")
	secret, ok := f.secrets[keyID]
	if !ok || sign(signedRequest{method: r.Method, host: r.Host, path: r.URL.Path, query: query, headers: headers}, keyID, secret) != auth {
		writeJSON(w, http.StatusBadRequest, map[string]string{"Code": "IncompleteSignature", "Message": "The request signature does not conform to Aliyun standards.", "RequestId": "req-sig"})
		return
	}
	call := fakeCall{host: r.Host, action: headers["x-acs-action"], token: headers["x-acs-security-token"], method: r.Method, path: r.URL.Path, query: query}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	status, body := f.handle(call)
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeAliyun) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.action)
	}
	return out
}

// newFake starts a fake API and a driver whose every endpoint host resolves
// to it, except hosts listed in down, which refuse connections.
func newFake(t *testing.T, handle func(fakeCall) (int, any), down ...string) (*Driver, *fakeAliyun) {
	t.Helper()
	fake := &fakeAliyun{t: t, secrets: map[string]string{"LTAIuser": "user-secret", "STS.temp": "temp-secret"}, handle: handle}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	closed := closedPort(t)
	d := New()
	d.api.baseURL = func(host string) *url.URL {
		for _, h := range down {
			if h == host {
				return &url.URL{Scheme: "http", Host: closed}
			}
		}
		return &url.URL{Scheme: "http", Host: target.Host}
	}
	d.api.sleep = func(context.Context, time.Duration) error { return nil }
	return d, fake
}

func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

var userCreds = provider.Credentials{PublicKey: "LTAIuser", PrivateKey: "user-secret"}

func regionsBody(ids ...string) map[string]any {
	regions := []map[string]string{}
	for _, id := range ids {
		regions = append(regions, map[string]string{"RegionId": id, "RegionEndpoint": "ecs." + id + ".aliyuncs.com", "LocalName": id})
	}
	return map[string]any{"Regions": map[string]any{"Region": regions}, "RequestId": "req-regions"}
}

// instanceJSON is shaped like a real DescribeInstances item (fields the
// driver does not read trimmed).
func instanceJSON(id, region, status, public, private string, gpus int, spec string) map[string]any {
	publicIPs := []string{}
	if public != "" {
		publicIPs = append(publicIPs, public)
	}
	return map[string]any{
		"InstanceId": id, "InstanceName": "name-" + id, "RegionId": region, "ZoneId": region + "-b", "Status": status,
		"OSType": "linux", "OSName": "Ubuntu  24.04 64位", "GPUAmount": gpus, "GPUSpec": spec,
		"InstanceType": "ecs.gn7i-c8g1.2xlarge", "InstanceChargeType": "PostPaid",
		"PublicIpAddress": map[string]any{"IpAddress": publicIPs},
		"EipAddress":      map[string]any{"IpAddress": "", "AllocationId": ""},
		"VpcAttributes":   map[string]any{"PrivateIpAddress": map[string]any{"IpAddress": []string{private}}, "VpcId": "vpc-1"},
		"InnerIpAddress":  map[string]any{"IpAddress": []string{}},
	}
}

func TestListInstancesPagesRegionsAndMapsInstallPaths(t *testing.T) {
	d, fake := newFake(t, func(c fakeCall) (int, any) {
		switch c.action {
		case "DescribeRegions":
			return 200, regionsBody("cn-shanghai", "cn-beijing")
		case "DescribeInstances":
			if c.query["MaxResults"] != "100" {
				t.Errorf("MaxResults = %q", c.query["MaxResults"])
			}
			switch c.query["RegionId"] + "/" + c.query["NextToken"] {
			case "cn-shanghai/":
				return 200, map[string]any{"Instances": map[string]any{"Instance": []any{
					instanceJSON("i-pub", "cn-shanghai", "Running", "47.0.0.1", "172.16.0.1", 1, "NVIDIA A10"),
				}}, "NextToken": "page2"}
			case "cn-shanghai/page2":
				return 200, map[string]any{"Instances": map[string]any{"Instance": []any{
					instanceJSON("i-private", "cn-shanghai", "Running", "", "172.16.0.2", 2, "NVIDIA L20"),
					instanceJSON("i-stopped", "cn-shanghai", "Stopped", "", "172.16.0.3", 8, "NVIDIA H20"),
					instanceJSON("i-cpu", "cn-shanghai", "Running", "", "172.16.0.4", 0, ""),
				}}, "NextToken": ""}
			case "cn-beijing/":
				return 200, map[string]any{"Instances": map[string]any{"Instance": []any{}}, "NextToken": ""}
			}
		case "DescribeCloudAssistantStatus":
			if c.query["InstanceId.1"] != "i-pub" || c.query["InstanceId.2"] != "i-private" || c.query["InstanceId.3"] != "" {
				t.Errorf("status asked for %v; only running GPU instances belong there", c.query)
			}
			return 200, map[string]any{"InstanceCloudAssistantStatusSet": map[string]any{"InstanceCloudAssistantStatus": []any{
				map[string]any{"InstanceId": "i-pub", "CloudAssistantStatus": "false"},
				map[string]any{"InstanceId": "i-private", "CloudAssistantStatus": "true"},
			}}}
		}
		return 404, map[string]string{"Code": "InvalidAction.NotFound", "Message": c.action}
	})
	insts, err := d.ListInstances(context.Background(), userCreds)
	if err != nil {
		t.Fatal(err)
	}
	// i-cpu has no GPU: it is not listed.
	if len(insts) != 3 {
		t.Fatalf("instances = %+v", insts)
	}
	byID := map[string]provider.Instance{}
	for _, in := range insts {
		byID[in.ExternalID] = in
	}
	pub := byID["i-pub"]
	if pub.GPUModel != "NVIDIA A10" || pub.GPUCount != 1 || pub.Power != provider.PowerRunning || pub.PublicIP != "47.0.0.1" || pub.PrivateIP != "172.16.0.1" || pub.Region != "cn-shanghai" || pub.Kind != "vm" {
		t.Fatalf("i-pub = %+v", pub)
	}
	// Cloud Assistant offline: SSH on the public address only.
	if len(pub.SSHConnections) != 1 || pub.SSHConnections[0].Type != "direct" || pub.SSHConnections[0].Host != "47.0.0.1" || pub.SSHConnections[0].Port != 22 || pub.SSHPort != 22 {
		t.Fatalf("i-pub connections = %+v", pub.SSHConnections)
	}
	// No public address, Cloud Assistant online: that is the one path.
	private := byID["i-private"]
	if len(private.SSHConnections) != 1 || private.SSHConnections[0].Type != provider.ConnectionCloudAssistant || !private.SSHConnections[0].InstallSupported || private.PublicIP != "" {
		t.Fatalf("i-private = %+v", private)
	}
	if stopped := byID["i-stopped"]; stopped.Power != provider.PowerStopped || len(stopped.SSHConnections) != 0 {
		t.Fatalf("i-stopped = %+v", stopped)
	}
	if got := strings.Join(fake.actions(), ","); strings.Count(got, "DescribeInstances") != 3 || strings.Count(got, "DescribeCloudAssistantStatus") != 1 {
		t.Fatalf("calls = %s", got)
	}
}

func TestRegionsFieldLimitsAndValidatesTheScan(t *testing.T) {
	d, fake := newFake(t, func(c fakeCall) (int, any) {
		switch c.action {
		case "DescribeRegions":
			return 200, regionsBody("cn-shanghai", "cn-beijing", "ap-southeast-1")
		case "DescribeInstances":
			return 200, map[string]any{"Instances": map[string]any{"Instance": []any{}}}
		}
		return 404, map[string]string{"Code": "InvalidAction.NotFound"}
	})
	creds := userCreds
	creds.Extra = map[string]string{"regions": "cn-beijing, cn-beijing\nap-southeast-1"}
	if _, err := d.ListInstances(context.Background(), creds); err != nil {
		t.Fatal(err)
	}
	var regions []string
	for _, c := range fake.calls {
		if c.action == "DescribeInstances" {
			regions = append(regions, c.query["RegionId"])
		}
	}
	if len(regions) != 2 {
		t.Fatalf("scanned %v, want cn-beijing and ap-southeast-1 once each", regions)
	}
	creds.Extra["regions"] = "cn-mars-1"
	if err := d.ValidateCredentials(context.Background(), creds); err == nil || !strings.Contains(err.Error(), `region "cn-mars-1" is not available`) {
		t.Fatalf("unknown region error = %v", err)
	}
}

// A region that fails fails the sync: syncing the rest would remove the
// failed region's instances from the inventory.
func TestListInstancesFailsWhenARegionFails(t *testing.T) {
	d, _ := newFake(t, func(c fakeCall) (int, any) {
		switch {
		case c.action == "DescribeRegions":
			return 200, regionsBody("cn-shanghai", "cn-beijing")
		case c.query["RegionId"] == "cn-beijing":
			return 403, map[string]string{"Code": "Forbidden.RAM", "Message": "User not authorized to operate on the specified resource.", "RequestId": "req-forbidden"}
		}
		return 200, map[string]any{"Instances": map[string]any{"Instance": []any{}}}
	})
	_, err := d.ListInstances(context.Background(), userCreds)
	if err == nil || !strings.Contains(err.Error(), "region cn-beijing") || !strings.Contains(err.Error(), "Forbidden.RAM") {
		t.Fatalf("err = %v", err)
	}
}

func TestThrottlingIsRetriedAndOtherErrorsAreNot(t *testing.T) {
	attempts := 0
	d, _ := newFake(t, func(c fakeCall) (int, any) {
		attempts++
		if attempts < 3 {
			return 400, map[string]string{"Code": "Throttling.User", "Message": "Request was denied due to user flow control."}
		}
		return 200, regionsBody("cn-shanghai")
	})
	if err := d.ValidateCredentials(context.Background(), userCreds); err != nil || attempts != 3 {
		t.Fatalf("err = %v after %d attempts", err, attempts)
	}

	attempts = 0
	d, _ = newFake(t, func(c fakeCall) (int, any) {
		attempts++
		return 400, map[string]string{"Code": "InvalidAccessKeyId.NotFound", "Message": "Specified access key is not found.", "RequestId": "req-key"}
	})
	err := d.ValidateCredentials(context.Background(), provider.Credentials{PublicKey: "LTAIuser", PrivateKey: "user-secret"})
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.Code != "InvalidAccessKeyId.NotFound" || attempts != 1 {
		t.Fatalf("err = %v after %d attempts", err, attempts)
	}
}

func TestBadSecretIsRejectedBySignature(t *testing.T) {
	d, _ := newFake(t, func(fakeCall) (int, any) { return 200, regionsBody("cn-shanghai") })
	err := d.ValidateCredentials(context.Background(), provider.Credentials{PublicKey: "LTAIuser", PrivateKey: "wrong"})
	if err == nil || !strings.Contains(err.Error(), "IncompleteSignature") {
		t.Fatalf("err = %v", err)
	}
	if err := d.ValidateCredentials(context.Background(), provider.Credentials{PublicKey: "LTAIuser"}); err == nil || !strings.Contains(err.Error(), "AccessKey Secret are required") {
		t.Fatalf("missing secret err = %v", err)
	}
}

// A regional host this network cannot reach falls back to the central host.
func TestUnreachableRegionalHostFallsBackToCentral(t *testing.T) {
	d, fake := newFake(t, func(c fakeCall) (int, any) {
		if c.action == "DescribeRegions" {
			return 200, regionsBody("cn-wulanchabu")
		}
		return 200, map[string]any{"Instances": map[string]any{"Instance": []any{}}}
	}, "ecs.cn-wulanchabu.aliyuncs.com")
	if _, err := d.ListInstances(context.Background(), userCreds); err != nil {
		t.Fatal(err)
	}
	if got := fake.calls[len(fake.calls)-1]; got.action != "DescribeInstances" || got.query["RegionId"] != "cn-wulanchabu" {
		t.Fatalf("last call = %+v", got)
	}

	// When the central host does not serve the region either, the error
	// names the regional host's failure, not the central host's refusal.
	d, _ = newFake(t, func(c fakeCall) (int, any) {
		if c.action == "DescribeRegions" {
			return 200, regionsBody("cn-wulanchabu")
		}
		return 404, map[string]string{"Code": "InvalidOperation.NotSupportedEndpoint", "Message": "The specified endpoint cant operate this region."}
	}, "ecs.cn-wulanchabu.aliyuncs.com")
	_, err := d.ListInstances(context.Background(), userCreds)
	if err == nil || !strings.Contains(err.Error(), "ecs.cn-wulanchabu.aliyuncs.com") || !strings.Contains(err.Error(), "central endpoint does not serve region cn-wulanchabu") {
		t.Fatalf("err = %v", err)
	}
}

// A regional host the central host cannot stand in for gets its retries: one
// connection reset there must not fail the call.
func TestRegionalHostBlipIsRetriedWhenCentralDoesNotServeTheRegion(t *testing.T) {
	// The regional host resets the first connection, as the real one did
	// from CI (2026-10-28), and answers after that.
	d, fake, regionalCalls := newResettingRegionalHost(t, 1)
	instances, err := d.ListInstances(context.Background(), userCreds)
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 || instances[0].ExternalID != "i-gz" {
		t.Fatalf("instances = %+v", instances)
	}
	if got := regionalCalls.Load(); got != 2 {
		t.Fatalf("regional host called %d times, want the reset attempt and one retry", got)
	}
	if got := centralDescribeInstances(fake); got != 1 {
		t.Fatalf("central host asked %d times, want once", got)
	}
}

// When every retry on the regional host fails too, the call gives up after
// attemptsPerHost tries there and names the regional failure.
func TestRegionalHostDownIsReportedWhenCentralDoesNotServeTheRegion(t *testing.T) {
	d, fake, regionalCalls := newResettingRegionalHost(t, attemptsPerHost)
	_, err := d.ListInstances(context.Background(), userCreds)
	if err == nil || !strings.Contains(err.Error(), "central endpoint does not serve region cn-guangzhou") || strings.Contains(err.Error(), "NotSupportedEndpoint") {
		t.Fatalf("err = %v", err)
	}
	if got := regionalCalls.Load(); got != attemptsPerHost {
		t.Fatalf("regional host called %d times, want %d", got, attemptsPerHost)
	}
	if got := centralDescribeInstances(fake); got != 1 {
		t.Fatalf("central host asked %d times, want once", got)
	}
}

// newResettingRegionalHost serves cn-guangzhou from a regional host that
// resets the first `resets` DescribeInstances connections, behind a central
// host that refuses the region.
func newResettingRegionalHost(t *testing.T, resets int32) (*Driver, *fakeAliyun, *atomic.Int32) {
	t.Helper()
	d, fake := newFake(t, func(c fakeCall) (int, any) {
		switch {
		case c.action == "DescribeRegions":
			return 200, regionsBody("cn-guangzhou")
		case c.host == centralECSHost:
			return 404, map[string]string{"Code": "InvalidOperation.NotSupportedEndpoint", "Message": "The specified endpoint cant operate this region.", "RequestId": "req-central"}
		}
		return 200, map[string]any{"Instances": map[string]any{"Instance": []any{instanceJSON("i-gz", "cn-guangzhou", "Running", "8.134.0.1", "172.16.0.1", 1, "NVIDIA A10")}}}
	})
	regionalCalls := new(atomic.Int32) // DescribeInstances calls
	regional := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-acs-action") == "DescribeInstances" && regionalCalls.Add(1) <= resets {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(regional.Close)
	healthy := d.api.baseURL
	d.api.baseURL = func(host string) *url.URL {
		if host == "ecs.cn-guangzhou.aliyuncs.com" {
			u, _ := url.Parse(regional.URL)
			return u
		}
		return healthy(host)
	}
	return d, fake, regionalCalls
}

func centralDescribeInstances(fake *fakeAliyun) int {
	var n int
	for _, c := range fake.calls {
		if c.host == centralECSHost && c.action == "DescribeInstances" {
			n++
		}
	}
	return n
}

// With a role ARN the AccessKey only assumes the role; every other call is
// signed with the role's temporary key and carries its security token, and
// the temporary key is reused until it nears expiry.
func TestRoleARNAssumesTheRoleOnceAndSignsWithItsToken(t *testing.T) {
	d, fake := newFake(t, func(c fakeCall) (int, any) {
		switch c.action {
		case "AssumeRole":
			if c.host == "" || c.query["RoleArn"] != "acs:ram::123:role/gpuworks" || c.query["RoleSessionName"] != "gpuworks" || c.token != "" {
				t.Errorf("AssumeRole call = %+v", c)
			}
			return 200, map[string]any{"Credentials": map[string]string{
				"AccessKeyId": "STS.temp", "AccessKeySecret": "temp-secret", "SecurityToken": "token-1",
				"Expiration": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			}}
		case "DescribeRegions":
			if c.token != "token-1" {
				t.Errorf("DescribeRegions token = %q", c.token)
			}
			return 200, regionsBody("cn-shanghai")
		}
		return 404, map[string]string{"Code": "InvalidAction.NotFound"}
	})
	creds := userCreds
	creds.Extra = map[string]string{"role_arn": "acs:ram::123:role/gpuworks"}
	for i := 0; i < 2; i++ {
		if err := d.ValidateCredentials(context.Background(), creds); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(fake.actions(), ","); got != "AssumeRole,DescribeRegions,DescribeRegions" {
		t.Fatalf("calls = %s", got)
	}
}

func TestPowerFindsTheRegionAndKeepsChargingOnStop(t *testing.T) {
	d, fake := newFake(t, func(c fakeCall) (int, any) {
		switch c.action {
		case "DescribeRegions":
			return 200, regionsBody("cn-shanghai", "cn-beijing")
		case "DescribeInstances":
			if !strings.HasPrefix(c.query["InstanceIds"], `["i-`) || strings.Count(c.query["InstanceIds"], ",") != 0 {
				t.Errorf("InstanceIds = %q, want one ID", c.query["InstanceIds"])
			}
			if c.query["RegionId"] == "cn-beijing" {
				return 200, map[string]any{"Instances": map[string]any{"Instance": []any{instanceJSON("i-1", "cn-beijing", "Running", "", "10.0.0.1", 1, "NVIDIA A10")}}}
			}
			return 200, map[string]any{"Instances": map[string]any{"Instance": []any{}}}
		case "StopInstance", "StartInstance":
			return 200, map[string]string{"RequestId": "req-power"}
		}
		return 404, map[string]string{"Code": "InvalidAction.NotFound"}
	})
	if state, err := d.PowerState(context.Background(), userCreds, "i-1"); err != nil || state != provider.PowerRunning {
		t.Fatalf("state = %s, %v", state, err)
	}
	if err := d.PowerOff(context.Background(), userCreds, "i-1"); err != nil {
		t.Fatal(err)
	}
	last := fake.calls[len(fake.calls)-1]
	if last.action != "StopInstance" || last.query["RegionId"] != "cn-beijing" || last.query["InstanceId"] != "i-1" || last.query["StoppedMode"] != "KeepCharging" {
		t.Fatalf("stop call = %+v", last)
	}
	if _, err := d.PowerState(context.Background(), userCreds, "i-missing"); err == nil || !strings.Contains(err.Error(), "i-missing not found") {
		t.Fatalf("missing instance err = %v", err)
	}
	if err := d.DeleteInstance(context.Background(), userCreds, "i-1"); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("delete = %v", err)
	}
}

// assistantFake serves the Cloud Assistant calls of one run: status, then
// RunCommand, then results that are Running once before they finish.
func assistantFake(t *testing.T, online bool, finalStatus string, exitCode int, output string) (*Driver, *fakeAliyun) {
	polls := 0
	return newFake(t, func(c fakeCall) (int, any) {
		switch c.action {
		case "DescribeCloudAssistantStatus":
			return 200, map[string]any{"InstanceCloudAssistantStatusSet": map[string]any{"InstanceCloudAssistantStatus": []any{
				map[string]any{"InstanceId": c.query["InstanceId.1"], "CloudAssistantStatus": fmt.Sprint(online)},
			}}}
		case "RunCommand":
			return 200, map[string]string{"CommandId": "c-1", "InvokeId": "t-1", "RequestId": "req-run"}
		case "DescribeInvocationResults":
			polls++
			status := finalStatus
			if polls == 1 {
				status = "Running"
			}
			return 200, map[string]any{"Invocation": map[string]any{"InvocationResults": map[string]any{"InvocationResult": []any{map[string]any{
				"InvokeId": "t-1", "InstanceId": c.query["InstanceId"], "InvocationStatus": status, "ExitCode": exitCode,
				"Output": base64.StdEncoding.EncodeToString([]byte(output)), "ErrorInfo": "",
			}}}}}
		}
		return 404, map[string]string{"Code": "InvalidAction.NotFound"}
	})
}

var assistantConnection = provider.SSHConnection{ID: provider.ConnectionCloudAssistant, Type: provider.ConnectionCloudAssistant, InstallSupported: true}

func assistantRequest() provider.InstallRequest {
	return provider.InstallRequest{
		Instance:  provider.Instance{ExternalID: "i-private", Region: "cn-shanghai", PrivateIP: "172.16.0.2", Kind: "vm"},
		ServerURL: "http://172.16.0.9:8800", Token: "node-token", ConnectMode: "agent_dial",
		Connection: assistantConnection,
	}
}

func TestCloudAssistantInstallRunsTheScriptAndReportsIt(t *testing.T) {
	d, fake := assistantFake(t, true, "Success", 0, "gpuworks: installing agent from http://172.16.0.9:8800 (arch amd64)\ngpuworks: agent started via systemd.\n")
	report, err := d.InstallAgentWithReport(context.Background(), userCreds, assistantRequest())
	if err != nil {
		t.Fatal(err)
	}
	var run fakeCall
	for _, c := range fake.calls {
		if c.action == "RunCommand" {
			run = c
		}
	}
	want := "exec 2>&1\n" + provision.ScriptInstallSteps("http://172.16.0.9:8800", "node-token", "agent_dial", "", provision.ScriptOptions{})
	if run.query["CommandContent"] != want || run.query["Type"] != "RunShellScript" || run.query["InstanceId.1"] != "i-private" || run.query["Timeout"] != "600" || run.query["RegionId"] != "cn-shanghai" {
		t.Fatalf("RunCommand = %+v", run.query)
	}
	if len(report.Checks) != 2 || report.Checks[1].Name != "Agent process" || report.Checks[1].Status != "ok" || !strings.Contains(report.Checks[1].Detail, "agent started via systemd") {
		t.Fatalf("report = %+v", report)
	}
}

// Cloud Assistant hands the operator's directory and supervisor to install.sh
// and reports the directory, which the server records for recovery.
func TestCloudAssistantInstallCarriesTheDirectoryAndSupervisor(t *testing.T) {
	d, fake := assistantFake(t, true, "Success", 0, "gpuworks: agent started via nohup pid 42\n")
	req := assistantRequest()
	req.InstallDir, req.NoSystemd = "/data/gpuworks", true
	report, err := d.InstallAgentWithReport(context.Background(), userCreds, req)
	if err != nil {
		t.Fatal(err)
	}
	var run fakeCall
	for _, c := range fake.calls {
		if c.action == "RunCommand" {
			run = c
		}
	}
	if !strings.HasSuffix(run.query["CommandContent"], "--install-dir '/data/gpuworks' --no-systemd") {
		t.Fatalf("CommandContent = %s", run.query["CommandContent"])
	}
	if report.InstallDir != "/data/gpuworks" {
		t.Fatalf("report install dir = %q", report.InstallDir)
	}
}

func TestCloudAssistantInstallFailureCarriesTheScriptOutput(t *testing.T) {
	d, _ := assistantFake(t, true, "Failed", 22, "curl: (22) The requested URL returned error: 502\n")
	_, err := d.InstallAgentWithReport(context.Background(), userCreds, assistantRequest())
	if err == nil || !strings.Contains(err.Error(), "ended Failed with exit code 22") || !strings.Contains(err.Error(), "returned error: 502") || provider.IsRetryableSSHConnectionError(err) {
		t.Fatalf("err = %v", err)
	}
}

// Offline Cloud Assistant is a connection failure, so auto selection moves on
// to SSH; nothing is run.
func TestOfflineCloudAssistantIsARetryableConnectionError(t *testing.T) {
	d, fake := assistantFake(t, false, "Success", 0, "")
	_, err := d.InstallAgentWithReport(context.Background(), userCreds, assistantRequest())
	if !provider.IsRetryableSSHConnectionError(err) || !strings.Contains(err.Error(), "not online") {
		t.Fatalf("err = %v", err)
	}
	if got := strings.Join(fake.actions(), ","); got != "DescribeCloudAssistantStatus" {
		t.Fatalf("calls = %s", got)
	}
}

// The server dials this agent, so the instance cannot download it from the
// server: Cloud Assistant runs nothing, and auto selection moves on to SSH,
// which uploads the agent (the failure behind "Could not resolve host").
func TestCloudAssistantLeavesAServerDialInstallToSSH(t *testing.T) {
	d, fake := assistantFake(t, true, "Success", 0, "")
	req := assistantRequest()
	req.ServerURL, req.ConnectMode, req.ServerDialListen = "", "server_dial", "0.0.0.0:18800"
	_, err := d.InstallAgentWithReport(context.Background(), userCreds, req)
	if !provider.IsRetryableSSHConnectionError(err) || !strings.Contains(err.Error(), "install over SSH") {
		t.Fatalf("err = %v", err)
	}
	if calls := fake.actions(); len(calls) != 0 {
		t.Fatalf("Cloud Assistant was asked to run something: %v", calls)
	}
}

func TestSSHFallbackNeedsAPublicAddress(t *testing.T) {
	d := New()
	req := assistantRequest()
	req.Connection = provider.SSHConnection{ID: "direct", Type: "direct"}
	if _, err := d.InstallAgentWithReport(context.Background(), userCreds, req); err == nil || !strings.Contains(err.Error(), "neither Cloud Assistant online nor a public address") {
		t.Fatalf("err = %v", err)
	}
}

func TestPrecheckThroughCloudAssistantParsesRows(t *testing.T) {
	output := "OS/Arch\tok\tLinux x86_64 6.8.0\nGPU\twarn\tno NVIDIA driver answered\nDocker\tok\tnot installed\nServer reachability\terror\tcurl: (28) Connection timed out\nnoise line\n"
	d, fake := assistantFake(t, true, "Success", 0, output)
	checks, err := d.PrecheckCommand(context.Background(), userCreds, assistantRequest().Instance, assistantConnection, provider.CommandPrecheckRequest{ServerURL: "http://172.16.0.9:8800", ConnectMode: "agent_dial"})
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, c := range checks {
		got = append(got, c.Name+"="+c.Status)
	}
	if strings.Join(got, ",") != "Cloud Assistant=ok,OS/Arch=ok,GPU=warn,Docker=ok,Server reachability=error" {
		t.Fatalf("checks = %v", checks)
	}
	var script string
	for _, c := range fake.calls {
		if c.action == "RunCommand" {
			script = c.query["CommandContent"]
		}
	}
	if !strings.Contains(script, `'http://172.16.0.9:8800/healthz'`) {
		t.Fatalf("precheck script does not probe the server:\n%s", script)
	}
	// server_dial has no agent → server step to check.
	if s := precheckScript(provider.CommandPrecheckRequest{ServerURL: "http://x", ConnectMode: "server_dial"}); strings.Contains(s, "healthz") {
		t.Fatalf("server_dial precheck probes the server:\n%s", s)
	}
}

func TestPrecheckWithCloudAssistantOfflineSaysSo(t *testing.T) {
	d, _ := assistantFake(t, false, "Success", 0, "")
	checks, err := d.PrecheckCommand(context.Background(), userCreds, assistantRequest().Instance, assistantConnection, provider.CommandPrecheckRequest{})
	if err != nil || len(checks) != 1 || checks[0].Status != "error" || !strings.Contains(checks[0].Detail, "not online") {
		t.Fatalf("checks = %+v, err = %v", checks, err)
	}
}

// ECS holds no platform key and issues no password: before logging in over
// SSH, Cloud Assistant adds the platform key to root's authorized_keys, so a
// server-dialed install needs no credential from the operator.
func TestSSHInstallAuthorizesThePlatformKeyThroughCloudAssistant(t *testing.T) {
	d, fake := assistantFake(t, true, "Success", 0, "added the platform key to /root/.ssh/authorized_keys through Cloud Assistant\n")
	req := assistantRequest()
	req.ServerURL, req.ConnectMode, req.ServerDialListen = "", "server_dial", "0.0.0.0:18800"
	req.Connection = provider.SSHConnection{ID: "direct", Type: "direct", Host: "127.0.0.1", Port: 1, User: "root", InstallSupported: true}
	req.Instance.PublicIP, req.Instance.SSHPort, req.Instance.SSHUser = "127.0.0.1", 1, "root"
	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	req.PlatformKey = &provider.PlatformKey{PublicKey: "ssh-ed25519 AAAAplatform gpuworks@server", PrivatePEM: string(pem.EncodeToMemory(block))}
	req.ReadAgentBinary = func(string) ([]byte, error) { return []byte("agent"), nil }
	_, err = d.InstallAgentWithReport(context.Background(), userCreds, req)
	if !provider.IsRetryableSSHConnectionError(err) {
		t.Fatalf("err = %v, want the SSH connection to the closed port to fail", err)
	}
	var run fakeCall
	for _, c := range fake.calls {
		if c.action == "RunCommand" {
			run = c
		}
	}
	if !strings.Contains(run.query["CommandContent"], "'ssh-ed25519 AAAAplatform gpuworks@server'") || run.query["InstanceId.1"] != "i-private" {
		t.Fatalf("Cloud Assistant did not add the platform key before SSH: %+v (calls %v)", run.query, fake.actions())
	}
}

// The key script appends the key once, with the modes sshd requires.
func TestAuthorizeKeyScriptAddsTheKeyOnce(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "getent"), []byte("#!/bin/sh\necho \"root:x:0:0:root:$FAKE_HOME:/bin/sh\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	key := "ssh-ed25519 AAAAplatform gpuworks@server"
	for i := 0; i < 2; i++ {
		cmd := exec.Command(sh, "-c", authorizeKeyScript(key))
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_HOME=" + home}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("run %d: %v\n%s", i, err, out)
		}
	}
	data, err := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
	if err != nil || string(data) != key+"\n" {
		t.Fatalf("authorized_keys = %q, %v", data, err)
	}
	for path, want := range map[string]os.FileMode{filepath.Join(home, ".ssh"): 0o700, filepath.Join(home, ".ssh", "authorized_keys"): 0o600} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s mode = %v, %v", path, info.Mode().Perm(), err)
		}
	}
}

// The precheck learns that the install adds the key through Cloud Assistant,
// waiting for it when it is not online yet; asking runs nothing.
func TestPlatformKeyAtInstallSaysTheInstallWaitsForCloudAssistant(t *testing.T) {
	for _, online := range []bool{true, false} {
		d, fake := assistantFake(t, online, "Success", 0, "")
		check, ok := d.PlatformKeyAtInstall(context.Background(), userCreds, assistantRequest().Instance, "ssh-ed25519 AAAAplatform")
		if !ok || check.Status != "warn" || !strings.Contains(check.Detail, "Cloud Assistant") {
			t.Fatalf("online=%v: ok=%v check=%+v", online, ok, check)
		}
		if waits := strings.Contains(check.Detail, "waits up to 2m0s"); waits == online {
			t.Fatalf("online=%v: detail = %q", online, check.Detail)
		}
		if got := strings.Join(fake.actions(), ","); got != "DescribeCloudAssistantStatus" {
			t.Fatalf("actions = %s", got)
		}
	}
}

// On an instance started a moment ago Cloud Assistant comes online about a
// minute after SSH: the install waits for it (T81), then adds the key; one
// whose Cloud Assistant never comes online costs the bounded wait.
func TestPlatformKeyWaitsForCloudAssistantToComeOnline(t *testing.T) {
	statuses := 0
	d, fake := newFake(t, func(c fakeCall) (int, any) {
		switch c.action {
		case "DescribeCloudAssistantStatus":
			statuses++
			return 200, map[string]any{"InstanceCloudAssistantStatusSet": map[string]any{"InstanceCloudAssistantStatus": []any{
				map[string]any{"InstanceId": c.query["InstanceId.1"], "CloudAssistantStatus": fmt.Sprint(statuses >= 3)},
			}}}
		case "RunCommand":
			return 200, map[string]string{"CommandId": "c-1", "InvokeId": "t-1"}
		case "DescribeInvocationResults":
			return 200, map[string]any{"Invocation": map[string]any{"InvocationResults": map[string]any{"InvocationResult": []any{map[string]any{
				"InvokeId": "t-1", "InstanceId": c.query["InstanceId"], "InvocationStatus": "Success", "ExitCode": 0,
				"Output": base64.StdEncoding.EncodeToString([]byte("added the platform key\n")),
			}}}}}
		}
		return 404, map[string]string{"Code": "InvalidAction.NotFound"}
	})
	d.assistantWait, d.assistantPoll = time.Second, time.Millisecond
	req := assistantRequest()
	req.PlatformKey = &provider.PlatformKey{PublicKey: "ssh-ed25519 AAAAplatform gpuworks@server"}
	check := d.authorizePlatformKey(context.Background(), userCreds, req)
	if check.Status != "ok" || !strings.Contains(check.Detail, "waited") || statuses != 3 {
		t.Fatalf("check = %+v after %d status calls (%v)", check, statuses, fake.actions())
	}

	never, _ := assistantFake(t, false, "Success", 0, "")
	never.assistantWait, never.assistantPoll = 30*time.Millisecond, 5*time.Millisecond
	start := time.Now()
	check = never.authorizePlatformKey(context.Background(), userCreds, req)
	if check.Status != "warn" || !strings.Contains(check.Detail, "did not come online") || time.Since(start) < 30*time.Millisecond {
		t.Fatalf("check = %+v after %s", check, time.Since(start))
	}
}
