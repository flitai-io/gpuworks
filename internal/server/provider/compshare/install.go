package compshare

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

func (d *Driver) InstallAgent(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) error {
	_, err := d.InstallAgentWithReport(ctx, creds, req)
	return err
}

func (d *Driver) InstallAgentWithReport(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) (provider.InstallReport, error) {
	if req.Instance.PublicIP == "" || req.Token == "" {
		return provider.InstallReport{}, fmt.Errorf("compshare: install requires instance public IP and token")
	}
	// A server-dialed agent never connects out, so it is installed without a
	// server URL.
	if req.ServerURL == "" && req.ConnectMode != "server_dial" {
		return provider.InstallReport{}, fmt.Errorf("compshare: install requires a server URL unless the server dials the agent")
	}
	listen, address := providerInstallListen(req.ServerDialListen), req.ServerDialAddress
	var chosen provider.InstallCheck
	autoPort := req.ConnectMode == "server_dial" && strings.TrimSpace(address) == "" && d.MapsServerDialPorts(req.Instance)
	if autoPort {
		var err error
		if listen, address, chosen, err = d.serverDialPort(ctx, creds, req); err != nil {
			return provider.InstallReport{}, err
		}
	}
	installer := provision.NewSSHInstaller(provision.AgentBinaryReader(req.ReadAgentBinary))
	installer.HostKeys = d.hostKeys
	report, err := installer.InstallSSH(ctx, provision.SSHInstallRequest{
		Target: net.JoinHostPort(req.Instance.PublicIP, strconv.Itoa(firstPort(req.Instance.SSHPort))),
		User:   firstNonEmptyLocal(req.Instance.SSHUser, "root"), Port: firstPort(req.Instance.SSHPort), ServerURL: req.ServerURL, Token: req.Token,
		ProviderKind: "compshare", Region: req.Instance.Region, InstanceKind: req.InstanceKind,
		ConnectMode: providerInstallConnectMode(req.ConnectMode), ServerDialListen: listen,
		ServerDialAddress: address,
		InstallDir:        req.InstallDir,
		NoSystemd:         req.NoSystemd,
		SSHAuth:           req.SSHAuth(),
	})
	if err != nil {
		var connectionErr *provision.SSHConnectionError
		if errors.As(err, &connectionErr) {
			return provider.InstallReport{}, provider.RetryableSSHConnectionError("compshare: SSH connection", connectionErr)
		}
		return provider.InstallReport{}, fmt.Errorf("compshare: upload/install agent: %w", err)
	}
	if len(report.Checks) == 0 {
		return provider.InstallReport{}, fmt.Errorf("compshare: installer returned no checks")
	}
	out := provider.ReportFromSSHInstall(report)
	if autoPort {
		out.Checks = append([]provider.InstallCheck{chosen}, out.Checks...)
		out.ServerDialListen, out.ServerDialAddress = listen, address
	}
	return out, nil
}

func firstPort(port int) int {
	if port > 0 {
		return port
	}
	return 22
}

func firstNonEmptyLocal(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

// acceleratedDNSCommand enables the Compshare UAAA resolver only in regions
// where the platform documents the accelerated route. It is deliberately
// best-effort: an image may manage resolv.conf itself, and agent installation
// must still work when that file is immutable or sudo is unavailable.
func acceleratedDNSCommand(region string) string {
	if !compshareAcceleratedRegion(region) {
		return ""
	}
	return "(printf 'nameserver 100.90.90.90\\nnameserver 100.90.90.100\\n' >/etc/resolv.conf 2>/dev/null || sudo -n sh -c \"printf 'nameserver 100.90.90.90\\\\nnameserver 100.90.90.100\\\\n' >/etc/resolv.conf\" 2>/dev/null || true);"
}

func compshareAcceleratedRegion(region string) bool {
	s := strings.ToLower(strings.TrimSpace(region))
	s = strings.ReplaceAll(s, " ", "")
	for _, marker := range []string{
		"华北一c", "上海二a", "上海二b", "华北二a", "华北二c",
		"cn-wlcb", "cn-sh2-a", "cn-sh2-b", "cn-bj2-a", "cn-bj2-c",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

func providerInstallConnectMode(mode string) string {
	if mode == "server_dial" {
		return "server_dial"
	}
	return "agent_dial"
}

func providerInstallListen(listen string) string {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return ":3389"
	}
	return listen
}
