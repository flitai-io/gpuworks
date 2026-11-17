package provision

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGoarchFor(t *testing.T) {
	tests := map[string]string{
		"x86_64":  "amd64",
		"amd64":   "amd64",
		"aarch64": "arm64",
		"arm64":   "arm64",
	}
	for in, want := range tests {
		got, err := goarchFor(in)
		if err != nil {
			t.Fatalf("goarchFor(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("goarchFor(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := goarchFor("mips"); err == nil {
		t.Fatal("goarchFor should reject unsupported arch")
	}
}

func TestClaimInstallDirCommandDoesNotNestQuotes(t *testing.T) {
	got := claimInstallDirCommand("/opt/gpu works", "1000", "1000")
	want := "sudo -n mkdir -p '/opt/gpu works' && sudo -n chown '1000':'1000' '/opt/gpu works'"
	if got != want {
		t.Fatalf("claimInstallDirCommand = %q, want %q", got, want)
	}
	if strings.Contains(got, "sudo -n sh -c") {
		t.Fatalf("claimInstallDirCommand unexpectedly nests shell: %q", got)
	}
}

func TestInstallDirWriteProbeUsesUnprivilegedTouchAndCleanup(t *testing.T) {
	got := installDirWriteProbeCommand("/opt/gpu works", 42)
	want := "touch '/opt/gpu works/.gpuworks-write-test-42' && rm -f '/opt/gpu works/.gpuworks-write-test-42'"
	if got != want {
		t.Fatalf("installDirWriteProbeCommand = %q, want %q", got, want)
	}
}

func TestDetectGOARCHRetriesEmptyOutput(t *testing.T) {
	attempts := 0
	uname, goarch, err := detectGOARCH(func(string) (string, error) {
		attempts++
		if attempts < 3 {
			return "", nil
		}
		return "x86_64\n", nil
	})
	if err != nil || uname != "x86_64" || goarch != "amd64" || attempts != 3 {
		t.Fatalf("detectGOARCH = %q/%q, attempts=%d, err=%v", uname, goarch, attempts, err)
	}
}

func TestDetectGOARCHContinuesAfterNULOutput(t *testing.T) {
	attempts := 0
	uname, goarch, err := detectGOARCH(func(string) (string, error) {
		attempts++
		if attempts == 1 {
			return "\x00\x00\x00\x00\x00\x00\x00\x00", nil
		}
		return "x86_64\n", nil
	})
	if err != nil || uname != "x86_64" || goarch != "amd64" || attempts != 2 {
		t.Fatalf("detectGOARCH = %q/%q, attempts=%d, err=%v", uname, goarch, attempts, err)
	}
}

func TestDetectGOARCHFindsArchAmongProbeNoise(t *testing.T) {
	uname, goarch, err := detectGOARCH(func(string) (string, error) {
		return "Welcome to the instance\narch=aarch64\n", nil
	})
	if err != nil || uname != "aarch64" || goarch != "arm64" {
		t.Fatalf("detectGOARCH = %q/%q, err=%v", uname, goarch, err)
	}
}

func TestDetectGOARCHReturnsLastProbeError(t *testing.T) {
	wantErr := errors.New("session failed")
	attempts := 0
	_, _, err := detectGOARCHWithSleep(func(string) (string, error) {
		attempts++
		return "", wantErr
	}, func(time.Duration) {})
	if !errors.Is(err, wantErr) || attempts != 40 {
		t.Fatalf("detectGOARCH attempts=%d, err=%v", attempts, err)
	}
}

func TestDetectGOARCHWaitsForColdSSHUserland(t *testing.T) {
	calls := 0
	var delays []time.Duration
	uname, goarch, err := detectGOARCHWithSleep(func(string) (string, error) {
		calls++
		if calls <= 25 {
			return "", errors.New("Process exited with status 127")
		}
		return "x86_64\n", nil
	}, func(delay time.Duration) { delays = append(delays, delay) })
	if err != nil || uname != "x86_64" || goarch != "amd64" || calls != 26 {
		t.Fatalf("detectGOARCH = %q/%q, calls=%d, err=%v", uname, goarch, calls, err)
	}
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second}
	if !reflect.DeepEqual(delays, want) {
		t.Fatalf("retry delays = %v, want %v", delays, want)
	}
}

func TestAtomicReplaceCommandQuotesPaths(t *testing.T) {
	got := atomicReplaceCommand("/opt/gpu works/agent.new", "/opt/gpu works/agent")
	want := "mv -f '/opt/gpu works/agent.new' '/opt/gpu works/agent'"
	if got != want {
		t.Fatalf("atomicReplaceCommand = %q, want %q", got, want)
	}
}

func TestResolveDebugDirUsesHomeWhenAvailable(t *testing.T) {
	if got := resolveDebugDir("/home/ubuntu", nil, "/opt/gpuworks"); got != "/home/ubuntu" {
		t.Fatalf("resolveDebugDir = %q, want /home/ubuntu", got)
	}
}

func TestResolveDebugDirFallsBackOnError(t *testing.T) {
	if got := resolveDebugDir("", errors.New("x"), "/opt/gpuworks"); got != "/opt/gpuworks" {
		t.Fatalf("resolveDebugDir = %q, want /opt/gpuworks", got)
	}
}

func TestResolveDebugDirFallsBackOnEmptyHome(t *testing.T) {
	if got := resolveDebugDir("", nil, "/opt/gpuworks"); got != "/opt/gpuworks" {
		t.Fatalf("resolveDebugDir = %q, want /opt/gpuworks", got)
	}
}

func TestUdockerDebugPathsDefaultBundle(t *testing.T) {
	udockerDir, proot, entry := udockerDebugPaths("/opt/gpuworks", "", "amd64")
	if udockerDir != "/opt/gpuworks/udocker" {
		t.Errorf("udockerDir = %q", udockerDir)
	}
	if proot != "/opt/gpuworks/udocker/bin/proot-x86_64" {
		t.Errorf("proot = %q", proot)
	}
	if entry != "/opt/gpuworks/offline/udocker/udocker/udocker" {
		t.Errorf("entry = %q", entry)
	}
}

func TestUdockerDebugPathsCustomBundle(t *testing.T) {
	udockerDir, proot, entry := udockerDebugPaths("/opt/gpuworks", "/data/udocker-bundle", "arm64")
	if entry != "/data/udocker-bundle/udocker/udocker" {
		t.Errorf("entry = %q", entry)
	}
	if udockerDir != "/opt/gpuworks/udocker" {
		t.Errorf("udockerDir = %q", udockerDir)
	}
	if proot != "/opt/gpuworks/udocker/bin/proot-arm64" {
		t.Errorf("proot = %q", proot)
	}
}

func TestUdockerDebugScriptContainsRunuserFallback(t *testing.T) {
	script := udockerDebugScript("/opt/gpuworks", "", "amd64")
	for _, want := range []string{
		"UDOCKER_DIR='/opt/gpuworks/udocker'",
		"UDOCKER_USE_PROOT_EXECUTABLE='/opt/gpuworks/udocker/bin/proot-x86_64'",
		"ENTRY='/opt/gpuworks/offline/udocker/udocker/udocker'",
		"if id udocker >/dev/null 2>&1; then",
		"exec runuser -u udocker --",
		"else",
		`exec env UDOCKER_DIR="$UDOCKER_DIR"`,
		"fi",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("udocker debug script missing %q:\n%s", want, script)
		}
	}
}

func TestInstallSSHRejectsInvalidHostAddressBeforeConnecting(t *testing.T) {
	installer := NewSSHInstaller(func(string) ([]byte, error) { return []byte("binary"), nil })
	_, err := installer.InstallSSH(t.Context(), SSHInstallRequest{
		Target: "root@example.com", ServerURL: "https://server.example", Token: "token", HostAddress: "http://gpu3.example/path",
		SSHAuth: SSHAuth{Password: "secret"},
	})
	if err == nil || !strings.Contains(err.Error(), "host_address") {
		t.Fatalf("InstallSSH error = %v, want host_address validation", err)
	}
}

func TestInstallSSHRejectsServerURLWithoutScheme(t *testing.T) {
	installer := NewSSHInstaller(func(string) ([]byte, error) { return []byte("binary"), nil })
	_, err := installer.InstallSSH(t.Context(), SSHInstallRequest{
		Target: "root@example.com", ServerURL: "sa1.tunnelfrp.com:10001", Token: "token",
		SSHAuth: SSHAuth{Password: "secret"},
	})
	if err == nil || !strings.Contains(err.Error(), "must start with http:// or https://") {
		t.Fatalf("InstallSSH error = %v, want server_url scheme validation", err)
	}
}

func TestServerDialAddressUsesSSHHostAndListenPort(t *testing.T) {
	tests := map[string]string{
		"":                "gpu3.example:18800",
		":19091":          "gpu3.example:19091",
		"0.0.0.0:19092":   "gpu3.example:19092",
		"127.0.0.1:19093": "gpu3.example:19093",
		"19094":           "gpu3.example:19094",
	}
	for listen, want := range tests {
		if got := ServerDialAddress("gpu3.example", listen); got != want {
			t.Fatalf("ServerDialAddress(%q) = %q, want %q", listen, got, want)
		}
	}
}

func TestServerDialAddressPreservesExplicitTunnelPort(t *testing.T) {
	for _, input := range []string{"http://0.tcp.ap.ngrok.io:25408", "0.tcp.ap.ngrok.io:25408"} {
		if got := ServerDialAddress(input, ":19090"); got != "0.tcp.ap.ngrok.io:25408" {
			t.Fatalf("ServerDialAddress(%q) = %q", input, got)
		}
	}
}

func TestInstallSSHUsesTheHostAddressForServerDialReport(t *testing.T) {
	// Address selection happens before SSH, so ServerDialAddress itself is the
	// focused unit boundary for tunnel normalization. Keep this assertion beside
	// the install tests to prevent callers from reverting to the SSH target host.
	got := ServerDialAddress("http://0.tcp.ap.ngrok.io:25408", ":19090")
	if got != "0.tcp.ap.ngrok.io:25408" {
		t.Fatalf("server dial report address = %q", got)
	}
}

func TestUdockerBundleArchiveFromDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "udocker"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	archive, err := udockerBundleArchive(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := tarGzFiles(t, archive)
	if files["bin/udocker"] != "#!/bin/sh\n" {
		t.Fatalf("archive files = %+v", files)
	}
}

func TestUdockerBundleArchiveFromFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := os.WriteFile(file, []byte("raw-archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	archive, err := udockerBundleArchive(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(archive) != "raw-archive" {
		t.Fatalf("archive = %q", string(archive))
	}
}

func TestInstallUdockerBundleWarnsWhenSourceMissing(t *testing.T) {
	installer := &SSHInstaller{}
	check, err := installer.installUdockerBundle(context.Background(), nil, "/opt/gpuworks/offline/udocker", "/opt/gpuworks")
	if err != nil {
		t.Fatal(err)
	}
	if check.Status != "warn" || !strings.Contains(check.text(), "udocker_bundle_source") {
		t.Fatalf("check = %+v", check)
	}
}

func TestConfigureProviderDNSOnlyTargetsAcceleratedCompshareRegions(t *testing.T) {
	if _, ok := configureProviderDNS(context.Background(), nil, "runpod", "cn-wlcb", false, ""); ok {
		t.Fatal("configureProviderDNS should skip non-Compshare providers")
	}
	if _, ok := configureProviderDNS(context.Background(), nil, "compshare", "cn-unknown", false, ""); ok {
		t.Fatal("configureProviderDNS should skip unsupported regions")
	}
}

func tarGzFiles(t *testing.T, data []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		if hdr.FileInfo().IsDir() {
			continue
		}
		var b bytes.Buffer
		if _, err := b.ReadFrom(tr); err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = b.String()
	}
	return out
}

func TestUploadShellCommandGzipsWireAndVerifiesDecompressedDigest(t *testing.T) {
	gz := uploadShellCommand("/opt/gpuworks/gpuworks-agent", "/opt/gpuworks/gpuworks-agent.new.1", "abc123", true, true)
	for _, want := range []string{
		"cat > '/opt/gpuworks/gpuworks-agent.new.1.gz'",
		"gunzip -c '/opt/gpuworks/gpuworks-agent.new.1.gz' > '/opt/gpuworks/gpuworks-agent.new.1'",
		"rm -f '/opt/gpuworks/gpuworks-agent.new.1.gz'",
		"sha256sum -c -",
		"chmod +x '/opt/gpuworks/gpuworks-agent'",
	} {
		if !strings.Contains(gz, want) {
			t.Fatalf("compressed upload command missing %q:\n%s", want, gz)
		}
	}
	// The digest check must run on the staged (decompressed) file, which is
	// what the sha256sum line references.
	if strings.Contains(gz, "sha256sum -c - && gunzip") {
		t.Fatalf("digest must verify the decompressed payload, not the gzip stream:\n%s", gz)
	}
	plain := uploadShellCommand("/tmp/bundle.tar.gz", "/tmp/bundle.tar.gz.new.2", "def456", false, false)
	if strings.Contains(plain, "gunzip") {
		t.Fatalf("already-compressed payloads must not be double-gzipped:\n%s", plain)
	}
	if !strings.Contains(plain, "cat > '/tmp/bundle.tar.gz.new.2'") {
		t.Fatalf("raw upload command missing plain staging:\n%s", plain)
	}
}

func TestGzipWireRoundTripCarriesOriginalPayload(t *testing.T) {
	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte(i)
	}
	var packed bytes.Buffer
	gz := gzip.NewWriter(&packed)
	if _, err := gz.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if packed.Len() >= len(payload) {
		t.Fatalf("gzip wire made the payload bigger: %d >= %d", packed.Len(), len(payload))
	}
	rd, err := gzip.NewReader(&packed)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rd)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("gzip wire round trip lost data")
	}
}

func TestDialTCPTunnelsThroughSocks5ProxyEnv(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := io.ReadFull(conn, make([]byte, 3)); err != nil { // greeting
			return
		}
		if _, err := conn.Write([]byte{5, 0}); err != nil { // no-auth selected
			return
		}
		head := make([]byte, 5) // VER CMD RSV ATYP NAMELEN
		if _, err := io.ReadFull(conn, head); err != nil || head[3] != 0x03 {
			return
		}
		if _, err := io.ReadFull(conn, make([]byte, head[4]+2)); err != nil { // name + port
			return
		}
		if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil { // accept, IPv4 bound
			return
		}
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		_, _ = conn.Write(buf[:n])
	}()
	SetSSHProxy("socks5://" + ln.Addr().String())
	t.Cleanup(func() { SetSSHProxy("") })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := dialTCP(ctx, "ssh.example:22", 4*time.Second)
	if err != nil {
		t.Fatalf("dialTCP through proxy: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("hello-tunnel")); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 12)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, echo); err != nil {
		t.Fatalf("tunnel echo: %v", err)
	}
	if string(echo) != "hello-tunnel" {
		t.Fatalf("echo = %q", echo)
	}
}

func TestDialTCPPropagatesProxyRefusalAndBadEnv(t *testing.T) {
	SetSSHProxy("socks5://127.0.0.1:1") // nothing listens
	t.Cleanup(func() { SetSSHProxy("") })
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err := dialTCP(ctx, "ssh.example:22", 2*time.Second); err == nil {
		t.Fatal("unreachable proxy reported success")
	}
	for _, bad := range []string{"http://zstack.one:7890", "socks5://zstack.one", "://", "socks5://zstack.one:notaport"} {
		SetSSHProxy(bad)
		if _, err := dialTCP(ctx, "ssh.example:22", time.Second); err == nil {
			t.Fatalf("bad proxy %q accepted", bad)
		}
	}
}
