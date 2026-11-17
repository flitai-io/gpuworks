package provision

import "testing"

// When the install picks the port a provider maps (Vast, RunPod), the
// precheck does not probe the default listen port nor dial the instance on
// it: neither is where the agent will listen or be reached.
func TestPrecheckLeavesTheServerDialPortToTheInstallWhenTheProviderChoosesIt(t *testing.T) {
	dir := realTempDir(t)
	host, port := startFakeSSHServer(t, "root", "pw", scenarioHandler(t, dirScenario{}, dir))
	report, err := NewSSHPrechecker().PrecheckSSH(t.Context(), SSHPrecheckRequest{
		Target: "root@" + host, Port: port, InstallDir: dir, ConnectMode: "server_dial", ServerDialListen: "0.0.0.0:18800",
		HostAddress: "192.0.2.1", ProviderChoosesPort: true, SSHAuth: SSHAuth{Mode: AuthPassword, Password: "pw"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantCheck(t, report.Checks, CheckIDServerToAgent, "skip", "checked during the install")
	for _, c := range report.Checks {
		if c.ID == "agent_listen_port" || c.ID == CheckIDConnectivity {
			t.Fatalf("unexpected row %+v in %+v", c, report.Checks)
		}
	}
}
