package provision

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// authServer is an sshd stand-in: it accepts the listed public keys and
// password for one user, and runs exec requests with the real sh under a
// private HOME, so the authorize script is exercised as written.
type authServer struct {
	host, home string
	port       int
}

func startAuthServer(t *testing.T, user, password string, keys ...ssh.PublicKey) authServer {
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
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			for _, k := range keys {
				if meta.User() == user && bytes.Equal(k.Marshal(), key.Marshal()) {
					return nil, nil
				}
			}
			return nil, errors.New("public key rejected")
		},
		PasswordCallback: func(meta ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if password != "" && meta.User() == user && string(pass) == password {
				return nil, nil
			}
			return nil, errors.New("password rejected")
		},
	}
	cfg.AddHostKey(signer)
	home := t.TempDir()
	handler := func(cmd string, stdin io.Reader) (string, string, uint32) {
		c := exec.Command("sh", "-c", cmd)
		c.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
		var stdout, stderr bytes.Buffer
		c.Stdout, c.Stderr = &stdout, &stderr
		status := uint32(0)
		if err := c.Run(); err != nil {
			status = 1
		}
		return stdout.String(), stderr.String(), status
	}
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
			go serveFakeSSHConn(conn, cfg, handler)
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return authServer{host: addr.IP.String(), port: addr.Port, home: home}
}

func publicOf(t *testing.T, key *PlatformKey) ssh.PublicKey {
	t.Helper()
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func dial(t *testing.T, srv authServer, auth SSHAuth) (*ssh.Client, AuthOutcome, error) {
	t.Helper()
	client, outcome, err := DialSSH(context.Background(), "root", srv.host, srv.port, auth, 5*time.Second, nil)
	if client != nil {
		t.Cleanup(func() { _ = client.Close() })
	}
	return client, outcome, err
}

func TestPlatformKeyLogsInWithoutAnythingElse(t *testing.T) {
	key := testPlatformKey(t)
	srv := startAuthServer(t, "root", "", publicOf(t, key))
	client, outcome, err := dial(t, srv, SSHAuth{Mode: AuthPlatformKey, PlatformKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Used != AuthPlatformKey {
		t.Fatalf("used = %q", outcome.Used)
	}
	if check := outcome.Check(); check.Status != "pass" || check.Code != messages.CheckSSHLoggedIn || check.Params["method"] != AuthPlatformKey || check.Evidence != "platform key "+key.Fingerprint+" accepted" {
		t.Fatalf("check = %+v", check)
	}
	check, authorized := AuthorizePlatformKey(context.Background(), client, SSHAuth{PlatformKey: key}, outcome)
	if !authorized || check.text() != "authorized (used to log in)" {
		t.Fatalf("authorize = %+v %v", check, authorized)
	}
	if _, err := os.Stat(filepath.Join(srv.home, ".ssh")); err == nil {
		t.Fatal("logging in with the platform key must not touch authorized_keys")
	}
}

func TestUnauthorizedPlatformKeyTellsTheOperatorWhatToRun(t *testing.T) {
	key := testPlatformKey(t)
	srv := startAuthServer(t, "root", "secret")
	_, outcome, err := dial(t, srv, SSHAuth{Mode: AuthPlatformKey, PlatformKey: key})
	var authErr *SSHAuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("err = %v, want an SSHAuthError", err)
	}
	msg := err.Error()
	for _, want := range []string{"tried: platform key " + key.Fingerprint, "has not authorized the platform key", AuthorizeKeyCommand(key.PublicKey)} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q lacks %q", msg, want)
		}
	}
	if outcome.Used != "" || outcome.Check().Status != "fail" {
		t.Fatalf("outcome = %+v check=%+v", outcome, outcome.Check())
	}
}

// Cloud installs try the platform key first and fall back to the instance
// password; after the fallback the key is written so the next login needs
// nothing else.
func TestFallbackPasswordThenPlatformKeyIsAuthorizedOnce(t *testing.T) {
	key := testPlatformKey(t)
	srv := startAuthServer(t, "root", "instance-pw")
	auth := SSHAuth{PlatformKey: key, InstancePassword: "instance-pw"}
	client, outcome, err := dial(t, srv, auth)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Used != AuthInstancePassword {
		t.Fatalf("used = %q", outcome.Used)
	}
	want := "platform key " + key.Fingerprint + " rejected → instance password accepted"
	if got := outcome.Check().Evidence; got != want {
		t.Fatalf("detail = %q, want %q", got, want)
	}
	check, authorized := AuthorizePlatformKey(context.Background(), client, auth, outcome)
	file := filepath.Join(srv.home, ".ssh", "authorized_keys")
	if !authorized || check.Status != "pass" || check.text() != "added to "+file {
		t.Fatalf("authorize = %+v %v", check, authorized)
	}
	// Same key, any comment: not added twice.
	again := *key
	again.PublicKey = strings.Replace(key.PublicKey, "gpuworks@test", "gpuworks@renamed", 1)
	check, authorized = AuthorizePlatformKey(context.Background(), client, SSHAuth{PlatformKey: &again}, outcome)
	if !authorized || check.text() != "already in "+file {
		t.Fatalf("second authorize = %+v %v", check, authorized)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), strings.Fields(key.PublicKey)[1]) != 1 {
		t.Fatalf("authorized_keys = %q", raw)
	}
	for path, want := range map[string]os.FileMode{filepath.Join(srv.home, ".ssh"): 0o700, file: 0o600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s mode = %v err=%v, want %v", path, info.Mode().Perm(), err, want)
		}
	}

	// The written key now works on its own.
	if _, outcome, err := dial(t, startAuthServerFromFile(t, srv, file), SSHAuth{Mode: AuthPlatformKey, PlatformKey: key}); err != nil || outcome.Used != AuthPlatformKey {
		t.Fatalf("login with the authorized key: used=%q err=%v", outcome.Used, err)
	}
}

// startAuthServerFromFile serves the keys an authorized_keys file lists.
func startAuthServerFromFile(t *testing.T, _ authServer, file string) authServer {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var keys []ssh.PublicKey
	for len(bytes.TrimSpace(raw)) > 0 {
		pub, _, _, rest, err := ssh.ParseAuthorizedKey(raw)
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, pub)
		raw = rest
	}
	return startAuthServer(t, "root", "", keys...)
}

func TestAuthorizeFailureKeepsTheCredentialThatWorked(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
	key := testPlatformKey(t)
	srv := startAuthServer(t, "root", "pw")
	client, outcome, err := dial(t, srv, SSHAuth{Mode: AuthPassword, Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(srv.home, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(srv.home, 0o700) })
	check, authorized := AuthorizePlatformKey(context.Background(), client, SSHAuth{PlatformKey: key}, outcome)
	if authorized || check.Status != "warn" || !strings.Contains(check.text(), "could not add the platform key") {
		t.Fatalf("authorize = %+v %v", check, authorized)
	}
	check, authorized = AuthorizePlatformKey(context.Background(), client, SSHAuth{}, outcome)
	if authorized || check.Status != "skip" || check.Code != messages.CheckPlatformKeyUnavailable || !strings.Contains(check.text(), "not available") {
		t.Fatalf("authorize without a platform key = %+v %v", check, authorized)
	}
}

func TestExplicitModesTryOnlyWhatWasChosen(t *testing.T) {
	key := testPlatformKey(t)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(other, "")
	if err != nil {
		t.Fatal(err)
	}
	userKey := string(pem.EncodeToMemory(block))
	srv := startAuthServer(t, "root", "pw", publicOf(t, key))

	// The password is right but the operator chose their own key: the
	// platform key (which would work) and the password are not tried.
	_, outcome, err := dial(t, srv, SSHAuth{Mode: AuthPrivateKey, PrivateKeyPEM: userKey, Password: "pw", PlatformKey: key})
	if err == nil || len(outcome.Tried) != 1 || !strings.HasPrefix(outcome.Tried[0], "private key SHA256:") {
		t.Fatalf("private_key mode: tried=%v err=%v", outcome.Tried, err)
	}
	if strings.Contains(err.Error(), "has not authorized the platform key") {
		t.Fatalf("hint about the platform key for a private key failure: %v", err)
	}
	_, outcome, err = dial(t, srv, SSHAuth{Mode: AuthPassword, Password: "pw", PlatformKey: key})
	if err != nil || outcome.Used != AuthPassword || fmt.Sprint(outcome.Tried) != "[password]" {
		t.Fatalf("password mode: used=%q tried=%v err=%v", outcome.Used, outcome.Tried, err)
	}
}

func TestAuthorizeKeyCommandAuthorizesTheKey(t *testing.T) {
	key := testPlatformKey(t)
	home := t.TempDir()
	cmd := exec.Command("sh", "-c", AuthorizeKeyCommand(key.PublicKey))
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
	if err != nil || strings.TrimSpace(string(raw)) != key.PublicKey {
		t.Fatalf("authorized_keys = %q err=%v", raw, err)
	}
}

func TestPlatformKeyImport(t *testing.T) {
	key := testPlatformKey(t)
	imported, err := ParsePlatformKey(key.PrivatePEM, "gpuworks@other")
	if err != nil || imported.Fingerprint != key.Fingerprint || !strings.HasSuffix(imported.PublicKey, " gpuworks@other") {
		t.Fatalf("import = %+v err=%v", imported, err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePlatformKey(string(pem.EncodeToMemory(block)), ""); err == nil || !strings.Contains(err.Error(), "passphrase") {
		t.Fatalf("encrypted import err = %v", err)
	}
	for _, bad := range []string{"", "not a key"} {
		if _, err := ParsePlatformKey(bad, ""); err == nil {
			t.Fatalf("import %q accepted", bad)
		}
	}
	if PlatformKeyComment("") != "gpuworks" || PlatformKeyComment(" gpu3 ") != "gpuworks@gpu3" {
		t.Fatal("comment")
	}
}

// The precheck only reads: it says whether the key is there and leaves the
// file alone.
func TestPrecheckReportsPlatformKeyPresenceWithoutWriting(t *testing.T) {
	key := testPlatformKey(t)
	srv := startAuthServer(t, "root", "pw")
	client, _, err := dial(t, srv, SSHAuth{Mode: AuthPassword, Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(srv.home, ".ssh", "authorized_keys")
	if c := PlatformKeyPresence(context.Background(), client, key.PublicKey); c.Status != "pass" || c.text() != "not in "+file+" yet; the install adds it" {
		t.Fatalf("absent = %+v", c)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("the precheck created authorized_keys")
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(strings.Replace(key.PublicKey, "gpuworks@test", "other-comment", 1)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := PlatformKeyPresence(context.Background(), client, key.PublicKey); c.Status != "pass" || c.text() != "already in "+file {
		t.Fatalf("present = %+v", c)
	}
}

func generatedKeyPEM(t *testing.T, rsaBits int) (string, ssh.PublicKey) {
	t.Helper()
	var block *pem.Block
	var pub ssh.PublicKey
	var err error
	if rsaBits > 0 {
		priv, genErr := rsa.GenerateKey(rand.Reader, rsaBits)
		if genErr != nil {
			t.Fatal(genErr)
		}
		block, err = ssh.MarshalPrivateKey(priv, "")
		pub, _ = ssh.NewPublicKey(&priv.PublicKey)
	} else {
		edPub, priv, _ := ed25519.GenerateKey(rand.Reader)
		block, err = ssh.MarshalPrivateKey(priv, "")
		pub, _ = ssh.NewPublicKey(edPub)
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block)), pub
}

// What L4 caught on RunPod and Vast: the SSH client skips a second method of
// a type it already tried, so the operator's key after a rejected platform
// key was never offered. Both keys must be offered in one handshake.
func TestRejectedPlatformKeyFallsBackToTheOperatorsKey(t *testing.T) {
	key := testPlatformKey(t)
	userPEM, userPub := generatedKeyPEM(t, 0)
	srv := startAuthServer(t, "root", "", userPub)
	client, outcome, err := dial(t, srv, SSHAuth{PlatformKey: key, PrivateKeyPEM: userPEM})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Used != AuthPrivateKey {
		t.Fatalf("used = %q", outcome.Used)
	}
	if detail := outcome.Check().Evidence; !strings.HasPrefix(detail, "platform key "+key.Fingerprint+" rejected → private key SHA256:") || !strings.HasSuffix(detail, " accepted") {
		t.Fatalf("detail = %q", detail)
	}
	if check, ok := AuthorizePlatformKey(context.Background(), client, SSHAuth{PlatformKey: key}, outcome); !ok || !strings.HasPrefix(check.text(), "added to ") {
		t.Fatalf("authorize = %+v %v", check, ok)
	}
}

// Keys come before passwords, and every password is tried in order.
func TestEveryCredentialIsTriedInOrder(t *testing.T) {
	key := testPlatformKey(t)
	userPEM, _ := generatedKeyPEM(t, 0)
	srv := startAuthServer(t, "root", "instance-pw")
	_, outcome, err := dial(t, srv, SSHAuth{PlatformKey: key, PrivateKeyPEM: userPEM, Password: "stale-pw", InstancePassword: "instance-pw"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Used != AuthInstancePassword || len(outcome.Tried) != 4 {
		t.Fatalf("used=%q tried=%v", outcome.Used, outcome.Tried)
	}
	if outcome.Tried[0] != "platform key "+key.Fingerprint || !strings.HasPrefix(outcome.Tried[1], "private key ") || outcome.Tried[2] != "password" || outcome.Tried[3] != "instance password" {
		t.Fatalf("order = %v", outcome.Tried)
	}

	// Nothing works: the error lists all four.
	_, outcome, err = dial(t, startAuthServer(t, "root", "other"), SSHAuth{PlatformKey: key, PrivateKeyPEM: userPEM, Password: "a", InstancePassword: "b"})
	if err == nil || len(outcome.Tried) != 4 || !strings.Contains(err.Error(), "instance password") {
		t.Fatalf("all rejected: tried=%v err=%v", outcome.Tried, err)
	}
}

// A modern sshd refuses SHA-1 RSA signatures; wrapping the signer to record
// its use must not drop the rsa-sha2 algorithms.
func TestRSAKeysStillSignWithSHA2(t *testing.T) {
	rsaPEM, rsaPub := generatedKeyPEM(t, 2048)
	_, hostKey, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostKey)
	cfg := &ssh.ServerConfig{
		PublicKeyAuthAlgorithms: []string{ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512},
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), rsaPub.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("rejected")
		},
	}
	cfg.AddHostKey(hostSigner)
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
			go serveFakeSSHConn(conn, cfg, func(string, io.Reader) (string, string, uint32) { return "", "", 0 })
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	client, outcome, err := DialSSH(context.Background(), "root", addr.IP.String(), addr.Port, SSHAuth{PlatformKey: testPlatformKey(t), PrivateKeyPEM: rsaPEM}, 5*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	if outcome.Used != AuthPrivateKey {
		t.Fatalf("used = %q", outcome.Used)
	}
}
