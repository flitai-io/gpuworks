package provider

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/localization"
)

// keyTestDriver can authorize public keys and hand out instance passwords,
// each of which a test can make fail, and reports how it logged in.
type keyTestDriver struct {
	serviceTestDriver
	name         string
	host         string
	port         int
	authorizeErr error
	passwordErr  error
	password     string
	report       InstallReport

	authorized []string // externalIDs AuthorizePublicKey was called with
	created    bool
	req        InstallRequest
}

func (d *keyTestDriver) Name() string { return d.name }

func (d *keyTestDriver) ListInstances(context.Context, Credentials) ([]Instance, error) {
	return []Instance{{ExternalID: "inst-1", Name: "gpu", PublicIP: d.host, SSHConnections: []SSHConnection{
		{ID: "direct", Type: "direct", Label: localization.SameText("Direct", requiredProviderLocales...), Host: d.host, Port: d.port, User: "root", InstallSupported: true},
	}}}, nil
}

func (d *keyTestDriver) AuthorizePublicKey(_ context.Context, _ Credentials, externalID, publicKey string) (string, error) {
	if !strings.HasPrefix(publicKey, "ssh-ed25519 ") {
		return "", errors.New("not the platform public key: " + publicKey)
	}
	if externalID == "" && d.created {
		return "", errors.New("authorized after the instance was created")
	}
	d.authorized = append(d.authorized, externalID)
	return "registered with the provider", d.authorizeErr
}

func (d *keyTestDriver) InstancePassword(context.Context, Credentials, string) (string, error) {
	return d.password, d.passwordErr
}

func (d *keyTestDriver) CreateInstance(context.Context, Credentials, CreateInstanceRequest) (Instance, error) {
	d.created = true
	return Instance{ExternalID: "new"}, nil
}

func (d *keyTestDriver) InstallAgentWithReport(_ context.Context, _ Credentials, req InstallRequest) (InstallReport, error) {
	d.req = req
	return d.report, nil
}

func (d *keyTestDriver) InstallAgent(ctx context.Context, creds Credentials, req InstallRequest) error {
	_, err := d.InstallAgentWithReport(ctx, creds, req)
	return err
}

func newKeyService(t *testing.T, d *keyTestDriver, withKey bool) (*Service, *store.Store, uint, provision.PlatformKey) {
	t.Helper()
	Register(d)
	st, err := store.Open("file:"+d.name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, err := auth.NewSecretbox(d.name)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, box)
	key, err := provision.GeneratePlatformKey("gpuworks@test")
	if err != nil {
		t.Fatal(err)
	}
	if withKey {
		if _, err := st.SetPlatformSSHKey(key.PrivatePEM, key.PublicKey, key.Fingerprint); err != nil {
			t.Fatal(err)
		}
	}
	p, err := svc.Add(t.Context(), d.name, d.name, Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	return svc, st, p.ID, key
}

func checkNamed(checks []InstallCheck, name string) *InstallCheck {
	for i := range checks {
		if checks[i].Name == name {
			return &checks[i]
		}
	}
	return nil
}

// Nothing from the operator: the provider authorizes the platform key, the
// instance password is fetched, and recovery keeps only the platform key.
func TestCloudInstallNeedsNoCredentialFromTheOperator(t *testing.T) {
	d := &keyTestDriver{name: "key-test-auto", host: "203.0.113.20", port: 22, password: "instance-pw",
		report: InstallReport{AuthUsed: provision.AuthInstancePassword, PlatformKeyAuthorized: true, Checks: []InstallCheck{{Name: "SSH authentication", Status: "ok"}}}}
	svc, st, providerID, key := newKeyService(t, d, true)
	result, err := svc.Install(t.Context(), providerID, "inst-1", "https://server.example", InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d.req.PlatformKey == nil || d.req.PlatformKey.Fingerprint != key.Fingerprint || d.req.InstancePassword != "instance-pw" || d.req.SSHKeyPEM != "" || d.req.Password != "" {
		t.Fatalf("install request auth = key:%v ipw:%q", d.req.PlatformKey, d.req.InstancePassword)
	}
	// Once when the provider was added (account keys, no instance), then
	// for the instance being installed.
	if strings.Join(d.authorized, ",") != ",inst-1" {
		t.Fatalf("authorized = %v", d.authorized)
	}
	if got := result.InstallChecks; len(got) != 3 || got[0].Name != "Platform key (provider)" || got[0].Detail != "registered with the provider" || got[1].Name != "Instance password" || got[1].Detail != "fetched from the provider API" || got[2].Name != "SSH authentication" {
		t.Fatalf("checks = %+v", got)
	}
	cred, secrets, err := st.GetNodeSSHCredential(result.NodeUUID)
	if err != nil || !secrets.Empty() || cred.Host != "203.0.113.20" || cred.User != "root" {
		t.Fatalf("credential = %+v secrets=%+v err=%v", cred, secrets, err)
	}

	// The auth list the driver builds puts the platform key first.
	auth := d.req.SSHAuth()
	if auth.PlatformKey == nil || auth.InstancePassword != "instance-pw" || auth.Validate() != nil {
		t.Fatalf("SSHAuth = %+v", auth)
	}
}

// Each preparation step can fail on its own; the install goes on with what
// is left, and recovery keeps the credential that worked.
func TestCloudInstallPreparationFailuresAreReportedNotFatal(t *testing.T) {
	d := &keyTestDriver{name: "key-test-degraded", host: "203.0.113.21", port: 22,
		authorizeErr: errors.New("provider API: 403 forbidden"), passwordErr: errors.New("describe instance: timeout"),
		report: InstallReport{AuthUsed: provision.AuthPassword}}
	svc, st, providerID, _ := newKeyService(t, d, true)
	result, err := svc.Install(t.Context(), providerID, "inst-1", "https://server.example", InstallOptions{Password: "operator-pw"})
	if err != nil {
		t.Fatal(err)
	}
	if c := checkNamed(result.InstallChecks, "Platform key (provider)"); c == nil || c.Status != "warn" || !strings.Contains(c.Detail, "403") {
		t.Fatalf("provider key check = %+v", c)
	}
	if c := checkNamed(result.InstallChecks, "Instance password"); c == nil || c.Status != "warn" || !strings.Contains(c.Detail, "timeout") {
		t.Fatalf("password check = %+v", c)
	}
	if _, secrets, err := st.GetNodeSSHCredential(result.NodeUUID); err != nil || secrets.Password != "operator-pw" || secrets.PrivateKeyPEM != "" {
		t.Fatalf("secrets = %+v err=%v", secrets, err)
	}
}

func TestCloudInstallWithoutAPlatformKeySaysSo(t *testing.T) {
	d := &keyTestDriver{name: "key-test-nokey", host: "203.0.113.22", port: 22, report: InstallReport{AuthUsed: provision.AuthPrivateKey}}
	svc, st, providerID, _ := newKeyService(t, d, false)
	result, err := svc.Install(t.Context(), providerID, "inst-1", "https://server.example", InstallOptions{SSHKeyPEM: "KEY"})
	if err != nil {
		t.Fatal(err)
	}
	if c := checkNamed(result.InstallChecks, "Platform key (provider)"); c == nil || c.Status != "warn" || !strings.Contains(c.Detail, "not available") {
		t.Fatalf("check = %+v", c)
	}
	if len(d.authorized) != 0 || d.req.PlatformKey != nil {
		t.Fatalf("authorized=%v key=%v", d.authorized, d.req.PlatformKey)
	}
	if _, secrets, _ := st.GetNodeSSHCredential(result.NodeUUID); secrets.PrivateKeyPEM != "KEY" {
		t.Fatalf("secrets = %+v", secrets)
	}
}

// RunPod writes account keys into a pod only when it starts, so the key is
// registered before the pod is created.
func TestCreateInstanceRegistersThePlatformKeyFirst(t *testing.T) {
	d := &keyTestDriver{name: "key-test-create"}
	svc, _, providerID, _ := newKeyService(t, d, true)
	if _, err := svc.CreateInstance(t.Context(), providerID, CreateInstanceRequest{}); err != nil {
		t.Fatal(err)
	}
	// Once when the provider was added, again before the pod is created.
	if len(d.authorized) != 2 || d.authorized[0] != "" || d.authorized[1] != "" || !d.created {
		t.Fatalf("authorized=%v created=%v", d.authorized, d.created)
	}
	// A provider refusing the key does not stop the instance being created.
	d2 := &keyTestDriver{name: "key-test-create-fail", authorizeErr: errors.New("quota")}
	svc2, _, id2, _ := newKeyService(t, d2, true)
	if _, err := svc2.CreateInstance(t.Context(), id2, CreateInstanceRequest{}); err != nil || !d2.created {
		t.Fatalf("create with a refused key: err=%v created=%v", err, d2.created)
	}
}

// The precheck reports the preparation rows next to its SSH result, also when
// SSH itself fails.
func TestCloudPrecheckShowsPreparationRows(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close() // nothing listens: the dial is refused at once
	d := &keyTestDriver{name: "key-test-precheck", host: "127.0.0.1", port: port, password: "instance-pw"}
	svc, _, providerID, _ := newKeyService(t, d, true)
	report, err := svc.Precheck(t.Context(), providerID, "inst-1", "https://server.example", InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(report.Checks))
	for _, c := range report.Checks {
		names = append(names, c.Name+":"+c.Status)
	}
	if strings.Join(names, ",") != "Platform key (provider):ok,Instance password:ok,SSH:error" {
		t.Fatalf("checks = %v (%+v)", names, report.Checks)
	}
	if !strings.Contains(report.Checks[2].Detail, "ssh dial") {
		t.Fatalf("SSH detail = %q", report.Checks[2].Detail)
	}
}

// Where the SSH install put the agent is recorded for recovery; a driver that
// used the install script's default leaves it empty.
func TestCloudInstallRecordsTheInstallDirectory(t *testing.T) {
	for _, tc := range []struct{ name, dir string }{
		{name: "key-test-dir-reported", dir: "/root/gpuworks"},
		{name: "key-test-dir-default", dir: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &keyTestDriver{name: tc.name, host: "203.0.113.21", port: 22,
				report: InstallReport{AuthUsed: provision.AuthPlatformKey, PlatformKeyAuthorized: true, InstallDir: tc.dir}}
			svc, st, providerID, _ := newKeyService(t, d, true)
			result, err := svc.Install(t.Context(), providerID, "inst-1", "https://server.example", InstallOptions{})
			if err != nil {
				t.Fatal(err)
			}
			node, err := st.NodeByUUID(result.NodeUUID)
			if err != nil || node.InstallDir != tc.dir {
				t.Fatalf("install dir = %q err=%v, want %q", node.InstallDir, err, tc.dir)
			}
		})
	}
}

func TestReportFromSSHInstallKeepsTheInstallDirectory(t *testing.T) {
	if got := ReportFromSSHInstall(provision.SSHInstallReport{InstallDir: "/tmp/gpuworks"}); got.InstallDir != "/tmp/gpuworks" {
		t.Fatalf("install dir = %q", got.InstallDir)
	}
}

// A driver that restarted the instance (a RunPod pod edit renumbers SSH)
// says where it logged in; the node's credential points there.
func TestCloudInstallRecordsTheSSHEndpointTheDriverLoggedInTo(t *testing.T) {
	d := &keyTestDriver{name: "key-test-renumbered", host: "203.0.113.20", port: 32015,
		report: InstallReport{AuthUsed: provision.AuthPlatformKey, PlatformKeyAuthorized: true, SSHHost: "203.0.113.20", SSHPort: 32075}}
	svc, st, providerID, _ := newKeyService(t, d, true)
	result, err := svc.Install(t.Context(), providerID, "inst-1", "https://server.example", InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cred, _, err := st.GetNodeSSHCredential(result.NodeUUID)
	if err != nil || cred.Host != "203.0.113.20" || cred.Port != 32075 {
		t.Fatalf("credential = %+v, %v; want the renumbered port", cred, err)
	}
}

// An instance that forgets the keys added to it when it restarts (a RunPod
// pod) keeps the credential that logged in: recovery after the restart has
// nothing else, since the platform key is gone with the container disk.
func TestVolatilePlatformKeyKeepsTheInstallCredential(t *testing.T) {
	d := &keyTestDriver{name: "key-test-volatile", host: "203.0.113.23", port: 22,
		report: InstallReport{AuthUsed: provision.AuthPrivateKey, PlatformKeyAuthorized: true, PlatformKeyVolatile: true}}
	svc, st, providerID, _ := newKeyService(t, d, true)
	result, err := svc.Install(t.Context(), providerID, "inst-1", "https://server.example", InstallOptions{SSHKeyPEM: "RENT-KEY"})
	if err != nil {
		t.Fatal(err)
	}
	if _, secrets, err := st.GetNodeSSHCredential(result.NodeUUID); err != nil || secrets.PrivateKeyPEM != "RENT-KEY" || secrets.Password != "" {
		t.Fatalf("secrets = %+v err=%v", secrets, err)
	}

	// Durably authorized: only the platform key stays, nothing of the operator's.
	d2 := &keyTestDriver{name: "key-test-durable", host: "203.0.113.24", port: 22,
		report: InstallReport{AuthUsed: provision.AuthPrivateKey, PlatformKeyAuthorized: true}}
	svc2, st2, id2, _ := newKeyService(t, d2, true)
	result2, err := svc2.Install(t.Context(), id2, "inst-1", "https://server.example", InstallOptions{SSHKeyPEM: "RENT-KEY"})
	if err != nil {
		t.Fatal(err)
	}
	if _, secrets, err := st2.GetNodeSSHCredential(result2.NodeUUID); err != nil || !secrets.Empty() {
		t.Fatalf("secrets = %+v err=%v", secrets, err)
	}
}

func TestInstalledSecretsKeepOnlyTheKeyboardInteractivePassword(t *testing.T) {
	opts := InstallOptions{Password: "operator-pw", SSHKeyPEM: "unused-private-key"}
	for _, tc := range []struct {
		name   string
		report InstallReport
		want   store.NodeSSHSecrets
	}{
		{
			name:   "authorization failed",
			report: InstallReport{AuthUsed: provision.AuthKeyboardInteractive},
			want:   store.NodeSSHSecrets{Password: "operator-pw"},
		},
		{
			name:   "platform key durably authorized",
			report: InstallReport{AuthUsed: provision.AuthKeyboardInteractive, PlatformKeyAuthorized: true},
			want:   store.NodeSSHSecrets{},
		},
		{
			name:   "platform key volatile",
			report: InstallReport{AuthUsed: provision.AuthKeyboardInteractive, PlatformKeyAuthorized: true, PlatformKeyVolatile: true},
			want:   store.NodeSSHSecrets{Password: "operator-pw"},
		},
		{
			name:   "legacy driver without auth outcome",
			report: InstallReport{},
			want:   store.NodeSSHSecrets{Password: "operator-pw", PrivateKeyPEM: "unused-private-key"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := installedSecrets(tc.report, opts, sshAccess{instancePassword: "instance-pw"}); got != tc.want {
				t.Fatalf("retained credentials = %+v, want %+v", got, tc.want)
			}
		})
	}
}
