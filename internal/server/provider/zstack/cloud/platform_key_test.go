package cloud

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

func TestCloudAuthorizePublicKeyChecksHostResult(t *testing.T) {
	key, err := provision.GeneratePlatformKey("authorization-test")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body, wantError string
		status                int
	}{
		{"success", `{"inventory":{"host-a":{"returnCode":0,"stdout":"added /root/.ssh/authorized_keys\n","stderr":"","errorCode":null}}}`, "", 200},
		{"command failed", `{"inventory":{"host-a":{"returnCode":1,"stderr":"sensitive-output"}}}`, "exited with code 1", 200},
		{"host SSH failed", `{"inventory":{"host-a":{"returnCode":0,"errorCode":{"code":"SYS.1006","details":"sensitive-output"}}}}`, "could not execute", 200},
		{"empty response", `{}`, "missing host", 200},
		{"different host", `{"inventory":{"host-b":{"returnCode":0}}}`, "missing host", 200},
		{"null result", `{"inventory":{"host-a":null}}`, "missing host", 200},
		{"missing exit code", `{"inventory":{"host-a":{"stdout":"ok"}}}`, "missing host", 200},
		{"invalid exit code", `{"inventory":{"host-a":{"returnCode":"sensitive-output"}}}`, "invalid Cloud response", 200},
		{"API denied", `{"error":{"code":"ID.1002","details":"sensitive-output"}}`, "503", 503},
	} {
		for _, async := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/async=%t", tc.name, async), func(t *testing.T) {
				var submissions, polls int
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !validTestAccessKey(r) {
						t.Error("unsigned request")
						w.WriteHeader(401)
						return
					}
					switch r.Method + " " + r.URL.Path {
					case "PUT /v1/hosts/kvm/actions":
						submissions++
						var request struct {
							RunShell struct {
								HostUUIDs []string `json:"hostUuids"`
								Script    string   `json:"script"`
							} `json:"kvmRunShell"`
						}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Error(err)
							w.WriteHeader(400)
							return
						}
						if len(request.RunShell.HostUUIDs) != 1 || request.RunShell.HostUUIDs[0] != "host-a" || request.RunShell.Script == "" {
							t.Error("authorization did not send a script to the selected host")
						}
						if strings.Contains(request.RunShell.Script, key.PrivatePEM) {
							t.Error("private key sent to Cloud")
						}
						if async {
							w.WriteHeader(202)
							fmt.Fprint(w, `{"location":"http://mn.internal/zstack/v1/api-jobs/0123456789abcdef0123456789abcdef","apiTimeout":30000}`)
							return
						}
					case "GET /v1/api-jobs/0123456789abcdef0123456789abcdef":
						polls++
						if polls == 1 {
							w.WriteHeader(202)
							fmt.Fprint(w, `{}`)
							return
						}
					default:
						t.Errorf("unexpected request %s", r.URL)
						http.NotFound(w, r)
						return
					}
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
				}))
				defer server.Close()
				d, err := NewWithClientOptions(apiclient.WithPollInterval(time.Millisecond))
				if err != nil {
					t.Fatal(err)
				}
				detail, err := d.AuthorizePublicKey(t.Context(), provider.Credentials{Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret"}, "host-a", key.PublicKey)
				if tc.wantError == "" {
					if err != nil || detail == "" {
						t.Fatalf("authorization detail=%q err=%v", detail, err)
					}
				} else if detail != "" || err == nil || !strings.Contains(err.Error(), tc.wantError) || strings.Contains(err.Error(), "sensitive-output") {
					t.Fatalf("err=%v, want %q without remote output", err, tc.wantError)
				}
				if submissions != 1 || (async && polls != 2) {
					t.Fatalf("submissions=%d polls=%d", submissions, polls)
				}
			})
		}
	}
}

func TestCloudAuthorizePublicKeyRejectsInvalidInput(t *testing.T) {
	key, err := provision.GeneratePlatformKey("authorization-test")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ host, key string }{
		{"", key.PublicKey}, {"host-a", ""}, {"host-a", "invalid"},
		{"host-a", key.PublicKey + "\n" + key.PublicKey},
		{"host-a", `command="other-command" ` + key.PublicKey},
	} {
		if _, err := New().AuthorizePublicKey(t.Context(), provider.Credentials{}, tc.host, tc.key); err == nil || !strings.Contains(err.Error(), "SSH") {
			t.Fatalf("invalid authorization input was not rejected before client creation: %v", err)
		}
	}
}

func TestCloudPublicKeyScriptPreservesExistingKeys(t *testing.T) {
	existing, err := provision.GeneratePlatformKey("existing-key")
	if err != nil {
		t.Fatal(err)
	}
	platform, err := provision.GeneratePlatformKey("platform-key")
	if err != nil {
		t.Fatal(err)
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(platform.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	hostHome := t.TempDir()
	keyFile := filepath.Join(hostHome, ".ssh", "authorized_keys")
	if err := os.MkdirAll(filepath.Dir(keyFile), 0700); err != nil {
		t.Fatal(err)
	}
	original := strings.TrimSpace(existing.PublicKey)
	if err := os.WriteFile(keyFile, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		cmd := exec.CommandContext(t.Context(), "sh", "-c", hostPublicKeyScript(key))
		cmd.Env = []string{"HOME=" + hostHome, "PATH=" + os.Getenv("PATH")}
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("authorize host key: %v: %s", err, output)
		}
	}
	contents, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	want := original + "\n" + string(ssh.MarshalAuthorizedKey(key))
	if string(contents) != want {
		t.Fatal("authorization changed an existing key, joined key lines, or appended the platform key twice")
	}
}
