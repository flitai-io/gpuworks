package zstackcloud_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"gpuworks.ai/gpuworks/internal/server/provision"
)

// Exercise the UI's API route, Cloud's async shell API, the actual authorization
// script and SSH authentication. Agent binary upload remains the installer's
// responsibility and has its own provision tests.
func TestZStackCloudInstallPreparesSSHAccess(t *testing.T) {
	for _, tc := range []struct {
		name, credential, wantAuth string
		denyAuthorization          bool
	}{
		{"platform_key", "", provision.AuthPlatformKey, false},
		{"password", provision.AuthPassword, provision.AuthPlatformKey, false},
		{"private_key", provision.AuthPrivateKey, provision.AuthPlatformKey, false},
		{"password_after_api_denied", provision.AuthPassword, provision.AuthPassword, true},
		{"private_key_after_api_denied", provision.AuthPrivateKey, provision.AuthPrivateKey, true},
		{"no_credentials_after_api_denied", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			operatorKey, err := provision.GeneratePlatformKey("existing-operator-key")
			if err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			keyFile := filepath.Join(home, ".ssh", "authorized_keys")
			if err := os.MkdirAll(filepath.Dir(keyFile), 0700); err != nil {
				t.Fatal(err)
			}
			original := strings.TrimSpace(operatorKey.PublicKey) + "\n"
			if err := os.WriteFile(keyFile, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			const user, password = "cloud-admin", "operator-password"
			port := startCloudSSHAuthServer(t, user, password, keyFile)
			inventory := cloudInventoryFixture(t)
			var shellCalls, installCalls, logins atomic.Int32
			var shellResult struct {
				ReturnCode int    `json:"returnCode"`
				Stdout     string `json:"stdout"`
				Stderr     string `json:"stderr"`
			}
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !validCloudAccessKey(r) {
					w.WriteHeader(401)
					return
				}
				switch r.Method + " " + r.URL.Path {
				case "GET /v1/hosts":
					fmt.Fprintf(w, `{"inventories":[{"uuid":"host-a","name":"Host A","managementIp":"127.0.0.1","username":%q,"sshPort":%d,"hypervisorType":"KVM","status":"Connected","state":"Enabled"}]}`, user, port)
				case "PUT /v1/hosts/kvm/actions":
					shellCalls.Add(1)
					var body struct {
						RunShell struct {
							HostUUIDs []string `json:"hostUuids"`
							Script    string   `json:"script"`
						} `json:"kvmRunShell"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					if len(body.RunShell.HostUUIDs) != 1 || body.RunShell.HostUUIDs[0] != "host-a" {
						t.Error("authorized a host other than the selected target")
						w.WriteHeader(400)
						return
					}
					if tc.denyAuthorization {
						w.WriteHeader(http.StatusServiceUnavailable)
						fmt.Fprint(w, `{"error":{"code":"ID.1002","details":"permission denied"}}`)
						return
					}
					// KvmRunShell's ScriptRunner writes through an unquoted heredoc
					// before executing bash; direct sh -c would hide early expansion.
					wrapper := "cat << EOF1 > \"$1\"\n" + body.RunShell.Script + "\nEOF1\nbash \"$1\""
					cmd := exec.CommandContext(r.Context(), "bash", "-c", wrapper, "cloud-script", filepath.Join(home, "cloud-script.sh"))
					cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
					output, err := cmd.CombinedOutput()
					shellResult.ReturnCode, shellResult.Stdout = 0, string(output)
					if err != nil {
						var exitErr *exec.ExitError
						if !errors.As(err, &exitErr) {
							t.Errorf("execute Cloud script fixture: %v", err)
							w.WriteHeader(500)
							return
						}
						shellResult.ReturnCode = exitErr.ExitCode()
					}
					w.WriteHeader(202)
					fmt.Fprint(w, `{"location":"http://cloud.internal/zstack/v1/api-jobs/0123456789abcdef0123456789abcdef","apiTimeout":30000}`)
				case "GET /v1/api-jobs/0123456789abcdef0123456789abcdef":
					if err := json.NewEncoder(w).Encode(map[string]any{"inventory": map[string]any{"host-a": shellResult}}); err != nil {
						t.Error(err)
					}
				default:
					inventory.Config.Handler.ServeHTTP(w, r)
				}
			}))
			t.Cleanup(cloud.Close)
			env := newCloudEnvWithSSHServer(t, installerFunc(func(ctx context.Context, req provision.SSHInstallRequest) (provision.SSHInstallReport, error) {
				installCalls.Add(1)
				if req.User != user || req.Port != port || req.Target != "127.0.0.1" {
					return provision.SSHInstallReport{}, errors.New("Cloud SSH account or port was lost")
				}
				if req.PlatformKey == nil {
					return provision.SSHInstallReport{}, errors.New("Cloud dropped the platform key instead of using the shared credential order")
				}
				client, outcome, err := provision.DialSSH(ctx, req.User, req.Target, req.Port, req.SSHAuth, time.Second, nil)
				if err != nil {
					return provision.SSHInstallReport{}, err
				}
				defer client.Close()
				logins.Add(1)
				if outcome.Used != tc.wantAuth {
					return provision.SSHInstallReport{}, fmt.Errorf("SSH used %s, want %s", outcome.Used, tc.wantAuth)
				}
				check, authorized := provision.AuthorizePlatformKey(ctx, client, req.SSHAuth, outcome)
				if !authorized || check.Status != "ok" {
					return provision.SSHInstallReport{}, fmt.Errorf("unexpected authorization report: %+v", check)
				}
				return provision.SSHInstallReport{AuthUsed: outcome.Used, PlatformKeyAuthorized: authorized, Checks: []provision.CheckResult{outcome.Check(), check}}, nil
			}))
			platform, _, err := env.Store().PlatformSSHKey()
			if err != nil {
				t.Fatal(err)
			}
			path := addedProvider(t, env, cloud.URL)
			requestJSON(t, env, "POST", path+"/sync", `{}`, 200)
			if shellCalls.Load() != 0 {
				t.Fatal("adding or syncing Cloud modified the host")
			}
			body := map[string]string{"external_id": "host-a", "server_url": env.URL(), "ssh_key_pem": "", "password": "", "ssh_connection_id": "auto", "connect_mode": "agent_dial"}
			switch tc.credential {
			case provision.AuthPassword:
				body["password"] = password
			case provision.AuthPrivateKey:
				body["ssh_key_pem"] = operatorKey.PrivatePEM
			}
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			status := http.StatusOK
			if tc.wantAuth == "" {
				status = http.StatusFailedDependency
			}
			raw := requestJSON(t, env, "POST", path+"/install", string(encoded), status)
			for _, sensitive := range []string{password, "PRIVATE KEY-----", "test-access-secret"} {
				if bytes.Contains(raw, []byte(sensitive)) {
					t.Fatal("installation response leaked a credential")
				}
			}
			if shellCalls.Load() != 2 || installCalls.Load() != 1 {
				t.Fatalf("SSH preparation or installation was skipped: authorizations=%d installs=%d", shellCalls.Load(), installCalls.Load())
			}
			if tc.wantAuth == "" {
				if logins.Load() != 0 {
					t.Fatal("installation authenticated without any usable credential")
				}
				keys, err := os.ReadFile(keyFile)
				if err != nil || string(keys) != original {
					t.Fatal("failed authorization changed existing host keys")
				}
				nodes, err := env.Store().ListNodes()
				if err != nil || len(nodes) != 0 {
					t.Fatal("failed authorization left a pending node")
				}
				return
			}
			var result struct {
				NodeUUID string                  `json:"node_uuid"`
				Checks   []provision.CheckResult `json:"checks"`
			}
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
			if installCalls.Load() != 1 || logins.Load() != 1 {
				t.Fatal("installation did not authenticate over SSH")
			}
			keys, err := os.ReadFile(keyFile)
			if err != nil {
				t.Fatal(err)
			}
			wantPrecheck := "ok"
			if tc.denyAuthorization {
				wantPrecheck = "warn"
			}
			var passedPrecheck bool
			for _, check := range result.Checks {
				if check.Name == "precheck" && check.Status == wantPrecheck {
					passedPrecheck = true
				}
			}
			if !passedPrecheck {
				t.Fatalf("SSH precheck status must be %s: %+v", wantPrecheck, result.Checks)
			}
			keyBody := strings.Fields(platform.PublicKey)[1]
			if !bytes.HasPrefix(keys, []byte(original)) || strings.Count(string(keys), keyBody) != 1 {
				t.Fatal("authorization duplicated the platform key or changed an existing key")
			}
			credential, secrets, err := env.Store().GetNodeSSHCredential(result.NodeUUID)
			if err != nil || credential.User != user || credential.Port != port || !secrets.Empty() {
				t.Fatal("recovery did not retain the actual SSH connection using the authorized platform key")
			}
		})
	}
}

// Use real SSH authentication and authorization scripts in a temporary home.
// Read-only environment probes have explicit fixture responses; unknown
// commands are rejected instead of running on the test runner.
func startCloudSSHAuthServer(t *testing.T, user, password, keyFile string) int {
	t.Helper()
	hostKey, err := provision.GeneratePlatformKey("test-ssh-server")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey([]byte(hostKey.PrivatePEM))
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, supplied []byte) (*ssh.Permissions, error) {
			if meta.User() == user && string(supplied) == password {
				return nil, nil
			}
			return nil, errors.New("password rejected")
		},
		PublicKeyCallback: func(meta ssh.ConnMetadata, supplied ssh.PublicKey) (*ssh.Permissions, error) {
			contents, err := os.ReadFile(keyFile)
			if err != nil {
				return nil, err
			}
			for len(contents) > 0 {
				key, _, _, rest, err := ssh.ParseAuthorizedKey(contents)
				if err != nil {
					break
				}
				if meta.User() == user && bytes.Equal(key.Marshal(), supplied.Marshal()) {
					return nil, nil
				}
				contents = rest
			}
			return nil, errors.New("key not in authorized_keys")
		},
	}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				server, channels, requests, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for ch := range channels {
					if ch.ChannelType() != "session" {
						_ = ch.Reject(ssh.UnknownChannelType, "session channels only")
						continue
					}
					channel, requests, err := ch.Accept()
					if err != nil {
						continue
					}
					workers.Add(1)
					go func() {
						defer workers.Done()
						defer channel.Close()
						for req := range requests {
							var payload struct{ Command string }
							if req.Type != "exec" || ssh.Unmarshal(req.Payload, &payload) != nil {
								_ = req.Reply(false, nil)
								continue
							}
							_ = req.Reply(true, nil)
							output, status := cloudSSHCommand(t, filepath.Dir(filepath.Dir(keyFile)), payload.Command)
							_, _ = channel.Write([]byte(output))
							_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{status}))
							return
						}
					}()
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); workers.Wait() })
	return listener.Addr().(*net.TCPAddr).Port
}

func cloudSSHCommand(t *testing.T, home, command string) (string, uint32) {
	t.Helper()
	switch {
	case strings.HasPrefix(command, "set -e\numask 077\n"), strings.HasPrefix(command, `file="$HOME/.ssh/authorized_keys"`):
		cmd := exec.CommandContext(t.Context(), "sh", "-c", command)
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
		output, err := cmd.CombinedOutput()
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				return string(output), uint32(exitErr.ExitCode())
			}
			t.Errorf("execute SSH authorization fixture: %v", err)
			return "could not execute authorization script", 1
		}
		return string(output), 0
	case strings.Contains(command, "uname -s") && strings.Contains(command, "uname -m"):
		return "Linux x86_64 6.1.0\n", 0
	case command == "uname -s":
		// The precheck and install ask for the kernel name first: a Mac takes
		// the macOS path.
		return "Linux\n", 0
	case strings.HasPrefix(command, `if [ "$(id -u)" = "0" ]`):
		return "passwordless sudo available\n", 0
	case strings.HasPrefix(command, "for python_bin in "):
		return "Python 3.11.0\n", 0
	case strings.HasPrefix(command, "if command -v nvidia-smi "):
		return "GPU 0: NVIDIA A100 (UUID: GPU-fixture)\n", 0
	case strings.HasPrefix(command, "if test -e /dev/fuse;"):
		return "/dev/fuse present\n", 0
	case strings.HasPrefix(command, "command -v docker "):
		return "Docker version 27.0.0\n", 0
	case strings.HasPrefix(command, "command -v nvidia-ctk "):
		return "", 0
	case strings.HasPrefix(command, "curl -fsS --connect-timeout 3 "):
		return "ok\n", 0
	case strings.Contains(command, "systemctl cat gpuworks-agent") && strings.Contains(command, "ps axww"):
		// The existing-agent look on a fresh VM: no unit and no agent running.
		return "", 0
	case strings.HasPrefix(command, "sh -c 'dir="):
		// The install directory probe as a non-root user with passwordless
		// sudo on a fresh VM: the directory does not exist yet, so its nearest
		// ancestor on the local root filesystem is what gets probed.
		return "requested=/opt/gpuworks\nexists=no\nprobed=/opt\nuid=1000\nuser=cloud-admin\n" +
			"mount=/\nfstype=ext4\noptions=rw,relatime\nstatfs=ext2/ext3\n" +
			"total_kib=103084600\navail_kib=83886080\ncontainer=no\nsudo=yes\nwritable=no\n" +
			"priv_writable=yes\nchown=ok\nnames=ok\n", 0
	default:
		t.Errorf("unexpected SSH command: %s", command)
		return "unsupported fixture command", 127
	}
}
