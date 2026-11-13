package provision

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestValidateSSHProbeRequestMatchesRealAuthenticationRequirements(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  SSHProbeRequest
		ok   bool
	}{
		{name: "password", req: SSHProbeRequest{Host: "node.example", Port: 22, User: "root", SSHAuth: SSHAuth{Password: "secret"}}, ok: true},
		{name: "private-key", req: SSHProbeRequest{Target: "admin@node.example:2222", SSHAuth: SSHAuth{PrivateKeyPEM: testPrivateKeyPEM(t)}}, ok: true},
		{name: "missing-host", req: SSHProbeRequest{User: "root", SSHAuth: SSHAuth{Password: "secret"}}},
		{name: "missing-user", req: SSHProbeRequest{Host: "node.example", SSHAuth: SSHAuth{Password: "secret"}}},
		{name: "bad-port", req: SSHProbeRequest{Host: "node.example", Port: 70000, User: "root", SSHAuth: SSHAuth{Password: "secret"}}},
		{name: "port-embedded-in-host", req: SSHProbeRequest{Host: "node.example:22", Port: 22, User: "root", SSHAuth: SSHAuth{Password: "secret"}}},
		{name: "ipv6", req: SSHProbeRequest{Host: "2001:db8::1", Port: 22, User: "root", SSHAuth: SSHAuth{Password: "secret"}}, ok: true},
		{name: "missing-auth-without-platform-key", req: SSHProbeRequest{Host: "node.example", Port: 22, User: "root"}},
		{name: "platform-key", req: SSHProbeRequest{Host: "node.example", Port: 22, User: "root", SSHAuth: SSHAuth{PlatformKey: testPlatformKey(t)}}, ok: true},
		{name: "platform-key-mode-without-key", req: SSHProbeRequest{Host: "node.example", Port: 22, User: "root", SSHAuth: SSHAuth{Mode: AuthPlatformKey}}},
		{name: "password-mode-without-password", req: SSHProbeRequest{Host: "node.example", Port: 22, User: "root", SSHAuth: SSHAuth{Mode: AuthPassword, PlatformKey: testPlatformKey(t)}}},
		{name: "keyboard-interactive", req: SSHProbeRequest{Host: "node.example", Port: 22, User: "root", SSHAuth: SSHAuth{Mode: AuthKeyboardInteractive, Password: "secret"}}, ok: true},
		{name: "keyboard-interactive-without-password", req: SSHProbeRequest{Host: "node.example", Port: 22, User: "root", SSHAuth: SSHAuth{Mode: AuthKeyboardInteractive, PlatformKey: testPlatformKey(t)}}},
		{name: "unknown-mode", req: SSHProbeRequest{Host: "node.example", Port: 22, User: "root", SSHAuth: SSHAuth{Mode: "kerberos", Password: "x"}}},
		{name: "malformed-key", req: SSHProbeRequest{Host: "node.example", Port: 22, User: "root", SSHAuth: SSHAuth{PrivateKeyPEM: "not a key"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSSHProbeRequest(tc.req)
			if (err == nil) != tc.ok {
				t.Fatalf("ValidateSSHProbeRequest() err=%v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func testPlatformKey(t *testing.T) *PlatformKey {
	t.Helper()
	key, err := GeneratePlatformKey("gpuworks@test")
	if err != nil {
		t.Fatal(err)
	}
	return &key
}

func testPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw}))
}

// probeHost runs a probe script under sh with ps and systemctl faked: ps
// prints psLines (as procps prints `ps axww -o args=`), systemctl reports
// the unit active with unitExecStart when that is set.
func runProbeScript(t *testing.T, script string, psLines []string, unitExecStart string) (string, error) {
	t.Helper()
	bin := t.TempDir()
	for _, tool := range []string{"sh", "sed", "head", "tail", "grep", "awk", "cat", "printf"} {
		real, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("no %s", tool)
		}
		if err := os.Symlink(real, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, filepath.Join(bin, "ps"), `printf '%s' "$PS_OUT"`)
	writeExecutable(t, filepath.Join(bin, "systemctl"), `[ -n "$UNIT" ] || exit 3
case "$1" in show) echo "$UNIT" ;; esac
exit 0`)
	ps := ""
	for _, line := range psLines {
		ps += line + "\n"
	}
	cmd := exec.Command(filepath.Join(bin, "sh"), "-c", script)
	cmd.Env = []string{"PATH=" + bin, "PS_OUT=" + ps, "UNIT=" + unitExecStart}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func TestAgentStateProbeFindsTheAgentRunFromTheStateDirectory(t *testing.T) {
	dir := "/opt/gpu works"
	line := dir + "/gpuworks-agent run --state " + dir
	if out, err := runProbeScript(t, agentStateProbeCommand(dir), []string{"/usr/sbin/sshd", line}, ""); err != nil || out != "process" {
		t.Fatalf("process: %q %v", out, err)
	}
	// busybox ps prints PID, user and state before the command line.
	if out, err := runProbeScript(t, agentStateProbeCommand(dir), []string{"  812 root     S    " + line}, ""); err != nil || out != "process" {
		t.Fatalf("busybox process: %q %v", out, err)
	}
	if out, err := runProbeScript(t, agentStateProbeCommand(dir), []string{dir + "2/gpuworks-agent run --state " + dir + "2"}, ""); err == nil {
		t.Fatalf("an agent of another directory passed: %q", out)
	}
	unit := "{ path=/opt/x/gpuworks-agent ; argv[]=/opt/x/gpuworks-agent run --state /opt/x ; ignore_errors=no }"
	if out, err := runProbeScript(t, agentStateProbeCommand("/opt/x"), nil, unit); err != nil || out != "systemd" {
		t.Fatalf("unit: %q %v", out, err)
	}
}

// Recovery tells this server's agent from another's by the server in the
// agent's configuration file, or an earlier install's arguments; a node the
// server dials (no server) accepts its agent.
func TestAgentRecoveryProbeReadsTheServerFromTheConfiguration(t *testing.T) {
	state := t.TempDir()
	line := state + "/gpuworks-agent run --state " + state
	write := func(server string) {
		if err := os.WriteFile(filepath.Join(state, "gpuworks-agent.conf"), []byte("SERVER="+server+"\nTOKEN=t\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, conf, want, server string
		wantErr                  bool
		lines                    []string
		unit                     string
	}{
		{name: "configured for this server", conf: "http://gw:8800", server: "http://gw:8800/", lines: []string{line}, want: "process"},
		{name: "configured for another", conf: "http://other:8800", server: "http://gw:8800", lines: []string{line}, wantErr: true},
		{name: "server dial", conf: "", server: "", lines: []string{line}, want: "process"},
		{name: "earlier install of another server", server: "http://gw:8800", lines: []string{"/opt/gpuworks/gpuworks-agent run --server http://other:8800 --token x --state /nonexistent"}, want: "not-running"},
		{name: "unit configured for another", conf: "http://other:8800", server: "http://gw:8800", unit: "{ argv[]=" + state + "/gpuworks-agent run --state " + state + " ; }", want: "foreign-systemd http://other:8800"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write(tc.conf)
			out, err := runProbeScript(t, agentRecoveryProbeCommand(state, tc.server, "t"), tc.lines, tc.unit)
			if (err != nil) != tc.wantErr || !tc.wantErr && out != tc.want {
				t.Fatalf("probe = %q %v, want %q (error=%v)", out, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestNormalizeSSHProbeRequestCanonicalizesTargetAndIPv6(t *testing.T) {
	password := SSHProbeRequest{Target: "admin@node.example:2222", SSHAuth: SSHAuth{Password: "secret"}}
	normalized, err := NormalizeSSHProbeRequest(password)
	if err != nil || normalized.Target != "" || normalized.Host != "node.example" || normalized.Port != 2222 || normalized.User != "admin" {
		t.Fatalf("normalized target = %+v, err=%v", normalized, err)
	}
	ipv6, err := NormalizeSSHProbeRequest(SSHProbeRequest{Host: "[2001:db8::1]", Port: 22, User: "root", SSHAuth: SSHAuth{Password: "secret"}})
	if err != nil || ipv6.Host != "2001:db8::1" {
		t.Fatalf("normalized IPv6 = %+v, err=%v", ipv6, err)
	}
}

func TestCaptureSSHSessionOutputKeepsStreamsIndependent(t *testing.T) {
	sess := &ssh.Session{}
	out := captureSSHSessionOutput(sess)
	if sess.Stdout == sess.Stderr {
		t.Fatal("stdout and stderr share a writer; concurrent SSH copies can lose probe output")
	}
	if _, err := sess.Stdout.Write([]byte("installed\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Stderr.Write([]byte("diagnostic\n")); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "installed\ndiagnostic\n" {
		t.Fatalf("combined output = %q", got)
	}
}
