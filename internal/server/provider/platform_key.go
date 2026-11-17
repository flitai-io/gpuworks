package provider

import (
	"context"
	"log/slog"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// PlatformKey is the server's SSH key as drivers see it; the same fields as
// provision.PlatformKey, so a driver converts it directly.
type PlatformKey struct {
	PrivatePEM  string
	PublicKey   string
	Fingerprint string
}

// PublicKeyAuthorizer is an optional driver capability: make the provider
// put a public key on an instance. RunPod registers it with the account
// (externalID is ignored; pods started afterwards get it), Vast attaches it to
// the instance. The returned check says what was done, for the install
// report (provision.CheckIDPlatformKeyProvider); a zero check when there was
// nothing to do.
type PublicKeyAuthorizer interface {
	AuthorizePublicKey(ctx context.Context, creds Credentials, externalID, publicKey string) (provision.CheckResult, error)
}

// PlatformKeyAccountCheck is the row of a provider that registers the
// platform key with the account (RunPod, DigitalOcean): added says it was not
// there before; instances started from now on accept it.
func PlatformKeyAccountCheck(providerName string, added bool, message string) provision.CheckResult {
	code := messages.CheckPlatformKeyAccountPresent
	if added {
		code = messages.CheckPlatformKeyAccountAdded
	}
	return provision.PassCheck(provision.CheckIDPlatformKeyProvider, code, message, map[string]any{"provider": providerName})
}

// PlatformKeyInstanceCheck is the row of a provider that put the platform key
// on the instance itself (Vast, ZStack Cloud hosts).
func PlatformKeyInstanceCheck(providerName, message string) provision.CheckResult {
	return provision.PassCheck(provision.CheckIDPlatformKeyProvider, messages.CheckPlatformKeyInstanceAttached, message, map[string]any{"provider": providerName})
}

// InstallKeyAdder is an optional driver capability: the install adds the
// platform key to an instance that does not accept it yet (Alibaba Cloud
// through Cloud Assistant, RunPod by editing the pod). A precheck whose SSH
// login the instance refused asks the driver, read-only, whether the install
// will, so the precheck does not report a failure the install fixes.
type InstallKeyAdder interface {
	// PlatformKeyAtInstall is the precheck's SSH row when the install would
	// add publicKey to inst; ok is false when it could not.
	PlatformKeyAtInstall(ctx context.Context, creds Credentials, inst Instance, publicKey string) (check provision.CheckResult, ok bool)
}

// InstancePasswordSource is an optional driver capability: the root password
// the provider's API issued for an instance (CompShare, AutoDL). The server
// logs in with it once and authorizes the platform key.
type InstancePasswordSource interface {
	InstancePassword(ctx context.Context, creds Credentials, externalID string) (string, error)
}

// SSHAuth is the provision credential list for this install: the platform
// key first, then what the operator gave, then the provider's password.
func (r InstallRequest) SSHAuth() provision.SSHAuth {
	auth := provision.SSHAuth{PrivateKeyPEM: r.SSHKeyPEM, Password: r.Password, InstancePassword: r.InstancePassword}
	if r.PlatformKey != nil {
		key := provision.PlatformKey(*r.PlatformKey)
		auth.PlatformKey = &key
	}
	return auth
}

// ReportFromSSHInstall converts an SSH installer report for drivers that
// install through provision.
func ReportFromSSHInstall(report provision.SSHInstallReport) InstallReport {
	return InstallReport{Checks: report.Checks, AuthUsed: report.AuthUsed, PlatformKeyAuthorized: report.PlatformKeyAuthorized, InstallDir: report.InstallDir}
}

func (s *Service) platformKey() *PlatformKey {
	row, pem, err := s.store.PlatformSSHKey()
	if err != nil {
		return nil
	}
	return &PlatformKey{PrivatePEM: pem, PublicKey: row.PublicKey, Fingerprint: row.Fingerprint}
}

// sshAccess is everything the server can bring to an instance's SSH login
// without asking the operator, with a report row per step.
type sshAccess struct {
	platformKey      *PlatformKey
	instancePassword string
	checks           []provision.CheckResult
}

// prepareSSHAccess asks the provider to authorize the platform key and fetches
// the instance's password, so an install or precheck needs no credential from
// the operator. Each step is best effort: a failure becomes a warning row, and
// the login still tries whatever else is available.
func (s *Service) prepareSSHAccess(ctx context.Context, drv Driver, creds Credentials, externalID string) sshAccess {
	var access sshAccess
	access.platformKey = s.platformKey()
	if access.platformKey == nil {
		access.checks = append(access.checks, provision.SkipCheck(provision.CheckIDPlatformKeyProvider, messages.CheckPlatformKeyUnavailable, "the platform SSH key is not available", nil))
	} else if authorizer, ok := drv.(PublicKeyAuthorizer); ok {
		check, err := authorizer.AuthorizePublicKey(ctx, creds, externalID, access.platformKey.PublicKey)
		if err != nil {
			check = provision.WarnCheck(provision.CheckIDPlatformKeyProvider, messages.CheckPlatformKeyProviderFailed, "the provider could not authorize the platform key", nil).WithEvidence(err.Error())
		}
		if check.ID != "" {
			access.checks = append(access.checks, check)
		}
	}
	if source, ok := drv.(InstancePasswordSource); ok {
		password, err := source.InstancePassword(ctx, creds, externalID)
		switch {
		case err != nil:
			access.checks = append(access.checks, provision.WarnCheck(provision.CheckIDInstancePassword, messages.CheckInstancePasswordFailed, "the instance password could not be fetched", nil).WithEvidence(err.Error()))
		case password != "":
			access.instancePassword = password
			access.checks = append(access.checks, provision.PassCheck(provision.CheckIDInstancePassword, messages.CheckInstancePasswordFetched, "fetched from the provider API", nil))
		}
	}
	if access.instancePassword == "" && creds.Extra != nil {
		// AutoDL accounts may store one root password for all instances.
		access.instancePassword = strings.TrimSpace(creds.Extra["ssh_password"])
	}
	return access
}

// authorizeBeforeCreate registers the platform key with providers whose
// instances pick up keys only when they start (RunPod account keys), so a
// pod created next accepts the server without any operator step.
func authorizeBeforeCreate(ctx context.Context, s *Service, drv Driver, creds Credentials) {
	authorizer, ok := drv.(PublicKeyAuthorizer)
	if !ok {
		return
	}
	key := s.platformKey()
	if key == nil {
		return
	}
	if _, err := authorizer.AuthorizePublicKey(ctx, creds, "", key.PublicKey); err != nil {
		slog.Default().Warn("authorize platform ssh key before creating an instance", "err", err)
	}
}

// installedSecrets is what out-of-band recovery keeps after a cloud install:
// nothing once the instance durably accepts the platform key, otherwise the
// credential that worked.
func installedSecrets(report InstallReport, opts InstallOptions, access sshAccess) store.NodeSSHSecrets {
	if report.PlatformKeyAuthorized && !report.PlatformKeyVolatile {
		return store.NodeSSHSecrets{}
	}
	switch report.AuthUsed {
	case provision.AuthPassword, provision.AuthKeyboardInteractive:
		return store.NodeSSHSecrets{Password: opts.Password}
	case provision.AuthPrivateKey:
		return store.NodeSSHSecrets{PrivateKeyPEM: opts.SSHKeyPEM}
	case provision.AuthInstancePassword:
		return store.NodeSSHSecrets{Password: access.instancePassword}
	case provision.AuthPlatformKey:
		return store.NodeSSHSecrets{}
	}
	// A driver that does not report how it logged in.
	password := opts.Password
	if password == "" {
		password = access.instancePassword
	}
	return store.NodeSSHSecrets{Password: password, PrivateKeyPEM: opts.SSHKeyPEM}
}
