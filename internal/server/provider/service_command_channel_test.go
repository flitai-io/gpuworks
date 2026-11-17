package provider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// channelTestDriver lists one instance with Cloud Assistant and SSH, like an
// Alibaba Cloud instance with a public address.
type channelTestDriver struct {
	fallbackTestDriver
	connections   []string
	assistantErr  error
	precheckRows  []provision.CheckResult
	precheckCalls int
	last          InstallRequest
}

func (d *channelTestDriver) ListInstances(context.Context, Credentials) ([]Instance, error) {
	return []Instance{{
		ExternalID: "i-1", Name: "gpu-1", Region: "cn-shanghai", PublicIP: "203.0.113.20", PrivateIP: "127.0.0.1", Kind: "vm",
		SSHConnections: []SSHConnection{
			{ID: "direct", Type: "direct", Host: "203.0.113.20", Port: 22, User: "root", InstallSupported: true},
			{ID: ConnectionCloudAssistant, Type: ConnectionCloudAssistant, InstallSupported: true},
		},
	}}, nil
}

func (d *channelTestDriver) InstallAgentWithReport(_ context.Context, _ Credentials, req InstallRequest) (InstallReport, error) {
	d.connections = append(d.connections, req.Connection.ID)
	d.last = req
	if req.Connection.Type == ConnectionCloudAssistant {
		if req.Token == "" || req.ServerURL == "" {
			return InstallReport{}, errors.New("install request lacks the token or server URL")
		}
		// Like the Aliyun driver: the script install reports the directory
		// it was given.
		return InstallReport{InstallDir: req.InstallDir, Checks: []provision.CheckResult{{ID: "agent_process", Status: "pass"}}}, d.assistantErr
	}
	return InstallReport{Checks: []provision.CheckResult{{ID: "agent_process", Status: "pass"}}}, nil
}

func (d *channelTestDriver) PrecheckCommand(_ context.Context, _ Credentials, inst Instance, connection SSHConnection, req CommandPrecheckRequest) ([]provision.CheckResult, error) {
	d.precheckCalls++
	if inst.ExternalID != "i-1" || connection.Type != ConnectionCloudAssistant || req.ServerURL != "https://server.example" || req.ConnectMode != "agent_dial" {
		return nil, errors.New("unexpected precheck request")
	}
	return d.precheckRows, nil
}

func newChannelService(t *testing.T, name string) (*Service, uint, *channelTestDriver) {
	t.Helper()
	driver := &channelTestDriver{fallbackTestDriver: fallbackTestDriver{name: name}}
	svc, providerID := newFallbackService(t, &driver.fallbackTestDriver)
	// newFallbackService registered the embedded driver; register the
	// outer one under the same name so its overrides are what the service calls.
	Register(driver)
	return svc, providerID, driver
}

func TestInstallThroughCloudAssistantKeepsNoSSHCredential(t *testing.T) {
	svc, providerID, driver := newChannelService(t, "channel-install")
	result, err := svc.Install(t.Context(), providerID, "i-1", "https://server.example", InstallOptions{Password: "typed-but-unused"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(driver.connections, ",") != ConnectionCloudAssistant || result.SSHConnection == nil || result.SSHConnection.Type != ConnectionCloudAssistant {
		t.Fatalf("connections = %v, result = %+v", driver.connections, result.SSHConnection)
	}
	if result.SSHAttempts[0].Target != ConnectionCloudAssistant {
		t.Fatalf("attempts = %+v", result.SSHAttempts)
	}
	if _, _, err := svc.store.GetNodeSSHCredential(result.NodeUUID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a Cloud Assistant install must not record an SSH credential: %v", err)
	}
}

func TestOfflineCloudAssistantFallsBackToSSH(t *testing.T) {
	svc, providerID, driver := newChannelService(t, "channel-fallback")
	driver.assistantErr = RetryableSSHConnectionError("aliyun: Cloud Assistant", errors.New("not online"))
	result, err := svc.Install(t.Context(), providerID, "i-1", "https://server.example", InstallOptions{Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(driver.connections, ",") != ConnectionCloudAssistant+",direct" || result.SSHConnection.ID != "direct" {
		t.Fatalf("connections = %v, selected = %+v", driver.connections, result.SSHConnection)
	}
	if cred, secrets, err := svc.store.GetNodeSSHCredential(result.NodeUUID); err != nil || cred.Host != "203.0.113.20" || secrets.Password != "pw" {
		t.Fatalf("SSH fallback credential = %+v %+v %v", cred, secrets, err)
	}
}

// The precheck runs through the driver's channel, then the server probes the
// instance itself, so a node that cannot reach the server is explained.
func TestPrecheckThroughCloudAssistantAddsTheConnectivityVerdict(t *testing.T) {
	svc, providerID, driver := newChannelService(t, "channel-precheck")
	driver.precheckRows = []provision.CheckResult{
		{ID: "os_arch", Status: "pass", Message: "Linux x86_64"},
		{ID: "server_reachability", Status: "fail", Message: "curl: (28) Connection timed out"},
	}
	report, err := svc.Precheck(t.Context(), providerID, "i-1", "https://server.example", InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, check := range report.Checks {
		names = append(names, check.ID+"="+check.Status)
	}
	// The private address 127.0.0.1 refuses on the listen port: reachable.
	want := "os_arch=pass,server_reachability=fail,server_to_agent=pass,connectivity=warn"
	if strings.Join(names, ",") != want || driver.precheckCalls != 1 {
		t.Fatalf("checks = %s\nwant   %s", strings.Join(names, ","), want)
	}
	if verdict := report.Checks[3]; verdict.Code != messages.CheckConnectivityUseServerDial || !strings.Contains(checkText(verdict), "server_dial") {
		t.Fatalf("verdict = %s", verdict)
	}
}

// The operator's directory and supervisor reach the driver, and the directory
// a command-channel install reports is recorded like an SSH install's: SSH
// credentials added later recover the agent from there.
func TestInstallCarriesTheDirectoryAndSupervisorAndRecordsTheDirectory(t *testing.T) {
	svc, providerID, driver := newChannelService(t, "channel-install-dir")
	result, err := svc.Install(t.Context(), providerID, "i-1", "https://server.example", InstallOptions{InstallDir: " /data/gpuworks/ ", NoSystemd: true})
	if err != nil {
		t.Fatal(err)
	}
	if driver.last.InstallDir != "/data/gpuworks" || !driver.last.NoSystemd {
		t.Fatalf("driver request dir=%q no_systemd=%v", driver.last.InstallDir, driver.last.NoSystemd)
	}
	if opts := driver.last.ScriptOptions(); opts.InstallDir != "/data/gpuworks" || !opts.NoSystemd {
		t.Fatalf("script options = %+v", opts)
	}
	node, err := svc.store.NodeByUUID(result.NodeUUID)
	if err != nil || node.InstallDir != "/data/gpuworks" {
		t.Fatalf("recorded install dir = %q err=%v", node.InstallDir, err)
	}
}
