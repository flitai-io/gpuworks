package zstackcloud_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provision"
)

// The install prechecks the host it is about to install over SSH, with the
// operator's credential and the server URL the agent will use; the precheck
// is advisory, so an unreachable host still gets its install attempt and the
// response says the precheck failed.
func TestZStackCloudInstallPrechecksTheHostItInstalls(t *testing.T) {
	for _, tc := range []struct {
		name        string
		unreachable bool
		want        string
	}{
		// The Cloud fixture refuses the platform-key authorization (HTTP 503),
		// which the precheck reports as a warning next to its own rows.
		{"reachable", false, "warn"},
		{"unreachable", true, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloud := cloudInventoryFixture(t)
			precheck := &sshPrecheckFixture{unreachable: tc.unreachable}
			var installs atomic.Int32
			env := newCloudEnvPrechecking(t, installerFunc(func(context.Context, provision.SSHInstallRequest) (provision.SSHInstallReport, error) {
				installs.Add(1)
				return provision.SSHInstallReport{Checks: []provision.CheckResult{{Name: "Agent process", Status: "ok"}}}, nil
			}), precheck)
			path := addedProvider(t, env, cloud.URL)
			requestJSON(t, env, "POST", path+"/sync", `{}`, 200)
			raw := requestJSON(t, env, "POST", path+"/install", fmt.Sprintf(`{"external_id":"host-a","password":"secret","server_url":%q}`, env.URL()), 200)

			requests := precheck.Requests()
			if len(requests) != 1 {
				t.Fatalf("precheck requests = %d, want one", len(requests))
			}
			req := requests[0]
			if req.Target != "192.0.2.10:22" || req.User != "root" || req.Password != "secret" || req.ServerURL != env.URL() || req.ProviderKind != "zstack" {
				t.Fatalf("precheck request = target %q user %q password set %v server %q provider %q", req.Target, req.User, req.Password != "", req.ServerURL, req.ProviderKind)
			}
			if installs.Load() != 1 {
				t.Fatalf("installs = %d, want one whatever the precheck found", installs.Load())
			}
			var result struct {
				Checks          []provision.CheckResult `json:"checks"`
				PrecheckDetails []provision.CheckResult `json:"precheck_details"`
			}
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
			var row *provision.CheckResult
			for i := range result.Checks {
				if result.Checks[i].Name == "precheck" {
					row = &result.Checks[i]
				}
			}
			if row == nil || row.Status != tc.want {
				t.Fatalf("precheck row = %+v, want status %s in %s", row, tc.want, raw)
			}
			if tc.unreachable && !strings.Contains(row.Detail, "connection refused") {
				t.Fatalf("precheck detail %q does not say why", row.Detail)
			}
			authenticated := false
			for _, check := range result.PrecheckDetails {
				authenticated = authenticated || (check.Name == "SSH authentication" && check.Status == "ok")
			}
			if authenticated == tc.unreachable {
				t.Fatalf("precheck details %+v: the SSH row must come from the precheck exactly when the host answered", result.PrecheckDetails)
			}
		})
	}
}
