package aliyun

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

const (
	// installTimeout bounds the install script on the instance: it downloads
	// the agent (~40 MB) from the server, which can be slow over a tunnel.
	installTimeout = 10 * time.Minute
	// precheckTimeout bounds the read-only precheck script.
	precheckTimeout = 60 * time.Second
	pollInterval    = 2 * time.Second
	// outputTail is what of a script's output a report keeps.
	outputTail = 2000
)

// InstallAgentWithReport installs the agent through the chosen connection:
// Cloud Assistant runs the server's install script on the instance (the
// instance downloads the agent from the server), SSH uploads it.
func (d *Driver) InstallAgentWithReport(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) (provider.InstallReport, error) {
	if req.Connection.Type == provider.ConnectionCloudAssistant {
		return d.installByAssistant(ctx, creds, req)
	}
	return d.installBySSH(ctx, creds, req)
}

// errAssistantCannotServeServerDial is why Cloud Assistant does not install
// an agent the server dials: it can only run commands, so the instance would
// have to download the agent from the server, which such an instance cannot
// reach. Auto selection moves on to SSH, which uploads the agent.
var errAssistantCannotServeServerDial = errors.New("Cloud Assistant can only make the instance download the agent from the server, which an instance the server dials cannot reach; install over SSH (give the instance's SSH key or password), or let the agent connect to the server")

func (d *Driver) installByAssistant(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) (provider.InstallReport, error) {
	if req.ConnectMode == "server_dial" {
		return provider.InstallReport{}, provider.RetryableSSHConnectionError("aliyun: Cloud Assistant", errAssistantCannotServeServerDial)
	}
	inst := req.Instance
	if err := d.requireAssistant(ctx, creds, inst); err != nil {
		// Offline Cloud Assistant is a connection failure: auto selection
		// moves on to SSH.
		return provider.InstallReport{}, provider.RetryableSSHConnectionError("aliyun: Cloud Assistant", err)
	}
	script := provision.ScriptInstallSteps(req.ServerURL, req.Token, req.ConnectMode, req.ServerDialListen, req.ScriptOptions())
	result, err := d.runCommand(ctx, creds, inst, "gpuworks-install-agent", "exec 2>&1\n"+script, installTimeout)
	if err != nil {
		return provider.InstallReport{}, fmt.Errorf("aliyun: Cloud Assistant: %w", err)
	}
	ran := provider.InstallCheck{Name: "Cloud Assistant", Status: "ok", Detail: fmt.Sprintf("ran the install script as root (invocation %s)", result.invokeID)}
	if !result.succeeded() {
		return provider.InstallReport{Checks: []provider.InstallCheck{ran}}, fmt.Errorf("aliyun: install script on %s %s: %s", inst.ExternalID, result.describe(), tail(result.output))
	}
	return provider.InstallReport{InstallDir: req.InstallDir, Checks: []provider.InstallCheck{
		ran,
		{Name: "Agent process", Status: "ok", Detail: tail(result.output)},
	}}, nil
}

// installBySSH installs over SSH with the provider-neutral installer: for
// instances whose Cloud Assistant is offline, and for agents the server
// dials, which Cloud Assistant cannot install (the instance would have to
// download from the server). ECS issues no password and holds no platform
// key, so while Cloud Assistant is online it first adds the platform key to
// root's authorized_keys; otherwise the credentials the operator gave are
// the only way in.
func (d *Driver) installBySSH(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) (provider.InstallReport, error) {
	inst := req.Instance
	if inst.PublicIP == "" || inst.SSHPort == 0 {
		return provider.InstallReport{}, fmt.Errorf("aliyun: instance %s has neither Cloud Assistant online nor a public address for SSH", inst.ExternalID)
	}
	if req.ReadAgentBinary == nil {
		return provider.InstallReport{}, errors.New("aliyun: agent binary reader is not configured")
	}
	keyCheck := d.authorizePlatformKey(ctx, creds, req)
	installer := provision.NewSSHInstaller(provision.AgentBinaryReader(req.ReadAgentBinary))
	report, err := installer.InstallSSH(ctx, provision.SSHInstallRequest{
		Target: inst.PublicIP, User: inst.SSHUser, Port: inst.SSHPort,
		ProviderKind: "aliyun", InstanceKind: req.InstanceKind,
		ServerURL: req.ServerURL, Token: req.Token,
		ConnectMode: req.ConnectMode, ServerDialListen: req.ServerDialListen, ServerDialAddress: req.ServerDialAddress,
		InstallDir: req.InstallDir, NoSystemd: req.NoSystemd,
		SSHAuth: req.SSHAuth(),
	})
	if err != nil {
		var connectionErr *provision.SSHConnectionError
		if errors.As(err, &connectionErr) {
			return provider.InstallReport{}, provider.RetryableSSHConnectionError("aliyun: SSH connection", connectionErr)
		}
		return provider.InstallReport{}, fmt.Errorf("aliyun: install over SSH on %s: %w", inst.PublicIP, err)
	}
	out := provider.ReportFromSSHInstall(report)
	if keyCheck.Name != "" {
		out.Checks = append([]provider.InstallCheck{keyCheck}, out.Checks...)
	}
	return out, nil
}

// authorizePlatformKey adds the platform public key to root's
// authorized_keys through Cloud Assistant, once (a line already there is
// left as it is). It never fails the install: without it the operator's own
// credentials may still log in, and the SSH error then says what to do.
func (d *Driver) authorizePlatformKey(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) provider.InstallCheck {
	check := provider.InstallCheck{Name: "Platform key (Cloud Assistant)"}
	if req.PlatformKey == nil || strings.TrimSpace(req.PlatformKey.PublicKey) == "" {
		return provider.InstallCheck{}
	}
	waited, err := d.waitForAssistant(ctx, creds, req.Instance)
	if err != nil {
		check.Status, check.Detail = "warn", "not added: "+err.Error()
		return check
	}
	result, err := d.runCommand(ctx, creds, req.Instance, "gpuworks-authorize-key", authorizeKeyScript(req.PlatformKey.PublicKey), authorizeKeyTimeout)
	switch {
	case err != nil:
		check.Status, check.Detail = "warn", "not added: "+err.Error()
	case !result.succeeded():
		check.Status, check.Detail = "warn", "not added: the command "+result.describe()+": "+tail(result.output)
	default:
		check.Status, check.Detail = "ok", strings.TrimSpace(result.output)
		if waited > 0 {
			check.Detail += fmt.Sprintf(" (waited %s for Cloud Assistant to come online)", waited.Round(time.Second))
		}
	}
	return check
}

// assistantWaitTimeout bounds the wait for Cloud Assistant: on a freshly
// started instance SSH answers about a minute before Cloud Assistant reports
// its first heartbeat (observed 2026-10-20), and without it the platform key
// cannot be added, so the first install failed every time (todo T81).
const assistantWaitTimeout = 2 * time.Minute

// waitForAssistant waits, up to assistantWaitTimeout, for Cloud Assistant on
// the instance to come online, and says how long it waited. An instance
// whose Cloud Assistant never comes online (a custom image without it) costs
// the whole wait before the install goes on without it.
func (d *Driver) waitForAssistant(ctx context.Context, creds provider.Credentials, inst provider.Instance) (time.Duration, error) {
	wait, poll := assistantWaitTimeout, 5*time.Second
	if d.assistantWait > 0 {
		wait = d.assistantWait
	}
	if d.assistantPoll > 0 {
		poll = d.assistantPoll
	}
	if inst.Region == "" {
		return 0, fmt.Errorf("instance %s has no region", inst.ExternalID)
	}
	start := time.Now()
	for {
		online, err := d.assistantOnline(ctx, creds, inst.Region, []string{inst.ExternalID})
		if err != nil {
			return time.Since(start), err
		}
		if online[inst.ExternalID] {
			return time.Since(start), nil
		}
		if time.Since(start) >= wait {
			return time.Since(start), fmt.Errorf("Cloud Assistant did not come online on %s within %s (custom images may lack it; a running instance reports a heartbeat every minute)", inst.ExternalID, wait)
		}
		select {
		case <-ctx.Done():
			return time.Since(start), ctx.Err()
		case <-time.After(poll):
		}
	}
}

// PlatformKeyAtInstall implements provider.InstallKeyAdder: the install
// adds the key through Cloud Assistant, waiting for it to come online first.
func (d *Driver) PlatformKeyAtInstall(ctx context.Context, creds provider.Credentials, inst provider.Instance, _ string) (provider.InstallCheck, bool) {
	if inst.Region == "" {
		return provider.InstallCheck{}, false
	}
	if d.requireAssistant(ctx, creds, inst) != nil {
		return provider.InstallCheck{Status: "warn", Detail: fmt.Sprintf("the instance does not accept the platform key yet and Cloud Assistant is not online (an instance started a moment ago reports in about a minute); the install waits up to %s for it, then adds the key (the remaining checks run once SSH logs in)", assistantWaitTimeout)}, true
	}
	return provider.InstallCheck{Status: "warn", Detail: "the instance does not accept the platform key yet; the install adds it through Cloud Assistant (the remaining checks run once SSH logs in)"}, true
}

// authorizeKeyTimeout bounds the one-line key script.
const authorizeKeyTimeout = time.Minute

// authorizeKeyScript appends key to root's authorized_keys unless the
// exact line is already there.
func authorizeKeyScript(key string) string {
	return `set -e
key=` + shellQuote(strings.TrimSpace(key)) + `
home=$(getent passwd root | cut -d: -f6)
[ -n "$home" ] || home=/root
mkdir -p "$home/.ssh"
chmod 700 "$home/.ssh"
touch "$home/.ssh/authorized_keys"
chmod 600 "$home/.ssh/authorized_keys"
if grep -qxF "$key" "$home/.ssh/authorized_keys"; then
  echo "the platform key was already in $home/.ssh/authorized_keys"
else
  printf '%s\n' "$key" >> "$home/.ssh/authorized_keys"
  echo "added the platform key to $home/.ssh/authorized_keys through Cloud Assistant"
fi`
}

// PrecheckCommand implements provider.CommandPrechecker: the same read-only
// facts the SSH precheck gathers, through Cloud Assistant. Each line the
// script prints is "name<TAB>status<TAB>detail".
func (d *Driver) PrecheckCommand(ctx context.Context, creds provider.Credentials, inst provider.Instance, connection provider.SSHConnection, req provider.CommandPrecheckRequest) ([]provider.InstallCheck, error) {
	if connection.Type != provider.ConnectionCloudAssistant {
		return nil, fmt.Errorf("aliyun: cannot precheck through %q", connection.Type)
	}
	if err := d.requireAssistant(ctx, creds, inst); err != nil {
		return []provider.InstallCheck{{Name: "Cloud Assistant", Status: "error", Detail: err.Error()}}, nil //nolint:nilerr // Cloud Assistant unavailability is returned as a failed precheck row.
	}
	result, err := d.runCommand(ctx, creds, inst, "gpuworks-precheck", precheckScript(req), precheckTimeout)
	if err != nil {
		return nil, err
	}
	checks := []provider.InstallCheck{{Name: "Cloud Assistant", Status: "ok", Detail: "online; checks ran as root"}}
	checks = append(checks, parseCheckLines(result.output)...)
	if !result.succeeded() {
		checks = append(checks, provider.InstallCheck{Name: "Precheck script", Status: "warn", Detail: result.describe() + ": " + tail(result.output)})
	}
	return checks, nil
}

// precheckScript prints one check per line. The names match the SSH
// precheck's, so the connectivity verdict reads "Server reachability" the
// same way whichever channel ran it.
func precheckScript(req provider.CommandPrecheckRequest) string {
	var b strings.Builder
	b.WriteString(`row() { printf '%s\t%s\t%s\n' "$1" "$2" "$3"; }
row "OS/Arch" ok "$(uname -s) $(uname -m) $(uname -r)"
if command -v nvidia-smi >/dev/null 2>&1 && gpus=$(nvidia-smi -L 2>&1); then row GPU ok "$(echo "$gpus" | head -n 8 | tr '\n' ';')"; else row GPU warn "no NVIDIA driver answered (nvidia-smi missing or failing); install the GPU driver before deploying"; fi
if command -v docker >/dev/null 2>&1; then row Docker ok "$(docker --version 2>&1 | head -n 1)"; else row Docker ok "not installed; the agent uses its built-in runtime (udocker)"; fi
`)
	if server := strings.TrimRight(strings.TrimSpace(req.ServerURL), "/"); server != "" && req.ConnectMode != "server_dial" {
		fmt.Fprintf(&b, `if out=$(curl -fsS --connect-timeout 3 --max-time 10 %s 2>&1); then row %q ok "$out"; else row %q error "$(echo "$out" | head -n 1)"; fi
`, shellWord(server+"/healthz"), "Server reachability", "Server reachability")
	}
	return b.String()
}

func shellWord(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func parseCheckLines(output string) []provider.InstallCheck {
	var checks []provider.InstallCheck
	for _, line := range strings.Split(output, "\n") {
		parts := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 3)
		if len(parts) != 3 {
			continue
		}
		switch parts[1] {
		case "ok", "warn", "error":
			checks = append(checks, provider.InstallCheck{Name: parts[0], Status: parts[1], Detail: strings.TrimRight(parts[2], ";")})
		}
	}
	return checks
}

// requireAssistant fails unless Cloud Assistant on the instance is online
// (a heartbeat within the last two minutes).
func (d *Driver) requireAssistant(ctx context.Context, creds provider.Credentials, inst provider.Instance) error {
	if inst.Region == "" {
		return fmt.Errorf("instance %s has no region", inst.ExternalID)
	}
	online, err := d.assistantOnline(ctx, creds, inst.Region, []string{inst.ExternalID})
	if err != nil {
		return err
	}
	if !online[inst.ExternalID] {
		return fmt.Errorf("Cloud Assistant is not online on %s (custom images may lack it; it reports a heartbeat every minute while the instance runs)", inst.ExternalID)
	}
	return nil
}

// invocation is a finished Cloud Assistant run.
type invocation struct {
	invokeID string
	status   string
	exitCode int
	errInfo  string
	output   string
}

func (r invocation) succeeded() bool { return r.status == "Success" && r.exitCode == 0 }

func (r invocation) describe() string {
	detail := fmt.Sprintf("ended %s with exit code %d", r.status, r.exitCode)
	if r.errInfo != "" {
		detail += " (" + r.errInfo + ")"
	}
	return detail
}

// runCommand runs a shell script as root through Cloud Assistant and waits
// for it. The script is not kept as a saved command (KeepCommand defaults to
// false), but the invocation record, script included, stays in the account
// for 30 days: the install script carries the node's token, which the agent
// keeps using, so anyone allowed ecs:DescribeInvocations can read it until
// the node is deleted (docs/how-to/rent-cloud-gpus.md#阿里云).
func (d *Driver) runCommand(ctx context.Context, creds provider.Credentials, inst provider.Instance, name, script string, timeout time.Duration) (invocation, error) {
	var started struct {
		InvokeID string `json:"InvokeId"`
	}
	params := map[string]string{
		"Type":           "RunShellScript",
		"CommandContent": script,
		"InstanceId.1":   inst.ExternalID,
		"Name":           name,
		"Timeout":        strconv.Itoa(int(timeout / time.Second)),
	}
	if err := d.api.call(ctx, creds, ecsProduct, inst.Region, "RunCommand", params, &started); err != nil {
		return invocation{}, err
	}
	if started.InvokeID == "" {
		return invocation{}, errors.New("RunCommand returned no invocation id")
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout+time.Minute)
	defer cancel()
	for {
		result, done, err := d.invocationResult(waitCtx, creds, inst, started.InvokeID)
		if err != nil {
			return invocation{}, err
		}
		if done {
			return result, nil
		}
		if err := d.api.sleep(waitCtx, pollInterval); err != nil {
			return invocation{}, fmt.Errorf("invocation %s: %w", started.InvokeID, err)
		}
	}
}

func (d *Driver) invocationResult(ctx context.Context, creds provider.Credentials, inst provider.Instance, invokeID string) (invocation, bool, error) {
	var out struct {
		Invocation struct {
			Results struct {
				Result []struct {
					InvocationStatus string `json:"InvocationStatus"`
					ExitCode         int    `json:"ExitCode"`
					ErrorInfo        string `json:"ErrorInfo"`
					Output           string `json:"Output"`
				} `json:"InvocationResult"`
			} `json:"InvocationResults"`
		} `json:"Invocation"`
	}
	params := map[string]string{"InvokeId": invokeID, "InstanceId": inst.ExternalID}
	if err := d.api.call(ctx, creds, ecsProduct, inst.Region, "DescribeInvocationResults", params, &out); err != nil {
		return invocation{}, false, err
	}
	if len(out.Invocation.Results.Result) == 0 {
		return invocation{}, false, nil
	}
	r := out.Invocation.Results.Result[0]
	switch r.InvocationStatus {
	case "Success", "Failed", "Timeout", "Error", "Aborted", "Cancelled", "Terminated", "Invalid":
	default: // Pending, Running, Stopping
		return invocation{}, false, nil
	}
	output, err := base64.StdEncoding.DecodeString(r.Output)
	if err != nil {
		output = []byte(r.Output)
	}
	return invocation{invokeID: invokeID, status: r.InvocationStatus, exitCode: r.ExitCode, errInfo: r.ErrorInfo, output: string(output)}, true, nil
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > outputTail {
		s = "…" + s[len(s)-outputTail:]
	}
	return s
}

// shellQuote single-quotes a value for a /bin/sh command line.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
