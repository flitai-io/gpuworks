package runpod

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

// InstallAgent implements provider.Driver. It uploads the embedded agent over
// the pod's exposed TCP SSH endpoint and starts it remotely.
func (d *Driver) InstallAgent(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) error {
	_, err := d.InstallAgentWithReport(ctx, creds, req)
	return err
}

func (d *Driver) InstallAgentWithReport(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) (provider.InstallReport, error) {
	// A pod started before the platform key was in the account does not
	// accept it; adding it restarts the pod, so only with consent.
	instance, keyCheck, err := d.ensurePlatformKey(ctx, creds, req)
	if err != nil {
		return provider.InstallReport{}, err
	}
	req.Instance = instance
	listen, address := req.ServerDialListen, req.ServerDialAddress
	var portCheck provision.CheckResult
	if req.ConnectMode == "server_dial" && strings.TrimSpace(address) == "" {
		var err error
		listen, address, instance, portCheck, err = d.serverDialPort(ctx, creds, req)
		if err != nil {
			return provider.InstallReport{}, err
		}
	}
	if instance.PublicIP == "" || instance.SSHPort == 0 || instance.SSHUser != "root" {
		if detailed, err := d.getPod(ctx, creds, instance.ExternalID); err == nil {
			resolved := detailed.toInstance()
			if resolved.PublicIP != "" && resolved.SSHPort != 0 {
				instance.PublicIP = resolved.PublicIP
				instance.SSHPort = resolved.SSHPort
				instance.SSHUser = resolved.SSHUser
			}
		}
	}
	if instance.PublicIP == "" || instance.SSHPort == 0 {
		return provider.InstallReport{}, fmt.Errorf("runpod: no ssh access for pod %q; install manually", req.Instance.ExternalID)
	}
	if req.ReadAgentBinary == nil {
		return provider.InstallReport{}, fmt.Errorf("runpod: agent binary reader is not configured")
	}

	installer := provision.NewSSHInstaller(provision.AgentBinaryReader(req.ReadAgentBinary))
	report, err := installer.InstallSSH(ctx, provision.SSHInstallRequest{
		Target:            instance.PublicIP,
		User:              firstNonEmpty(instance.SSHUser, "root"),
		Port:              instance.SSHPort,
		ProviderKind:      "runpod",
		InstanceKind:      req.InstanceKind,
		ServerURL:         req.ServerURL,
		Token:             req.Token,
		ConnectMode:       providerInstallConnectMode(req.ConnectMode),
		ServerDialListen:  providerInstallListen(listen),
		ServerDialAddress: address,
		InstallDir:        req.InstallDir,
		NoSystemd:         req.NoSystemd,
		SSHAuth:           req.SSHAuth(),
	})
	if err != nil {
		var connectionErr *provision.SSHConnectionError
		if errors.As(err, &connectionErr) {
			return provider.InstallReport{}, provider.RetryableSSHConnectionError("runpod: SSH connection", connectionErr)
		}
		return provider.InstallReport{}, fmt.Errorf("runpod: upload/install agent on %s:%d: %w", instance.PublicIP, instance.SSHPort, err)
	}
	if len(report.Checks) == 0 {
		return provider.InstallReport{}, fmt.Errorf("runpod: installer returned no checks for pod %q", instance.ExternalID)
	}
	out := provider.ReportFromSSHInstall(report)
	// A pod edit (key or port) renumbers SSH: record where the install
	// actually logged in.
	out.SSHHost, out.SSHPort = instance.PublicIP, instance.SSHPort
	out.PlatformKeyVolatile = d.platformKeyVolatile(ctx, creds, req, out)
	if keyCheck.ID != "" {
		out.Checks = append([]provision.CheckResult{keyCheck}, out.Checks...)
	}
	if req.ConnectMode == "server_dial" {
		out.ServerDialListen, out.ServerDialAddress = providerInstallListen(listen), address
		if portCheck.ID != "" {
			out.Checks = append([]provision.CheckResult{portCheck}, out.Checks...)
		}
	}
	return out, nil
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
		return provision.DefaultServerDialListen
	}
	return listen
}

// platformKeyVolatile reports whether the platform key the install authorized
// will be gone after the pod restarts. A pod's keys are the PUBLIC_KEY it was
// created (or last edited) with; a key the install appended to the pod's
// authorized_keys lives on the container disk, which an edit of the pod
// clears. When the pod cannot be read, assume the key is volatile: keeping the
// credential that logged in costs nothing.
func (d *Driver) platformKeyVolatile(ctx context.Context, creds provider.Credentials, req provider.InstallRequest, report provider.InstallReport) bool {
	if !report.PlatformKeyAuthorized || req.PlatformKey == nil {
		return false
	}
	p, err := d.getPod(ctx, creds, req.Instance.ExternalID)
	return err != nil || !p.hasPublicKey(req.PlatformKey.PublicKey)
}
