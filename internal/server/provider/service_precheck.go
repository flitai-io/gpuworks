package provider

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provision"
)

// Precheck performs the read-only checks used by the cloud install wizard:
// over SSH, or through the provider's command channel when that is the
// chosen path. Providers that do not expose either leave the driver to report
// the provider-specific failure; this keeps the service usable with API-only
// test drivers and vendor-native exec transports.
func (s *Service) Precheck(ctx context.Context, providerID uint, externalID, serverURL string, opts InstallOptions) (provision.SSHPrecheckReport, error) {
	p, drv, creds, target, err := s.installTarget(ctx, providerID, externalID)
	if err != nil {
		return provision.SSHPrecheckReport{}, err
	}
	candidates, candidateErr := SSHInstallCandidates(*target, opts.SSHConnectionID)
	if candidateErr != nil && target.Power == PowerStopped {
		return provision.SSHPrecheckReport{Target: externalID, Checks: []provision.CheckResult{{Name: "SSH", Status: "error", Detail: stoppedInstanceError(*target).Error()}}}, nil
	}
	if candidateErr != nil {
		return provision.SSHPrecheckReport{Target: externalID, Checks: []provision.CheckResult{{Name: "SSH", Status: "warn", Detail: "provider did not expose an SSH endpoint; driver-native install will validate access"}}}, nil
	}
	mode := normalizeInstallConnectMode(opts.ConnectMode)
	selected := ServerDialCandidates(candidates, mode, opts.SSHConnectionID)[0]
	dialHost := precheckDialHost(*target, selected, firstNonBlank(opts.ServerDialAddress, opts.HostAddress))
	if !IsSSHConnection(selected) {
		return s.precheckCommand(ctx, drv, creds, *target, selected, serverURL, mode, dialHost, opts.ServerDialListen), nil
	}
	access := s.prepareSSHAccess(ctx, drv, creds, externalID)
	if checker, ok := drv.(InboundRuleChecker); ok && mode == "server_dial" {
		access.checks = append(access.checks, checker.CheckInbound(ctx, creds, *target, serverDialPort(opts)))
	}
	port := selected.Port
	user := selected.User
	if user == "" {
		user = "root"
	}
	checker := s.sshPrecheck
	if checker == nil {
		defaults := provision.NewSSHPrechecker()
		// A newly booted cloud VM can accept TCP/SSH before it has enough CPU
		// to start all probe sessions within five seconds. Keep connection
		// setup and the whole precheck independently bounded while leaving
		// enough time for that first-boot load to settle.
		defaults.Timeout = 10 * time.Second
		checker = defaults
	}
	precheckCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	report, err := checker.PrecheckSSH(precheckCtx, provision.SSHPrecheckRequest{
		Target: net.JoinHostPort(selected.Host, strconv.Itoa(port)), User: user, Port: port, ServerURL: serverURL, ConnectMode: mode, InstanceKind: target.Kind,
		ProviderKind: p.Kind, ServerDialListen: opts.ServerDialListen, InstallDir: opts.InstallDir,
		HostAddress: dialHost, SkipServerToAgent: dialHost == "", ProviderChoosesPort: providerChoosesPort(drv, *target, mode, opts),
		SSHAuth: InstallRequest{SSHKeyPEM: opts.SSHKeyPEM, Password: opts.Password, PlatformKey: access.platformKey, InstancePassword: access.instancePassword}.SSHAuth(),
	})
	prepared := make([]provision.CheckResult, 0, len(access.checks))
	for _, check := range access.checks {
		prepared = append(prepared, provision.CheckResult{Name: check.Name, Status: check.Status, Detail: check.Detail})
	}
	if err != nil {
		if check, ok := s.keyAddedAtInstall(ctx, drv, creds, *target, access, err); ok {
			return provision.SSHPrecheckReport{Target: externalID, Checks: append(prepared, check)}, nil
		}
		// Keep what the server did to prepare access next to the SSH failure.
		return provision.SSHPrecheckReport{Target: externalID, Checks: append(prepared, provision.CheckResult{Name: "SSH", Status: "error", Detail: err.Error()})}, nil
	}
	report.Checks = append(prepared, report.Checks...)
	return report, nil
}

// keyAddedAtInstall is the SSH row for a login the instance refused when the
// install will add the platform key itself (InstallKeyAdder).
func (s *Service) keyAddedAtInstall(ctx context.Context, drv Driver, creds Credentials, target Instance, access sshAccess, err error) (provision.CheckResult, bool) {
	var authErr *provision.SSHAuthError
	adder, ok := drv.(InstallKeyAdder)
	if !ok || access.platformKey == nil || !errors.As(err, &authErr) {
		return provision.CheckResult{}, false
	}
	check, ok := adder.PlatformKeyAtInstall(ctx, creds, target, access.platformKey.PublicKey)
	if !ok {
		return provision.CheckResult{}, false
	}
	return provision.CheckResult{Name: "SSH", Status: check.Status, Detail: check.Detail}, true
}

// precheckCommand runs the precheck through a provider command channel.
func (s *Service) precheckCommand(ctx context.Context, drv Driver, creds Credentials, target Instance, connection SSHConnection, serverURL, mode, dialHost, listen string) provision.SSHPrecheckReport {
	report := provision.SSHPrecheckReport{Target: target.ExternalID}
	prechecker, ok := drv.(CommandPrechecker)
	if !ok {
		report.Checks = []provision.CheckResult{{Name: connection.Type, Status: "warn", Detail: "the driver cannot precheck through this connection; the install will validate access"}}
		return report
	}
	checks, err := prechecker.PrecheckCommand(ctx, creds, target, connection, CommandPrecheckRequest{ServerURL: serverURL, ConnectMode: mode})
	for _, check := range checks {
		report.Checks = append(report.Checks, provision.CheckResult{Name: check.Name, Status: check.Status, Detail: check.Detail})
	}
	if err != nil {
		report.Checks = append(report.Checks, provision.CheckResult{Name: connection.Type, Status: "error", Detail: err.Error()})
		return report
	}
	report.Checks = provision.AppendConnectivity(ctx, report.Checks, mode, dialHost, listen)
	return report
}

// serverDialPort is the TCP port the server dials: the one in the access
// address the operator gave, else the listen port.
func serverDialPort(opts InstallOptions) int {
	if _, port, err := net.SplitHostPort(strings.TrimSpace(opts.ServerDialAddress)); err == nil {
		if n, err := strconv.Atoi(port); err == nil {
			return n
		}
	}
	return PortOf(normalizeServerDialListen(opts.ServerDialListen))
}

// providerChoosesPort reports whether the install, not the operator, picks
// where a server-dialed agent listens and is reached (ServerDialPortMapper
// with no address given): the precheck cannot know that port yet.
func providerChoosesPort(drv Driver, target Instance, mode string, opts InstallOptions) bool {
	return MapsServerDialPorts(drv, target) && mode == "server_dial" && strings.TrimSpace(opts.ServerDialAddress) == ""
}

// precheckDialHost is the address the server would dial a server_dial agent
// at: the operator's choice, else the instance's private then public
// address. An SSH proxy host is not the instance, so a proxy-only instance
// without either address has none.
func precheckDialHost(target Instance, selected SSHConnection, hostAddress string) string {
	for _, candidate := range []string{hostAddress, target.PrivateIP, target.PublicIP} {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			if selected.Type == "proxy" && candidate == strings.TrimSpace(selected.Host) {
				continue
			}
			return candidate
		}
	}
	if selected.Type == "direct" {
		return strings.TrimSpace(selected.Host)
	}
	return ""
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
