package provider

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"gpuworks.ai/gpuworks/internal/server/provision"
)

// keyAdderDriver says whether its install would add the platform key.
type keyAdderDriver struct {
	keyTestDriver
	adds  bool
	asked []string // public keys PlatformKeyAtInstall was asked about
}

func (d *keyAdderDriver) PlatformKeyAtInstall(_ context.Context, _ Credentials, inst Instance, publicKey string) (provision.CheckResult, bool) {
	d.asked = append(d.asked, inst.ExternalID+" "+publicKey)
	if !d.adds {
		return provision.CheckResult{}, false
	}
	return provision.WarnCheck(provision.CheckIDSSH, "CHECK_PLATFORM_KEY_ASSISTANT_PENDING", "the install adds the platform key", nil), true
}

// rejectingSSHServer completes the SSH handshake and refuses every login.
func rejectingSSHServer(t *testing.T) (string, int) {
	t.Helper()
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return nil, errors.New("public key rejected")
		},
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			return nil, errors.New("password rejected")
		},
	}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _, _, _ = ssh.NewServerConn(conn, cfg)
			}()
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

// A login the instance refuses is not a precheck failure when the install
// adds the platform key itself; it stays one when the install cannot.
func TestPrecheckSSHRowSaysTheInstallAddsThePlatformKey(t *testing.T) {
	for _, tc := range []struct {
		name   string
		adds   bool
		status string
	}{
		{name: "key-test-adder-adds", adds: true, status: "warn"},
		{name: "key-test-adder-cannot", adds: false, status: "fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, port := rejectingSSHServer(t)
			d := &keyAdderDriver{keyTestDriver: keyTestDriver{name: tc.name, host: host, port: port}, adds: tc.adds}
			svc, _, providerID, _ := newKeyService(t, &d.keyTestDriver, true)
			Register(d) // the service resolves the driver by name: use the adder
			report, err := svc.Precheck(t.Context(), providerID, "inst-1", "https://server.example", InstallOptions{})
			if err != nil {
				t.Fatal(err)
			}
			last := report.Checks[len(report.Checks)-1]
			if last.ID != "ssh" || last.Status != tc.status {
				t.Fatalf("checks = %+v", report.Checks)
			}
			if tc.adds && checkText(last) != "the install adds the platform key" {
				t.Fatalf("SSH detail = %q", checkText(last))
			}
			if !tc.adds && !strings.Contains(checkText(last), "unable to authenticate") {
				t.Fatalf("SSH detail = %q", checkText(last))
			}
			if len(d.asked) != 1 || !strings.HasPrefix(d.asked[0], "inst-1 ssh-ed25519 ") {
				t.Fatalf("asked = %v", d.asked)
			}
		})
	}
}

// A dial that never reaches authentication is not something adding a key
// fixes: the driver is not asked.
func TestPrecheckDoesNotOfferTheKeyForAnUnreachableInstance(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	d := &keyAdderDriver{keyTestDriver: keyTestDriver{name: "key-test-adder-unreachable", host: "127.0.0.1", port: port}, adds: true}
	svc, _, providerID, _ := newKeyService(t, &d.keyTestDriver, true)
	Register(d)
	report, err := svc.Precheck(t.Context(), providerID, "inst-1", "https://server.example", InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if last := report.Checks[len(report.Checks)-1]; last.ID != "ssh" || last.Status != "fail" || len(d.asked) != 0 {
		t.Fatalf("checks = %+v asked = %v", report.Checks, d.asked)
	}
}

// flakyDetailDriver lists its instance without direct SSH, as RunPod did for
// a few seconds once, and describes it with SSH from the given call on.
type flakyDetailDriver struct {
	keyTestDriver
	sshFromCall int // 0: never
	gets        int
	stopped     bool
}

func (d *flakyDetailDriver) ListInstances(context.Context, Credentials) ([]Instance, error) {
	return []Instance{d.instance(false)}, nil
}

func (d *flakyDetailDriver) GetInstance(context.Context, Credentials, string) (Instance, error) {
	d.gets++
	return d.instance(d.sshFromCall > 0 && d.gets >= d.sshFromCall), nil
}

func (d *flakyDetailDriver) instance(direct bool) Instance {
	inst := Instance{ExternalID: "inst-1", Name: "gpu", Power: PowerRunning, SSHConnections: []SSHConnection{
		{ID: "proxy", Type: "proxy", Host: "ssh.example", Port: 22, User: "inst-1", InstallSupported: false},
	}}
	if d.stopped {
		inst.Power = PowerStopped
	}
	if direct {
		inst.PublicIP = d.host
		inst.SSHConnections = append([]SSHConnection{{ID: "direct", Type: "direct", Host: d.host, Port: d.port, User: "root", InstallSupported: true}}, inst.SSHConnections...)
	}
	return inst
}

func (d *flakyDetailDriver) InstallAgentWithReport(_ context.Context, _ Credentials, req InstallRequest) (InstallReport, error) {
	d.req = req
	return d.report, nil
}

func (d *flakyDetailDriver) InstallAgent(ctx context.Context, creds Credentials, req InstallRequest) error {
	_, err := d.InstallAgentWithReport(ctx, creds, req)
	return err
}

// An instance whose direct SSH the provider leaves out for a moment is asked
// about again before the install gives up; when it never shows, the error
// says the provider reports none right now.
func TestInstallAsksAgainForAnInstanceWithoutDirectSSH(t *testing.T) {
	old := detailRetryDelay
	detailRetryDelay = time.Millisecond
	t.Cleanup(func() { detailRetryDelay = old })

	d := &flakyDetailDriver{keyTestDriver: keyTestDriver{name: "key-test-flaky-detail", host: "203.0.113.30", port: 10533,
		report: InstallReport{AuthUsed: "platform_key", PlatformKeyAuthorized: true}}, sshFromCall: 2}
	svc, _, providerID, _ := newKeyService(t, &d.keyTestDriver, true)
	Register(d)
	if _, err := svc.Install(t.Context(), providerID, "inst-1", "https://server.example", InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if d.gets != 2 || d.req.Instance.SSHPort != 10533 {
		t.Fatalf("gets = %d, install request instance = %+v", d.gets, d.req.Instance)
	}

	never := &flakyDetailDriver{keyTestDriver: keyTestDriver{name: "key-test-flaky-never"}}
	svc, _, providerID, _ = newKeyService(t, &never.keyTestDriver, true)
	Register(never)
	_, err := svc.Install(t.Context(), providerID, "inst-1", "https://server.example", InstallOptions{})
	if err == nil || !strings.Contains(err.Error(), "reports no direct SSH endpoint for it right now") || never.gets != detailAttempts {
		t.Fatalf("err = %v, gets = %d", err, never.gets)
	}
}

// An instance the provider reports stopped has no SSH endpoint: install and
// precheck say so at once instead of asking again or blaming a restart.
func TestInstallAndPrecheckSayAStoppedInstanceIsStopped(t *testing.T) {
	d := &flakyDetailDriver{keyTestDriver: keyTestDriver{name: "key-test-stopped"}, stopped: true}
	svc, _, providerID, _ := newKeyService(t, &d.keyTestDriver, true)
	Register(d)
	_, err := svc.Install(t.Context(), providerID, "inst-1", "https://server.example", InstallOptions{})
	if err == nil || !strings.Contains(err.Error(), "is stopped at the provider") || d.gets != 0 {
		t.Fatalf("install err = %v, gets = %d", err, d.gets)
	}
	report, err := svc.Precheck(t.Context(), providerID, "inst-1", "https://server.example", InstallOptions{})
	if err != nil || len(report.Checks) != 1 || report.Checks[0].Status != "fail" || !strings.Contains(checkText(report.Checks[0]), "is stopped at the provider") {
		t.Fatalf("precheck = %+v, %v", report.Checks, err)
	}
}
