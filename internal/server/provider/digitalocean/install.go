package digitalocean

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

// InstallAgent implements provider.Driver.
func (d *Driver) InstallAgent(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) error {
	_, err := d.InstallAgentWithReport(ctx, creds, req)
	return err
}

// InstallAgentWithReport implements provider.ReportingDriver: the agent is
// uploaded and started over SSH on the droplet's public address. The login
// is the platform key when the droplet was created with it, else what the
// operator gave (DigitalOcean cannot add a key to an existing droplet).
func (d *Driver) InstallAgentWithReport(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) (provider.InstallReport, error) {
	inst := req.Instance
	if inst.Cluster != "" {
		return provider.InstallReport{}, &provider.InstanceInClusterError{Cluster: inst.Cluster}
	}
	if strings.TrimSpace(inst.PublicIP) == "" && inst.ExternalID != "" {
		if fresh, err := d.GetInstance(ctx, creds, inst.ExternalID); err == nil {
			inst.PublicIP = fresh.PublicIP
		}
	}
	if strings.TrimSpace(inst.PublicIP) == "" {
		return provider.InstallReport{}, fmt.Errorf("digitalocean: droplet %q has no public address yet; install once it is active", inst.ExternalID)
	}
	if req.ServerURL == "" || req.Token == "" {
		return provider.InstallReport{}, errors.New("digitalocean: install requires the server URL and a token")
	}
	if req.ReadAgentBinary == nil {
		return provider.InstallReport{}, errors.New("digitalocean: agent binary reader is not configured")
	}
	port := inst.SSHPort
	if port <= 0 {
		port = 22
	}
	user := strings.TrimSpace(inst.SSHUser)
	if user == "" {
		user = "root"
	}
	installer := provision.NewSSHInstaller(provision.AgentBinaryReader(req.ReadAgentBinary))
	report, err := installer.InstallSSH(ctx, provision.SSHInstallRequest{
		Target:            net.JoinHostPort(inst.PublicIP, strconv.Itoa(port)),
		User:              user,
		Port:              port,
		ProviderKind:      "digitalocean",
		Region:            inst.Region,
		InstanceKind:      req.InstanceKind,
		ServerURL:         req.ServerURL,
		Token:             req.Token,
		ConnectMode:       connectMode(req.ConnectMode),
		ServerDialListen:  dialListen(req.ServerDialListen),
		ServerDialAddress: req.ServerDialAddress,
		InstallDir:        req.InstallDir,
		NoSystemd:         req.NoSystemd,
		SSHAuth:           req.SSHAuth(),
	})
	if err != nil {
		var connectionErr *provision.SSHConnectionError
		if errors.As(err, &connectionErr) {
			return provider.InstallReport{}, provider.RetryableSSHConnectionError("digitalocean: SSH connection", connectionErr)
		}
		return provider.InstallReport{}, fmt.Errorf("digitalocean: upload/install agent on %s:%d: %w", inst.PublicIP, port, err)
	}
	if len(report.Checks) == 0 {
		return provider.InstallReport{}, fmt.Errorf("digitalocean: installer returned no checks for droplet %q", inst.ExternalID)
	}
	return provider.ReportFromSSHInstall(report), nil
}

func connectMode(mode string) string {
	if mode == "server_dial" {
		return "server_dial"
	}
	return "agent_dial"
}

func dialListen(listen string) string {
	if listen = strings.TrimSpace(listen); listen == "" {
		return provision.DefaultServerDialListen
	}
	return listen
}
