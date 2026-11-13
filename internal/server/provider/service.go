package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/nodeaddr"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Service ties provider drivers to persistence: credential encryption, instance
// sync, and agent installation.
type Service struct {
	store           *store.Store
	box             *auth.Secretbox
	readAgentBinary AgentBinaryReader
	drivers         map[string]Driver
	clusterLists    clusterListCache
	// sshPrecheck replaces the SSH precheck an install precheck runs; nil is
	// the real one (provision.NewSSHPrechecker).
	sshPrecheck provision.Prechecker
}

// NewService builds the provider service. box encrypts stored credentials.
func NewService(st *store.Store, box *auth.Secretbox) *Service {
	if st != nil && box != nil {
		st.SetSecretbox(box)
	}
	return &Service{store: st, box: box}
}

// SetDriver configures an app-local driver before requests are served.
func (s *Service) SetDriver(driver Driver) {
	if s.drivers == nil {
		s.drivers = make(map[string]Driver)
	}
	s.drivers[driver.Name()] = driver
}

// SetSSHPrechecker replaces the SSH precheck that runs before an install,
// before requests are served. A server keeps the real one; a harness whose
// instances are fixtures, with no SSH server behind their addresses, answers
// for them instead of waiting out a connection timeout per install.
func (s *Service) SetSSHPrechecker(p provision.Prechecker) {
	s.sshPrecheck = p
}

func (s *Service) driver(kind string) (Driver, bool) {
	if driver, ok := s.drivers[kind]; ok {
		return driver, true
	}
	return Get(kind)
}

// DriverCredentials exposes decrypted credentials to provider resource services.
func (s *Service) DriverCredentials(id uint) (Driver, Credentials, error) {
	_, driver, credentials, err := s.driverCreds(id)
	return driver, credentials, err
}

// SetAgentBinaryReader provides embedded or operator-overridden agent builds
// to provider drivers that upload over SSH instead of downloading remotely.
func (s *Service) SetAgentBinaryReader(reader AgentBinaryReader) {
	s.readAgentBinary = reader
}

// AvailableKinds returns the registered provider driver schemas.
func (s *Service) AvailableKinds() []KindInfo { return KindInfos() }

// KindCapabilities are what a provider kind's driver supports.
func (s *Service) KindCapabilities(kind string) (Capabilities, bool) {
	drv, ok := s.driver(kind)
	if !ok {
		return Capabilities{}, false
	}
	return drv.Capabilities(), true
}

// Add validates credentials against the vendor and stores the provider with the
// credentials encrypted.
func (s *Service) Add(ctx context.Context, kind, name string, creds Credentials) (*store.Provider, error) {
	drv, ok := s.driver(kind)
	if !ok {
		return nil, fmt.Errorf("unknown provider kind: %s", kind)
	}
	if err := drv.ValidateCredentials(ctx, creds); err != nil {
		return nil, fmt.Errorf("credential validation failed: %w", err)
	}
	p, err := s.create(kind, name, creds)
	if err != nil {
		return nil, err
	}
	// Providers whose instances take account keys when they start (RunPod)
	// get the platform key now, so instances started from now on — in the
	// vendor's console too — accept the server without any step.
	authorizeBeforeCreate(ctx, s, drv, creds)
	return p, nil
}

// Update validates and replaces a provider's credentials while keeping its
// provider kind fixed.
func (s *Service) Update(ctx context.Context, providerID uint, name string, creds Credentials) (*store.Provider, error) {
	p, err := s.store.GetProvider(providerID)
	if err != nil {
		return nil, err
	}
	drv, ok := s.driver(p.Kind)
	if !ok {
		return nil, fmt.Errorf("driver %s not registered", p.Kind)
	}
	base, err := s.creds(p)
	if err != nil {
		return nil, err
	}
	creds = mergeCredentials(base, creds)
	if err := drv.ValidateCredentials(ctx, creds); err != nil {
		return nil, fmt.Errorf("credential validation failed: %w", err)
	}
	raw, err := json.Marshal(creds)
	if err != nil {
		return nil, err
	}
	cipher, err := s.box.Encrypt(raw)
	if err != nil {
		return nil, err
	}
	return s.store.UpdateProvider(providerID, name, cipher, "valid")
}

func mergeCredentials(base, patch Credentials) Credentials {
	out := base
	if strings.TrimSpace(patch.Endpoint) != "" {
		out.Endpoint = patch.Endpoint
	}
	if strings.TrimSpace(patch.APIKey) != "" {
		out.APIKey = patch.APIKey
	}
	if strings.TrimSpace(patch.PublicKey) != "" {
		out.PublicKey = patch.PublicKey
	}
	if strings.TrimSpace(patch.PrivateKey) != "" {
		out.PrivateKey = patch.PrivateKey
	}
	if out.Extra == nil {
		out.Extra = map[string]string{}
	}
	for k, v := range patch.Extra {
		if strings.TrimSpace(v) != "" {
			out.Extra[k] = v
		}
	}
	return out
}

// EditableCredentials returns only stored fields declared non-secret by the driver.
// It does not apply defaults: an empty stored endpoint is distinct from an override.
func (s *Service) EditableCredentials(p *store.Provider) (Credentials, error) {
	drv, ok := s.driver(p.Kind)
	if !ok {
		return Credentials{}, fmt.Errorf("provider configuration unavailable")
	}
	creds, err := s.creds(p)
	if err != nil {
		// Decryption/JSON errors must not expose any stored credential material.
		return Credentials{}, fmt.Errorf("provider configuration unavailable")
	}
	out := Credentials{Extra: map[string]string{}}
	for _, field := range driverCredentialFields(drv) {
		if field.Secret {
			continue
		}
		if field.Extra {
			if value, ok := creds.Extra[field.Name]; ok {
				out.Extra[field.Name] = value
			}
			continue
		}
		switch field.Name {
		case "endpoint":
			out.Endpoint = creds.Endpoint
		case "public_key":
			out.PublicKey = creds.PublicKey
		}
	}
	return out, nil
}

// creds decrypts a provider's stored credentials.
func (s *Service) creds(p *store.Provider) (Credentials, error) {
	var c Credentials
	raw, err := s.box.Decrypt(p.CredsCipher)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(raw, &c)
	return c, err
}

func (s *Service) driverCreds(providerID uint) (*store.Provider, Driver, Credentials, error) {
	p, err := s.store.GetProvider(providerID)
	if err != nil {
		return nil, nil, Credentials{}, err
	}
	drv, ok := s.driver(p.Kind)
	if !ok {
		return nil, nil, Credentials{}, fmt.Errorf("driver %s not registered", p.Kind)
	}
	creds, err := s.creds(p)
	if err != nil {
		return nil, nil, Credentials{}, err
	}
	return p, drv, creds, nil
}

// Power toggles one provider instance using the provider's native API.
func (s *Service) Power(ctx context.Context, providerID uint, externalID string, on bool) error {
	_, drv, creds, err := s.driverCreds(providerID)
	if err != nil {
		return err
	}
	if err := s.refuseClusterNode(providerID, externalID); err != nil {
		return err
	}
	if on {
		return drv.PowerOn(ctx, creds, externalID)
	}
	return drv.PowerOff(ctx, creds, externalID)
}

// CreateInstance asks a provider to rent/create a new instance.
func (s *Service) CreateInstance(ctx context.Context, providerID uint, req CreateInstanceRequest) (Instance, error) {
	_, drv, creds, err := s.driverCreds(providerID)
	if err != nil {
		return Instance{}, err
	}
	authorizeBeforeCreate(ctx, s, drv, creds)
	return drv.CreateInstance(ctx, creds, req)
}

// DeleteInstance asks a provider to remove one provider instance and, once the
// vendor confirmed it, drops the instance's row. A failed vendor call leaves
// the row as it was.
func (s *Service) DeleteInstance(ctx context.Context, providerID uint, externalID string) error {
	_, drv, creds, err := s.driverCreds(providerID)
	if err != nil {
		return err
	}
	if err := s.refuseClusterNode(providerID, externalID); err != nil {
		return err
	}
	if err := drv.DeleteInstance(ctx, creds, externalID); err != nil {
		return err
	}
	if err := s.store.DeleteProviderInstance(providerID, externalID); err != nil {
		return fmt.Errorf("instance %s was released but its record was not removed: %w", externalID, err)
	}
	return nil
}

// ResizeInstance asks a provider to resize one provider instance.
func (s *Service) ResizeInstance(ctx context.Context, providerID uint, externalID string, req ResizeInstanceRequest) error {
	_, drv, creds, err := s.driverCreds(providerID)
	if err != nil {
		return err
	}
	if err := s.refuseClusterNode(providerID, externalID); err != nil {
		return err
	}
	return drv.ResizeInstance(ctx, creds, externalID, req)
}

// Sync lists the provider's instances and makes the stored rows match: listed
// instances are upserted and rows for instances the vendor no longer lists are
// removed. A failed listing changes no rows. This relies on drivers returning
// either their complete listing or an error.
func (s *Service) Sync(ctx context.Context, providerID uint) ([]Instance, error) {
	p, err := s.store.GetProvider(providerID)
	if err != nil {
		return nil, err
	}
	drv, ok := s.driver(p.Kind)
	if !ok {
		return nil, fmt.Errorf("driver %s not registered", p.Kind)
	}
	creds, err := s.creds(p)
	if err != nil {
		return nil, err
	}
	insts, err := drv.ListInstances(ctx, creds)
	if err != nil {
		_ = s.store.SetProviderState(providerID, "invalid")
		return nil, err
	}
	_ = s.store.SetProviderState(providerID, "valid")
	rows, err := instanceRows(providerID, insts)
	if err != nil {
		return nil, err
	}
	if err := s.store.SyncProviderInstances(providerID, rows); err != nil {
		return nil, err
	}
	s.store.Bus.PublishChange("provider", fmt.Sprint(providerID), "synced")
	return insts, nil
}

// Install injects the agent onto a provider instance over ssh/exec.
func (s *Service) Install(ctx context.Context, providerID uint, externalID, serverURL string, opts InstallOptions) (InstallResult, error) {
	p, drv, creds, target, err := s.installTarget(ctx, providerID, externalID)
	if err != nil {
		return InstallResult{}, err
	}
	candidates, err := SSHInstallCandidates(*target, opts.SSHConnectionID)
	if err != nil {
		if target.Power == PowerStopped {
			return InstallResult{}, stoppedInstanceError(*target)
		}
		if _, asked := drv.(InstanceGetter); asked && len(target.SSHConnections) > 0 && !hasInstallableSSH(*target) {
			// Asked again and still none: an instance starting or restarting
			// has no direct SSH yet.
			err = fmt.Errorf("%w; the provider reports no direct SSH endpoint for it right now (an instance that is starting or restarting has none yet), try again shortly", err)
		}
		return InstallResult{}, err
	}

	mode := normalizeInstallConnectMode(opts.ConnectMode)
	candidates = ServerDialCandidates(candidates, mode, opts.SSHConnectionID)
	if mode == "server_dial" && !ServerDialSupported(p.Kind) {
		return InstallResult{}, fmt.Errorf("%s instances cannot be dialed by the server (they open HTTP ports only); install with the agent connecting to the server", p.Kind)
	}
	listen := normalizeServerDialListen(opts.ServerDialListen)
	if mode == "server_dial" && strings.TrimSpace(opts.ServerDialListen) == "" && p.Kind == "compshare" {
		listen = ":3389"
	}
	hostAddress := strings.TrimSpace(opts.HostAddress)
	if hostAddress == "" {
		hostAddress = strings.TrimSpace(target.PrivateIP)
		if hostAddress == "" {
			hostAddress = strings.TrimSpace(target.PublicIP)
		}
	}
	serverDialAddress := opts.ServerDialAddress
	// A provider that maps container ports chooses the agent's port and its
	// address itself when the operator gives no address (InstallReport).
	autoPort := mode == "server_dial" && MapsServerDialPorts(drv, *target) && strings.TrimSpace(serverDialAddress) == ""
	if mode == "server_dial" && strings.TrimSpace(serverDialAddress) == "" && p.Kind == "compshare" && !autoPort {
		serverDialAddress = target.PublicIP
	}
	if mode == "server_dial" && strings.TrimSpace(serverDialAddress) == "" && !autoPort {
		serverDialAddress = hostAddress
	}
	if strings.TrimSpace(serverDialAddress) != "" {
		serverDialAddress, err = nodeaddr.Endpoint(serverDialAddress)
		if err != nil {
			return InstallResult{}, fmt.Errorf("invalid server_dial_address: %w", err)
		}
	}
	pending, token, err := s.store.CreateNodePending(target.Name, mode)
	if err != nil {
		return InstallResult{}, fmt.Errorf("create pending node: %w", err)
	}
	if err := s.store.LinkProviderInstanceNode(providerID, externalID, pending.ID); err != nil {
		_ = s.store.DeleteNode(pending.UUID)
		return InstallResult{}, fmt.Errorf("link provider instance to node: %w", err)
	}
	if err := s.store.SetNodeServerURL(pending.UUID, serverURL); err != nil {
		_ = s.store.DeleteNode(pending.UUID)
		return InstallResult{}, fmt.Errorf("persist server URL: %w", err)
	}
	if mode == "server_dial" && strings.TrimSpace(serverDialAddress) != "" {
		if err := s.store.SetNodeServerDialAddress(pending.UUID, providerServerDialAddress(*target, serverDialAddress, listen)); err != nil {
			_ = s.store.DeleteNode(pending.UUID)
			return InstallResult{}, fmt.Errorf("persist server_dial address: %w", err)
		}
	}
	if err := s.store.SetNodeServerDialListen(pending.UUID, listen); err != nil {
		_ = s.store.DeleteNode(pending.UUID)
		return InstallResult{}, fmt.Errorf("persist server_dial listen address: %w", err)
	}
	access := s.prepareSSHAccess(ctx, drv, creds, externalID)
	req := InstallRequest{
		InstanceKind: target.Kind, ServerURL: serverURL, Token: token, SSHKeyPEM: opts.SSHKeyPEM,
		Password: opts.Password, PlatformKey: access.platformKey, InstancePassword: access.instancePassword,
		ConnectMode: mode, ServerDialListen: listen,
		ServerDialAddress:    serverDialAddress,
		AllowInstanceRestart: opts.AllowInstanceRestart,
		InstallDir:           strings.TrimRight(strings.TrimSpace(opts.InstallDir), "/"),
		NoSystemd:            opts.NoSystemd,
		ReadAgentBinary:      s.readAgentBinary,
	}
	var installReport InstallReport
	var selected *SSHConnection
	attempts := make([]SSHAttempt, 0, len(candidates))
	autoSelection := strings.TrimSpace(opts.SSHConnectionID) == "" || strings.TrimSpace(opts.SSHConnectionID) == "auto"
	for index := range candidates {
		candidate := candidates[index]
		req.Instance = ApplySSHConnection(*target, candidate)
		req.Connection = candidate
		if reporting, ok := drv.(ReportingDriver); ok {
			installReport, err = reporting.InstallAgentWithReport(ctx, creds, req)
		} else {
			err = drv.InstallAgent(ctx, creds, req)
		}
		attempt := SSHAttempt{ConnectionID: candidate.ID, Target: SSHConnectionTarget(candidate)}
		if err == nil {
			attempt.Status = "selected"
			attempts = append(attempts, attempt)
			selected = &candidate
			break
		}
		attempt.Status = "failed"
		attempt.Detail = err.Error()
		attempts = append(attempts, attempt)
		if !autoSelection || !IsRetryableSSHConnectionError(err) || index == len(candidates)-1 {
			break
		}
	}
	if err != nil {
		_ = s.store.DeleteNode(pending.UUID)
		via := "SSH connection " + attempts[len(attempts)-1].Target
		if last := candidates[len(attempts)-1]; !IsSSHConnection(last) {
			via = last.Type
		}
		return InstallResult{}, fmt.Errorf("install through %s: %w", via, err)
	}
	if selected != nil && IsSSHConnection(*selected) {
		secrets := installedSecrets(installReport, opts, access)
		// Empty secrets are kept too when the platform key works: recovery then
		// logs in with it.
		if !secrets.Empty() || installReport.PlatformKeyAuthorized || installReport.AuthUsed == provision.AuthPlatformKey {
			host, port := selected.Host, selected.Port
			if installReport.SSHHost != "" && installReport.SSHPort != 0 {
				host, port = installReport.SSHHost, installReport.SSHPort
			}
			if _, credentialErr := s.store.SetNodeSSHCredential(pending.UUID, host, port, sshFirstNonEmpty(selected.User, "root"), secrets); credentialErr != nil {
				return InstallResult{}, fmt.Errorf("persist node SSH credential: %w", credentialErr)
			}
		}
	}
	// Recorded whatever the channel: SSH credentials added to the node later
	// recover the agent from this directory too.
	if dir := strings.TrimSpace(installReport.InstallDir); dir != "" {
		if err := s.store.SetNodeInstallDir(pending.UUID, dir); err != nil {
			return InstallResult{}, fmt.Errorf("persist node install directory: %w", err)
		}
	}
	if autoPort {
		if installReport.ServerDialAddress == "" || installReport.ServerDialListen == "" {
			return InstallResult{}, fmt.Errorf("%s did not say where the server reaches the agent it installed", p.Kind)
		}
		listen, serverDialAddress = installReport.ServerDialListen, installReport.ServerDialAddress
		if err := s.store.SetNodeServerDialListen(pending.UUID, listen); err != nil {
			return InstallResult{}, fmt.Errorf("persist server_dial listen address: %w", err)
		}
		if err := s.store.SetNodeServerDialAddress(pending.UUID, serverDialAddress); err != nil {
			return InstallResult{}, fmt.Errorf("persist server_dial address: %w", err)
		}
	}
	result := InstallResult{
		Token: token, NodeUUID: pending.UUID, Instance: *target, ConnectMode: mode, ServerDialListen: listen,
		InstallChecks: append(access.checks, installReport.Checks...), SSHConnection: selected, SSHAttempts: attempts,
	}
	if mode == "server_dial" {
		address := hostAddress
		if serverDialAddress != "" {
			address = serverDialAddress
		}
		result.ServerDialAddress = providerServerDialAddress(*target, address, listen)
	}
	return result, nil
}

func (s *Service) installTarget(ctx context.Context, providerID uint, externalID string) (*store.Provider, Driver, Credentials, *Instance, error) {
	p, err := s.store.GetProvider(providerID)
	if err != nil {
		return nil, nil, Credentials{}, nil, err
	}
	drv, ok := s.driver(p.Kind)
	if !ok {
		return nil, nil, Credentials{}, nil, fmt.Errorf("driver %s not registered", p.Kind)
	}
	creds, err := s.creds(p)
	if err != nil {
		return nil, nil, Credentials{}, nil, err
	}
	insts, err := drv.ListInstances(ctx, creds)
	if err != nil {
		return nil, nil, Credentials{}, nil, err
	}
	for i := range insts {
		if insts[i].ExternalID == externalID {
			target := insts[i]
			if target.Cluster != "" {
				return nil, nil, Credentials{}, nil, &InstanceInClusterError{Cluster: target.Cluster}
			}
			if getter, ok := drv.(InstanceGetter); ok && target.Power != PowerStopped {
				target = s.detailUntilSSH(ctx, getter, creds, target)
			}
			return p, drv, creds, &target, nil
		}
	}
	return nil, nil, Credentials{}, nil, fmt.Errorf("instance %s not found", externalID)
}

// detailAttempts and detailRetryDelay bound how long an install waits for a
// listed instance without an SSH endpoint to report one. RunPod has now and
// then listed and described a running pod without its ports for a few
// seconds (2026-10-20, not reproduced since).
const detailAttempts = 3

var detailRetryDelay = 2 * time.Second

// detailUntilSSH fills in a listed instance that offers no SSH endpoint to
// install over from the provider's description of it, asking again a few
// times while that has none either.
func (s *Service) detailUntilSSH(ctx context.Context, getter InstanceGetter, creds Credentials, target Instance) Instance {
	for attempt := 0; attempt < detailAttempts; attempt++ {
		if _, err := SSHInstallCandidates(target, "auto"); err == nil {
			return target
		}
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return target
			case <-time.After(detailRetryDelay):
			}
		}
		if detailed, err := getter.GetInstance(ctx, creds, target.ExternalID); err == nil {
			target = mergeInstanceDetail(target, detailed)
		}
	}
	return target
}

// stoppedInstanceError explains an instance without an SSH endpoint that the
// provider reports stopped.
func stoppedInstanceError(target Instance) error {
	return fmt.Errorf("instance %q is stopped at the provider, so it has no SSH endpoint; start it there, then install again", target.ExternalID)
}

// hasInstallableSSH reports whether any listed connection supports a
// one-click install.
func hasInstallableSSH(inst Instance) bool {
	for _, connection := range inst.SSHConnections {
		if connection.InstallSupported {
			return true
		}
	}
	return false
}

func mergeInstanceDetail(listed, detailed Instance) Instance {
	if detailed.ExternalID == "" {
		detailed.ExternalID = listed.ExternalID
	}
	if detailed.Name == "" {
		detailed.Name = listed.Name
	}
	if detailed.GPUModel == "" {
		detailed.GPUModel = listed.GPUModel
	}
	if detailed.GPUCount == 0 {
		detailed.GPUCount = listed.GPUCount
	}
	if detailed.Power == "" {
		detailed.Power = listed.Power
	}
	if detailed.PricePerHr == 0 {
		detailed.PricePerHr = listed.PricePerHr
	}
	if detailed.Region == "" {
		detailed.Region = listed.Region
	}
	if detailed.Zone == "" {
		detailed.Zone = listed.Zone
	}
	if detailed.Kind == "" {
		detailed.Kind = listed.Kind
	}
	return detailed
}

func normalizeInstallConnectMode(mode string) string {
	if mode == "server_dial" {
		return "server_dial"
	}
	return "agent_dial"
}

func normalizeServerDialListen(listen string) string {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return provision.DefaultServerDialListen
	}
	return listen
}

// providerServerDialAddress is where the server dials an instance's agent:
// the address the operator gave, else the instance's public IP, with the
// listen port when the address names none (provision.ServerDialAddress).
func providerServerDialAddress(inst Instance, hostAddress, listen string) string {
	address := strings.TrimSpace(hostAddress)
	if address == "" {
		address = strings.TrimSpace(inst.PublicIP)
	}
	return provision.ServerDialAddress(address, normalizeServerDialListen(listen))
}
