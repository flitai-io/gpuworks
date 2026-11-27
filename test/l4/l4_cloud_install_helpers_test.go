//go:build cloudgpu

package l4

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/agentbin"
	"gpuworks.ai/gpuworks/internal/server/messages"
	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
	"gpuworks.ai/gpuworks/internal/testenv/sshutil"
)

func verifyCloudServerForward(ctx context.Context, instance *cloudprovider.Instance, serverURL string) error {
	command := fmt.Sprintf("set -eu; export NO_PROXY=127.0.0.1,localhost; export no_proxy=127.0.0.1,localhost; curl -fsS %s/healthz >/dev/null; curl -fsS %s/install.sh | head -n 1 | grep -F '#!/bin/sh' >/dev/null", shellQuoteL4(serverURL), shellQuoteL4(serverURL))
	var lastOutput string
	var lastErr error
	for attempt := 1; attempt <= 4; attempt++ {
		lastOutput, lastErr = runCloudCommand(ctx, instance, command)
		if lastErr == nil {
			return nil
		}
		if attempt == 4 {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("remote health/install script request canceled after attempt %d: %w (last SSH error: %v): %s", attempt, ctx.Err(), lastErr, strings.TrimSpace(lastOutput))
		case <-time.After(time.Duration(attempt) * 5 * time.Second):
		}
	}
	return fmt.Errorf("remote health/install script request failed after 4 attempts: %w: %s", lastErr, strings.TrimSpace(lastOutput))
}

func hasProviderCredentials(p cloudprovider.ManagementProvider) bool {
	creds := p.ManagementCredentials()
	return strings.TrimSpace(creds.APIKey) != "" || strings.TrimSpace(creds.PublicKey) != "" || strings.TrimSpace(creds.PrivateKey) != ""
}

// providerInstallReport carries what a provider install says beyond the node
// it created.
type providerInstallReport struct {
	// QueuedDial is the detail of a server_dial check that came back "queued
	// for reconnection": the install's own dial did not get through and the
	// server's reconnect loop took the node over. Empty otherwise. The node is
	// not known to work until it comes online, which waitQueuedDialNodeOnline
	// checks.
	QueuedDial string
}

// serverDialQueuedOnlineTimeout bounds how long a node whose first dial was
// queued gets to come online through the server's redial loop (10s, 30s, 90s
// and 270s backoff steps after the install returns).
const serverDialQueuedOnlineTimeout = 5 * time.Minute

// providerInstallVerdict is how one provider install check is judged.
type providerInstallVerdict struct {
	Accepted bool
	// Want describes the acceptable status for failure messages.
	Want string
	// QueuedDial marks an accepted server_dial warn that still has to be
	// followed by the node coming online.
	QueuedDial bool
}

// judgeProviderInstallCheck decides whether the install check at a position
// that should be the check named want is acceptable. Every check must be ok,
// except three advisory cases: prechecks may warn (a VM without Docker is
// valid when the agent picks udocker), agent_dial may report that the agent
// is still waiting for its first session, and server_dial may report that the
// server queued the node for reconnection after its first dial failed (a
// freshly mapped pod port can take a while to answer). The last two are only
// accepted with their exact codes; any other warn or fail on them, and any
// status other than pass elsewhere, fails.
func judgeProviderInstallCheck(want string, check provision.CheckResult) providerInstallVerdict {
	verdict := providerInstallVerdict{Want: proto.CheckPass}
	switch {
	case want == provision.CheckIDPrecheck:
		verdict.Want = "pass/warn"
		verdict.Accepted = check.Status == proto.CheckPass || check.Status == proto.CheckWarn
	case want == provision.CheckIDAgentDial && check.Status == proto.CheckWarn && check.Code == messages.CheckAgentSessionWaiting:
		// The management API observes the first session for three seconds.
		// A cold VM may need longer; setupCloudNode still waits for this exact
		// node to become online, and the suite subsequently requires GPUs.
		verdict.Want = "pass/warn(" + messages.CheckAgentSessionWaiting + ")"
		verdict.Accepted = true
	case want == provision.CheckIDServerDial && check.Status == proto.CheckWarn && check.Code == messages.CheckServerDialNotConnected:
		// The install tries the first dial a few times and otherwise hands
		// the node to the server's reconnect loop (internal/server/api/provision.go).
		verdict.Want = "pass/warn(" + messages.CheckServerDialNotConnected + ")"
		verdict.Accepted = true
		verdict.QueuedDial = true
	default:
		verdict.Accepted = check.Status == proto.CheckPass
	}
	if check.ID != want {
		verdict.Accepted = false
		verdict.QueuedDial = false
	}
	return verdict
}

// waitQueuedDialNodeOnline waits, up to timeout, for a node whose install
// queued its first server dial to come online through the server's redial
// loop. nodeState reads the node's current state; probe makes one direct
// attempt at the node's agent address and describes the outcome. When the node
// never comes online the error carries the install's dial error, the node's
// last state and the probe, which is the dial error the server keeps hitting.
func waitQueuedDialNodeOnline(ctx context.Context, timeout, poll time.Duration, nodeState func() (string, error), probe func() string, queued string) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	var lastState string
	var lastStateErr error
	for {
		lastState, lastStateErr = nodeState()
		if lastStateErr == nil && lastState == store.NodeStateOnline {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for the server to redial a queued server_dial node: %w (node state %q, first dial %s)", ctx.Err(), lastState, queued)
		case <-deadline.C:
			detail := fmt.Sprintf("node state %q", lastState)
			if lastStateErr != nil {
				detail = fmt.Sprintf("node state unreadable: %v", lastStateErr)
			}
			return fmt.Errorf("server_dial node did not come online within %s after its first dial was queued for reconnection; %s; install reported %q; direct probe now: %s",
				timeout, detail, strings.TrimSpace(queued), probe())
		case <-ticker.C:
		}
	}
}

// probeServerDialAddress makes one TCP connection attempt to addr and
// describes the result.
func probeServerDialAddress(addr string) string {
	if addr == "" {
		return "no server_dial address recorded"
	}
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fmt.Sprintf("dial %s: %v", addr, err)
	}
	_ = conn.Close()
	return fmt.Sprintf("dial %s: connected (the agent accepts TCP; the handshake or token is what fails)", addr)
}

// installCloudAgentViaProviderAPI adds the provider, syncs it and installs
// the agent on instance through the provider API: dialing serverURL, or,
// with serverURL empty, dialed by the server at the port the driver chooses
// from the instance's mappings. report receives what the install said beyond
// the node UUID.
func installCloudAgentViaProviderAPI(ctx context.Context, t *testing.T, env *testenv.Env, p cloudprovider.CloudProvider, instance *cloudprovider.Instance, serverURL string, report *providerInstallReport) (string, error) {
	t.Helper()
	serverDial := serverURL == ""
	var expectedVastSSH *serverprovider.SSHConnection
	management := p.(cloudprovider.ManagementProvider)
	creds := management.ManagementCredentials()
	api := env.API(t)
	addBody, err := json.Marshal(map[string]any{
		"kind": p.Name(), "name": cloudprovider.TestNamePrefix + p.Name(), "endpoint": creds.Endpoint,
		"api_key": creds.APIKey, "public_key": creds.PublicKey, "private_key": creds.PrivateKey, "extra": creds.Extra,
	})
	if err != nil {
		return "", err
	}
	resp, err := api.PostJSON("/api/v1/providers", addBody)
	if err != nil {
		return "", err
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("add %s provider status=%d body=%s", p.Name(), resp.StatusCode, data)
	}
	var added struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(data, &added); err != nil || added.ID == 0 {
		return "", fmt.Errorf("decode added %s provider: id=%d err=%v", p.Name(), added.ID, err)
	}
	resp, err = api.PostJSON(fmt.Sprintf("/api/v1/providers/%d/sync", added.ID), []byte(`{}`))
	if err != nil {
		return "", err
	}
	data, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("sync %s provider status=%d body=%s", p.Name(), resp.StatusCode, data)
	}
	if p.Name() == "vast" {
		var synced struct {
			Instances []serverprovider.Instance `json:"instances"`
		}
		if err := json.Unmarshal(data, &synced); err != nil {
			return "", fmt.Errorf("decode synced Vast instances: %w", err)
		}
		var found *serverprovider.Instance
		for index := range synced.Instances {
			if synced.Instances[index].ExternalID == instance.InstanceID {
				found = &synced.Instances[index]
				break
			}
		}
		if found == nil {
			return "", fmt.Errorf("synced Vast instance %s was not found", instance.InstanceID)
		}
		candidates, err := serverprovider.SSHInstallCandidates(*found, "auto")
		if err != nil || len(candidates) == 0 {
			return "", fmt.Errorf("synced Vast instance %s has no installable SSH connection: connections=%+v err=%v", instance.InstanceID, found.SSHConnections, err)
		}
		expected := candidates[0]
		if expected.Host != instance.PublicIP || expected.Port != instance.SSHPort {
			return "", fmt.Errorf("synced Vast preferred SSH connection=%+v, rented endpoint=%s:%d", expected, instance.PublicIP, instance.SSHPort)
		}
		expectedVastSSH = &expected
	}
	install := map[string]any{
		"external_id": instance.InstanceID, "server_url": serverURL,
		"ssh_key_pem": string(instance.PrivateKey), "password": instance.Password,
		"ssh_connection_id": "auto", "connect_mode": "agent_dial",
	}
	if serverDial {
		// Neither the listen port nor the address: the driver picks a mapped
		// port nothing on the instance holds.
		delete(install, "server_url")
		install["connect_mode"] = "server_dial"
		if p.Name() == "aliyun" {
			// The product dials an ECS instance's private address unless told
			// otherwise (the server is expected inside the VPC). This server runs
			// outside it, so the operator gives the public one.
			install["host_address"] = instance.PublicIP
		}
	}
	installBody, err := json.Marshal(install)
	if err != nil {
		return "", err
	}
	resp, err = api.PostJSON(fmt.Sprintf("/api/v1/providers/%d/install", added.ID), installBody)
	if err != nil {
		return "", err
	}
	data, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("provider install status=%d body=%s", resp.StatusCode, data)
	}
	var result struct {
		NodeUUID      string                        `json:"node_uuid"`
		ServerURL     string                        `json:"server_url"`
		SSHConnection *serverprovider.SSHConnection `json:"ssh_connection"`
		SSHAttempts   []serverprovider.SSHAttempt   `json:"ssh_attempts"`
		Checks        []provision.CheckResult       `json:"checks"`
		Details       []provision.CheckResult       `json:"install_details"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", err
	}
	if strings.TrimRight(result.ServerURL, "/") != strings.TrimRight(serverURL, "/") {
		return "", fmt.Errorf("provider install server_url=%q, want %q", result.ServerURL, serverURL)
	}
	if result.NodeUUID == "" {
		return "", fmt.Errorf("provider install returned an empty node_uuid")
	}
	if p.Name() == "vast" {
		if result.SSHConnection == nil || expectedVastSSH == nil || result.SSHConnection.ID != expectedVastSSH.ID || result.SSHConnection.Type != expectedVastSSH.Type || result.SSHConnection.Host != expectedVastSSH.Host || result.SSHConnection.Port != expectedVastSSH.Port {
			return "", fmt.Errorf("Vast auto install selected SSH connection=%+v, want preferred %+v", result.SSHConnection, expectedVastSSH)
		}
		if len(result.SSHAttempts) != 1 || result.SSHAttempts[0].ConnectionID != result.SSHConnection.ID || result.SSHAttempts[0].Status != "selected" {
			return "", fmt.Errorf("Vast auto install SSH attempts=%+v", result.SSHAttempts)
		}
	}
	wantChecks := []string{provision.CheckIDPrecheck, provision.CheckIDInstallAgent, provision.CheckIDAgentDial}
	if serverDial {
		wantChecks[2] = provision.CheckIDServerDial
	}
	if len(result.Checks) != len(wantChecks) {
		return "", fmt.Errorf("provider install checks=%+v, want %v", result.Checks, wantChecks)
	}
	var checkErr error
	for i, want := range wantChecks {
		t.Logf("provider install check: %s=%s %s %s", result.Checks[i].ID, result.Checks[i].Status, result.Checks[i].Code, checkText(result.Checks[i]))
		verdict := judgeProviderInstallCheck(want, result.Checks[i])
		if verdict.QueuedDial && report != nil {
			report.QueuedDial = checkText(result.Checks[i])
		}
		if checkErr == nil && !verdict.Accepted {
			checkErr = fmt.Errorf("provider install check %d=%+v, want %s=%s", i, result.Checks[i], want, verdict.Want)
		}
	}
	if checkErr != nil {
		diagnostic, diagnosticErr := runCloudCommand(ctx, instance, `set +e
printf 'PATH=%s\n' "$PATH"
for executable in python3 python; do
  printf '%s=' "$executable"
  command -v "$executable" 2>&1 || true
  "$executable" --version 2>&1 || true
done
ls -l /usr/bin/python* /usr/local/bin/python* /root/miniconda3/bin/python* 2>&1 | head -40`)
		return "", fmt.Errorf("%w; ssh Python diagnostic (err=%v): %s", checkErr, diagnosticErr, strings.TrimSpace(diagnostic))
	}
	// The instance must now accept the server's platform key, however it got
	// there (provider API, or written after a password / harness-key login):
	// out-of-band recovery relies on nothing else.
	var platformKey *provision.CheckResult
	assistantInstall := false
	for i := range result.Details {
		t.Logf("provider install detail: %s=%s %s %s", result.Details[i].ID, result.Details[i].Status, result.Details[i].Code, checkText(result.Details[i]))
		if result.Details[i].ID == provision.CheckIDPlatformKey {
			platformKey = &result.Details[i]
		}
		if result.Details[i].ID == provision.CheckIDCloudAssistant && result.Details[i].Status == proto.CheckPass {
			assistantInstall = true
		}
	}
	// An install Cloud Assistant ran as root on the instance never opens an SSH
	// session, so no platform key is added; the instance stays manageable
	// through Cloud Assistant, which is how that connection recovers it.
	if platformKey == nil && assistantInstall {
		t.Log("agent installed through Cloud Assistant: no SSH platform key was added, by design")
	} else if platformKey == nil || platformKey.Status != proto.CheckPass {
		return "", fmt.Errorf("provider install did not authorize the platform key: %+v", platformKey)
	}
	if serverDial {
		if err := assertCloudAgentServerDialed(ctx, env, instance, result.NodeUUID); err != nil {
			return "", err
		}
		return result.NodeUUID, nil
	}
	if err := assertCloudAgentServerURL(ctx, instance, serverURL); err != nil {
		return "", err
	}
	return result.NodeUUID, nil
}

// assertCloudAgentServerDialed checks a server-dialed agent: the node
// records the container port it listens on and the public address mapped to
// it, and the agent on the instance runs as `run --state` from a
// configuration that names no server and listens on that port.
func assertCloudAgentServerDialed(ctx context.Context, env *testenv.Env, instance *cloudprovider.Instance, nodeUUID string) error {
	node, err := env.Store().NodeByUUID(nodeUUID)
	if err != nil {
		return err
	}
	if node.ConnectMode != "server_dial" || node.ServerURL != "" || node.ServerDialListen == "" || node.ServerDialAddress == "" {
		return fmt.Errorf("server-dialed node mode=%q server_url=%q listen=%q address=%q", node.ConnectMode, node.ServerURL, node.ServerDialListen, node.ServerDialAddress)
	}
	command := "set -u\n" + agentconf.ShellFuncs + `
line=$({ ps axww -o args= 2>/dev/null || ps axww; } | grep -F -- 'gpuworks-agent run --state' | grep -v grep | head -n 1)
[ -n "$line" ] || { echo "no agent runs as run --state"; exit 1; }
printf 'mode=%s server=%s listen=%s\n' "$(gw_line_value "$line" CONNECT_MODE)" "$(gw_line_value "$line" SERVER)" "$(gw_line_value "$line" LISTEN)"`
	out, err := runCloudCommand(ctx, instance, command)
	if err != nil {
		return fmt.Errorf("read the server-dialed agent's configuration: %w: %s", err, out)
	}
	if want := "mode=server_dial server= listen=" + node.ServerDialListen; strings.TrimSpace(out) != want {
		return fmt.Errorf("server-dialed agent configuration = %q, want %q", strings.TrimSpace(out), want)
	}
	return nil
}

func topologyCloudTest(t *testing.T) bool {
	name := t.Name()
	return strings.Contains(name, "HAMi") || strings.Contains(name, "MultiNode") || strings.Contains(name, "GPUModelMatrix") || strings.Contains(name, "CompshareMultiGPU") || strings.Contains(name, "CompshareSuite")
}

func isCloudCapacityError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"no longer any instances available",
		"no preferred gpu candidate available",
		"requested capacity unavailable",
		// Aliyun: no spot or on-demand placement had stock (adapter message).
		"no aliyun capacity",
		// Gitee lists a model in /compute/specs but still rejects the create
		// with this message when every matching host is rented out.
		"没有找到符合条件的可用主机",
		"wait for runpod ssh endpoint",
		"wait for compshare ssh endpoint",
		"wait for ssh readiness",
		"claim install dir via sudo: eof",
		"ssh handshake: eof",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func waitCloudSSH(ctx context.Context, instance *cloudprovider.Instance) error {
	if instance.Password != "" {
		return sshutil.WaitSSHReadyPasswordAs(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.Password)
	}
	return sshutil.WaitSSHReadyAs(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.PrivateKey)
}

func openServerDialTunnel(ctx context.Context, instance *cloudprovider.Instance) (*sshutil.ServerDialTunnel, error) {
	const remoteAddr = "127.0.0.1:19090"
	if instance.Password != "" {
		return sshutil.NewServerDialTunnelPassword(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.Password, remoteAddr)
	}
	return sshutil.NewServerDialTunnel(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.PrivateKey, remoteAddr)
}

func openServerForward(ctx context.Context, instance *cloudprovider.Instance, localAddr string) (*sshutil.RemoteForward, error) {
	if instance.Password != "" {
		return sshutil.ForwardServerPassword(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.Password, localAddr)
	}
	return sshutil.ForwardServer(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.PrivateKey, localAddr)
}

func installCloudAgent(ctx context.Context, dataDir string, instance *cloudprovider.Instance, serverAddr, token string) error {
	agents, err := agentbin.Prepare(dataDir)
	if err != nil {
		return err
	}
	installer := provision.NewSSHInstaller(func(name string) ([]byte, error) {
		return agents.Read("", name)
	})
	req := provision.SSHInstallRequest{
		Target: instance.PublicIP, User: instance.Username, Port: instance.SSHPort,
		ServerURL: serverAddr, Token: token, ConnectMode: "server_dial",
		ServerDialListen: "127.0.0.1:19090",
		InstallDir:       instance.DataDiskPath,
		SSHAuth:          provision.SSHAuth{Password: instance.Password, PrivateKeyPEM: string(instance.PrivateKey)},
	}
	_, err = installer.InstallSSH(ctx, req)
	if err != nil {
		return err
	}
	return assertCloudAgentServerURL(ctx, instance, serverAddr)
}

// installCloudAgentViaAPI exercises the same management-plane one-click
// installer used by the UI. Keeping this path in acceptance tests catches
// regressions in token creation, privilege fallback, server_dial setup and
// agent binary selection instead of bypassing them with a test-only token.
func installCloudAgentViaAPI(ctx context.Context, t *testing.T, env *testenv.Env, instance *cloudprovider.Instance, providerKind, name, serverURL, connectMode, listen, serverDialAddress string) error {
	body, err := json.Marshal(map[string]any{
		"name": name, "provider_kind": providerKind, "region": instance.Region, "target": instance.PublicIP, "user": instance.Username, "port": instance.SSHPort,
		"password": instance.Password, "private_key_pem": string(instance.PrivateKey),
		"connect_mode": connectMode, "server_dial_listen": listen, "server_dial_address": serverDialAddress, "server_url": serverURL,
	})
	if err != nil {
		return err
	}
	api := env.API(t)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, env.URL()+"/api/v1/nodes/install", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := api.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		diagnostic, diagnosticErr := runCloudCommand(ctx, instance, "set +e; printf 'id='; id -u; printf '\\nuname='; uname -a 2>&1; printf '\\ndpkg='; dpkg --print-architecture 2>&1; printf '\\narch='; arch 2>&1; printf '\\ncpu='; head -n 3 /proc/cpuinfo 2>&1")
		if diagnosticErr != nil {
			diagnostic += " (diagnostic error: " + diagnosticErr.Error() + ")"
		}
		return fmt.Errorf("one-click agent install status=%d body=%s; ssh diagnostic=%s", resp.StatusCode, data, strings.TrimSpace(diagnostic))
	}
	// An agent the server dials never contacts the server, so the installer
	// records no server_url for it whatever the caller sent (installServerURL
	// in internal/server/api); only a dial-out agent has one to verify.
	wantServerURL := serverURL
	if connectMode == "server_dial" {
		wantServerURL = ""
	}
	var report provision.SSHInstallReport
	if err := json.Unmarshal(data, &report); err == nil {
		if strings.TrimRight(report.ServerURL, "/") != strings.TrimRight(wantServerURL, "/") {
			return fmt.Errorf("installer report server_url=%q, want %q", report.ServerURL, wantServerURL)
		}
		for _, check := range report.Checks {
			t.Logf("one-click install check: %s=%s %s %s", check.ID, check.Status, check.Code, checkText(check))
		}
	}
	if connectMode == "server_dial" {
		return nil
	}
	return assertCloudAgentServerURL(ctx, instance, serverURL)
}

// assertCloudAgentServerURL checks that the running agent dials serverURL,
// as its configuration file (or an earlier install's arguments) says.
func assertCloudAgentServerURL(ctx context.Context, instance *cloudprovider.Instance, serverURL string) error {
	needle := strings.TrimRight(strings.TrimSpace(serverURL), "/")
	command := "set -u\nwant=" + shellQuoteL4(needle) + "\n" + agentconf.ShellFuncs + `
if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet gpuworks-agent 2>/dev/null; then
  srv=$(gw_unit_value SERVER)
  [ "${srv%/}" = "$want" ] && exit 0
fi
{ ps axww -o args= 2>/dev/null || ps axww; } | grep -F -- gpuworks-agent | grep -v grep | while IFS= read -r line; do
  srv=$(gw_line_value "$line" SERVER)
  [ "${srv%/}" = "$want" ] && echo found
done | grep -q found`
	if _, err := runCloudCommand(ctx, instance, command); err != nil {
		return fmt.Errorf("the running agent does not dial server_url %q: %w", needle, err)
	}
	return nil
}

func shellQuoteL4(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func dialCloudAgent(ctx context.Context, env *testenv.Env, addr, token string) error {
	var lastErr error
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		lastErr = env.DialServerAgent(attemptCtx, addr, token)
		cancel()
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("dial cloud agent through SSH: %w (last error: %v)", ctx.Err(), lastErr)
		case <-time.After(3 * time.Second):
		}
	}
}
