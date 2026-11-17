package provider

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/pkg/localization"
)

// channelDriver lists a command channel before direct SSH, as Alibaba Cloud
// does, records every connection an install was tried over, and reports the
// ports its inbound check was asked about.
type channelDriver struct {
	keyTestDriver
	tried   []string
	inbound []int
}

func (d *channelDriver) ListInstances(context.Context, Credentials) ([]Instance, error) {
	return []Instance{{ExternalID: "inst-1", Name: "gpu", PublicIP: d.host, Power: PowerRunning, SSHConnections: []SSHConnection{
		{ID: ConnectionCloudAssistant, Type: ConnectionCloudAssistant, Label: localization.SameText("Cloud Assistant", requiredProviderLocales...), InstallSupported: true},
		{ID: "direct", Type: "direct", Label: localization.SameText("Direct", requiredProviderLocales...), Host: d.host, Port: d.port, User: "root", InstallSupported: true},
	}}}, nil
}

func (d *channelDriver) InstallAgentWithReport(_ context.Context, _ Credentials, req InstallRequest) (InstallReport, error) {
	d.tried = append(d.tried, req.Connection.ID)
	d.req = req
	return d.report, nil
}

func (d *channelDriver) InstallAgent(ctx context.Context, creds Credentials, req InstallRequest) error {
	_, err := d.InstallAgentWithReport(ctx, creds, req)
	return err
}

func (d *channelDriver) CheckInbound(_ context.Context, _ Credentials, _ Instance, port int) provision.CheckResult {
	d.inbound = append(d.inbound, port)
	return provision.CheckResult{ID: "security_group", Status: "fail", Message: "no inbound rule"}
}

// A command channel installs by having the instance download from the
// server, which a server-dialed instance cannot reach: an automatic choice
// goes straight to SSH instead of reporting a failed attempt first.
func TestServerDialAutoSelectionSkipsTheCommandChannel(t *testing.T) {
	d := &channelDriver{keyTestDriver: keyTestDriver{name: "key-test-channel", host: "203.0.113.40", port: 22,
		report: InstallReport{AuthUsed: "platform_key", PlatformKeyAuthorized: true}}}
	svc, _, providerID, _ := newKeyService(t, &d.keyTestDriver, true)
	Register(d)
	result, err := svc.Install(t.Context(), providerID, "inst-1", "", InstallOptions{ConnectMode: "server_dial", ServerDialAddress: "203.0.113.40"})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.tried) != 1 || d.tried[0] != "direct" || len(result.SSHAttempts) != 1 || result.SSHAttempts[0].Status != "selected" {
		t.Fatalf("tried %v, attempts %+v", d.tried, result.SSHAttempts)
	}

	// The agent connecting to the server keeps the command channel first.
	d.tried = nil
	if _, err := svc.Install(t.Context(), providerID, "inst-1", "https://server.example", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(d.tried) != 1 || d.tried[0] != ConnectionCloudAssistant {
		t.Fatalf("agent_dial tried %v", d.tried)
	}
}

// The precheck asks the provider's firewall about the port the server will
// dial: the one in the access address, else the listen port; and only for
// an agent the server dials.
func TestPrecheckAsksTheFirewallAboutTheDialedPort(t *testing.T) {
	d := &channelDriver{keyTestDriver: keyTestDriver{name: "key-test-inbound", host: "127.0.0.1", port: 1}}
	svc, _, providerID, _ := newKeyService(t, &d.keyTestDriver, true)
	Register(d)
	for _, opts := range []InstallOptions{
		{ConnectMode: "server_dial"},
		{ConnectMode: "server_dial", ServerDialListen: "0.0.0.0:6006"},
		{ConnectMode: "server_dial", ServerDialListen: "0.0.0.0:6006", ServerDialAddress: "203.0.113.40:36006"},
		{ConnectMode: "agent_dial"},
	} {
		report, err := svc.Precheck(t.Context(), providerID, "inst-1", "https://server.example", opts)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, c := range report.Checks {
			found = found || (c.ID == "security_group" && c.Status == "fail")
		}
		if found != (opts.ConnectMode == "server_dial") {
			t.Fatalf("%+v: security group row present = %v in %+v", opts, found, report.Checks)
		}
	}
	if len(d.inbound) != 3 || d.inbound[0] != 18800 || d.inbound[1] != 6006 || d.inbound[2] != 36006 {
		t.Fatalf("ports asked = %v", d.inbound)
	}
}
