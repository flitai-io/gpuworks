package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

var platformKeyStores int

func platformKeyAPIStore(t *testing.T, withKey bool) (*store.Store, provision.PlatformKey) {
	t.Helper()
	platformKeyStores++
	st, err := store.Open(fmt.Sprintf("file:%s_%d?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"), platformKeyStores), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, err := auth.NewSecretbox("platform-key-api")
	if err != nil {
		t.Fatal(err)
	}
	st.SetSecretbox(box)
	key, err := provision.GeneratePlatformKey("gpuworks@test")
	if err != nil {
		t.Fatal(err)
	}
	if withKey {
		if _, err := st.SetPlatformSSHKey(key.PrivatePEM, key.PublicKey, key.Fingerprint); err != nil {
			t.Fatal(err)
		}
	}
	return st, key
}

func decodeKeyResponse(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "authorize_command,created_at,fingerprint,public_key" {
		t.Fatalf("response keys = %v", keys)
	}
	if bytes.Contains(body, []byte("PRIVATE KEY")) {
		t.Fatalf("response leaks the private key: %s", body)
	}
	return got
}

func TestPlatformSSHKeyEndpoints(t *testing.T) {
	st, key := platformKeyAPIStore(t, false)
	h := apiHandlers{d: Deps{Store: st}}
	call := func(method, body string, handler http.HandlerFunc) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(method, "/api/v1/ssh-key", strings.NewReader(body)))
		return rec
	}

	if rec := call(http.MethodGet, "", h.getPlatformSSHKey); rec.Code != http.StatusNotFound {
		t.Fatalf("GET before a key: %d %s", rec.Code, rec.Body.String())
	}

	// Import an existing key: fingerprint is the key's own, comment names the server.
	rec := call(http.MethodPut, `{"private_key_pem":`+jsonString(key.PrivatePEM)+`}`, h.importPlatformSSHKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	got := decodeKeyResponse(t, rec.Body.Bytes())
	if got["fingerprint"] != key.Fingerprint || !strings.HasPrefix(got["public_key"].(string), strings.Join(strings.Fields(key.PublicKey)[:2], " ")) {
		t.Fatalf("import response = %v", got)
	}
	if got["authorize_command"] != provision.AuthorizeKeyCommand(got["public_key"].(string)) {
		t.Fatalf("authorize_command = %v", got["authorize_command"])
	}
	if _, err := time.Parse(time.RFC3339Nano, got["created_at"].(string)); err != nil {
		t.Fatalf("created_at = %v", got["created_at"])
	}
	rec = call(http.MethodGet, "", h.getPlatformSSHKey)
	if rec.Code != http.StatusOK || decodeKeyResponse(t, rec.Body.Bytes())["fingerprint"] != key.Fingerprint {
		t.Fatalf("GET after import: %d %s", rec.Code, rec.Body.String())
	}

	// Rotate: a new key replaces it.
	rec = call(http.MethodPost, "", h.rotatePlatformSSHKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body.String())
	}
	rotated := decodeKeyResponse(t, rec.Body.Bytes())
	if rotated["fingerprint"] == key.Fingerprint || !strings.HasPrefix(rotated["public_key"].(string), "ssh-ed25519 ") {
		t.Fatalf("rotate response = %v", rotated)
	}
	if row, _, err := st.PlatformSSHKey(); err != nil || row.Fingerprint != rotated["fingerprint"] {
		t.Fatalf("stored after rotate = %+v err=%v", row, err)
	}

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("secret"))
	for _, body := range []string{`{`, `{"private_key_pem":""}`, `{"private_key_pem":"junk"}`, `{"private_key_pem":` + jsonString(string(pem.EncodeToMemory(block))) + `}`} {
		if rec := call(http.MethodPut, body, h.importPlatformSSHKey); rec.Code != http.StatusBadRequest {
			t.Fatalf("import %q: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if row, _, _ := st.PlatformSSHKey(); row.Fingerprint != rotated["fingerprint"] {
		t.Fatal("a rejected import replaced the key")
	}
}

func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func TestPlatformSSHKeyChangesRequireAdministrator(t *testing.T) {
	signer, err := auth.NewTokenSigner("router-platform-key-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := platformKeyAPIStore(t, true)
	r := Router(Deps{AuthSigner: signer, Store: st})
	for _, tc := range []struct {
		method, path, role string
		want               int
	}{
		{http.MethodGet, "/api/v1/ssh-key", "user", http.StatusOK},
		{http.MethodPost, "/api/v1/ssh-key/rotate", "user", http.StatusForbidden},
		{http.MethodPut, "/api/v1/ssh-key", "user", http.StatusForbidden},
		{http.MethodPost, "/api/v1/ssh-key/rotate", "admin", http.StatusOK},
		{http.MethodGet, "/api/v1/ssh-key", "", http.StatusUnauthorized},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		if tc.role != "" {
			token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: tc.role, Role: tc.role})
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("%s %s as %q: %d, want %d (%s)", tc.method, tc.path, tc.role, rec.Code, tc.want, rec.Body.String())
		}
	}
}

// fakeKeyInstaller stands in for SSHInstaller's authorize and probe steps.
type fakeKeyInstaller struct {
	authorized bool
	authReq    provision.SSHProbeRequest
	probeReq   provision.SSHProbeRequest
}

func (f *fakeKeyInstaller) InstallSSH(context.Context, provision.SSHInstallRequest) (provision.SSHInstallReport, error) {
	panic("not used")
}

func (f *fakeKeyInstaller) AuthorizePlatformKey(_ context.Context, req provision.SSHProbeRequest) (provision.CheckResult, bool) {
	f.authReq = req
	if f.authorized {
		return provision.CheckResult{Name: "Platform key", Status: "ok", Detail: "added to /root/.ssh/authorized_keys"}, true
	}
	return provision.CheckResult{Name: "Platform key", Status: "warn", Detail: "could not log in"}, false
}

func (f *fakeKeyInstaller) Probe(_ context.Context, req provision.SSHProbeRequest) provision.ProbeResult {
	f.probeReq = req
	return provision.ProbeResult{Reachable: true}
}

// Saving a password for the out-of-band channel uses it once to authorize
// the platform key; when that works, the password is not kept.
func TestSavingAPasswordAuthorizesThePlatformKeyInstead(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       string
		authorized bool
		wantMode   string
		wantPW     bool
	}{
		{name: "password authorized", mode: provision.AuthPassword, authorized: true, wantMode: provision.AuthPlatformKey},
		{name: "password authorize fails", mode: provision.AuthPassword, authorized: false, wantMode: provision.AuthPassword, wantPW: true},
		{name: "keyboard-interactive authorized", mode: provision.AuthKeyboardInteractive, authorized: true, wantMode: provision.AuthPlatformKey},
		{name: "keyboard-interactive authorize fails", mode: provision.AuthKeyboardInteractive, authorized: false, wantMode: provision.AuthPassword, wantPW: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, key := platformKeyAPIStore(t, true)
			node, _, err := st.CreateNodePending("ssh-node", "agent_dial")
			if err != nil {
				t.Fatal(err)
			}
			installer := &fakeKeyInstaller{authorized: tc.authorized}
			h := apiHandlers{d: Deps{Store: st, NodeInstaller: installer}}
			body := `{"host":"gpu.example","port":2222,"user":"root","auth_mode":` + jsonString(tc.mode) + `,"password":"pw-123","private_key_pem":"ignored","probe":true}`
			req := withNodeRecoveryURLParam(httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body)), "uuid", node.UUID)
			rec := httptest.NewRecorder()
			h.putNodeSSH(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			assertSanitizedSSHResponse(t, rec.Body.String(), "pw-123")
			var resp struct {
				AuthMode    string                 `json:"auth_mode"`
				HasPassword bool                   `json:"has_password"`
				PlatformKey *provision.CheckResult `json:"platform_key"`
				Probe       *provision.ProbeResult `json:"probe"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.AuthMode != tc.wantMode || resp.HasPassword != tc.wantPW || resp.PlatformKey == nil || resp.Probe == nil {
				t.Fatalf("response = %s", rec.Body.String())
			}
			// The authorize login used exactly what the operator chose.
			if installer.authReq.Mode != tc.mode || installer.authReq.Password != "pw-123" || installer.authReq.PrivateKeyPEM != "" || installer.authReq.PlatformKey == nil || installer.authReq.PlatformKey.Fingerprint != key.Fingerprint {
				t.Fatalf("authorize request = %+v", installer.authReq.SSHAuth)
			}
			_, secrets, err := st.GetNodeSSHCredential(node.UUID)
			if err != nil || (secrets.Password != "") != tc.wantPW || (tc.wantPW && secrets.Password != "pw-123") || secrets.PrivateKeyPEM != "" {
				t.Fatalf("stored secrets = %+v err=%v", secrets, err)
			}
			if tc.authorized && (installer.probeReq.Mode != provision.AuthPlatformKey || installer.probeReq.Password != "" || installer.probeReq.PrivateKeyPEM != "") {
				t.Fatalf("probe after authorizing used %+v", installer.probeReq.SSHAuth)
			}
			if !tc.authorized && (installer.probeReq.Mode != tc.mode || installer.probeReq.Password != "pw-123" || installer.probeReq.PrivateKeyPEM != "") {
				t.Fatalf("probe without authorizing used %+v", installer.probeReq.SSHAuth)
			}
			// A GET reports the stored credential type, not the protocol used
			// during the one-off authorize/probe. It never returns the secret.
			get := httptest.NewRecorder()
			getReq := withNodeRecoveryURLParam(httptest.NewRequest(http.MethodGet, "/", nil), "uuid", node.UUID)
			h.getNodeSSH(get, getReq)
			if get.Code != http.StatusOK {
				t.Fatalf("GET status=%d body=%s", get.Code, get.Body.String())
			}
			assertSanitizedSSHResponse(t, get.Body.String(), "pw-123")
			var got map[string]any
			if err := json.Unmarshal(get.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			wantKeys := []string{"auth_mode", "has_password", "has_private_key", "host", "port", "user"}
			keys := make([]string, 0, len(got))
			for key := range got {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			if strings.Join(keys, ",") != strings.Join(wantKeys, ",") || got["auth_mode"] != tc.wantMode || got["has_password"] != tc.wantPW || got["has_private_key"] != false || got["host"] != "gpu.example" || got["port"] != float64(2222) || got["user"] != "root" {
				t.Fatalf("GET response = %s", get.Body.String())
			}
		})
	}
}

func TestSavingThePlatformKeyModeStoresNoSecret(t *testing.T) {
	st, _ := platformKeyAPIStore(t, true)
	node, _, err := st.CreateNodePending("ssh-node", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	installer := &fakeKeyInstaller{}
	h := apiHandlers{d: Deps{Store: st, NodeInstaller: installer}}
	req := withNodeRecoveryURLParam(httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"host":"gpu.example","user":"root","auth_mode":"platform_key","password":"stale"}`)), "uuid", node.UUID)
	rec := httptest.NewRecorder()
	h.putNodeSSH(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"auth_mode":"platform_key"`) || strings.Contains(rec.Body.String(), `"platform_key":{`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if installer.authReq.Host != "" {
		t.Fatal("platform_key mode must not log in to authorize anything")
	}
	if _, secrets, err := st.GetNodeSSHCredential(node.UUID); err != nil || !secrets.Empty() {
		t.Fatalf("stored = %+v err=%v", secrets, err)
	}

	// Without a platform key on the server the mode cannot be saved.
	bare, _ := platformKeyAPIStore(t, false)
	other, _, _ := bare.CreateNodePending("ssh-node-2", "agent_dial")
	h = apiHandlers{d: Deps{Store: bare}}
	req = withNodeRecoveryURLParam(httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"host":"gpu.example","user":"root","auth_mode":"platform_key"}`)), "uuid", other.UUID)
	rec = httptest.NewRecorder()
	h.putNodeSSH(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "platform SSH key is not available") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestInstalledSSHSecretsKeepOnlyWhatIsNeeded(t *testing.T) {
	auth := provision.SSHAuth{Password: "pw", PrivateKeyPEM: "KEY", InstancePassword: "ipw"}
	for _, tc := range []struct {
		report provision.SSHInstallReport
		want   store.NodeSSHSecrets
	}{
		{provision.SSHInstallReport{AuthUsed: provision.AuthPassword, PlatformKeyAuthorized: true}, store.NodeSSHSecrets{}},
		{provision.SSHInstallReport{AuthUsed: provision.AuthKeyboardInteractive, PlatformKeyAuthorized: true}, store.NodeSSHSecrets{}},
		{provision.SSHInstallReport{AuthUsed: provision.AuthPlatformKey, PlatformKeyAuthorized: true}, store.NodeSSHSecrets{}},
		{provision.SSHInstallReport{AuthUsed: provision.AuthPassword}, store.NodeSSHSecrets{Password: "pw"}},
		{provision.SSHInstallReport{AuthUsed: provision.AuthKeyboardInteractive}, store.NodeSSHSecrets{Password: "pw"}},
		{provision.SSHInstallReport{AuthUsed: provision.AuthPrivateKey}, store.NodeSSHSecrets{PrivateKeyPEM: "KEY"}},
		{provision.SSHInstallReport{AuthUsed: provision.AuthInstancePassword}, store.NodeSSHSecrets{Password: "ipw"}},
	} {
		if got := installedSSHSecrets(auth, tc.report); got != tc.want {
			t.Fatalf("%+v: secrets = %+v, want %+v", tc.report, got, tc.want)
		}
	}
}
