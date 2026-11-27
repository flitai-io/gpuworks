package selfinstall

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// fakeRunner records every command an install or upgrade runs and answers from
// canned results. Commands are keyed by the string an operator would type
// (including the "sudo" the runner would add), which is also what a test
// asserts on.
type fakeRunner struct {
	mu     sync.Mutex
	calls  []Command
	output map[string]string
	fail   map[string]error
	// on runs for every command before its canned result, so a test can model a
	// service whose state changes as commands arrive.
	on func(cmd Command)
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{output: map[string]string{}, fail: map[string]error{}}
}

func (r *fakeRunner) Run(_ context.Context, cmd Command) error {
	// tee and rm act on the files they name, as the real commands do: the
	// systemd unit's presence is what later reads of the installation see.
	switch {
	case cmd.Path == "tee" && len(cmd.Args) == 1 && cmd.Stdin != nil && r.failure(cmd) == nil:
		content, _ := io.ReadAll(cmd.Stdin)
		cmd.Stdin = bytes.NewReader(content)
		if err := os.MkdirAll(filepath.Dir(cmd.Args[0]), 0o755); err == nil {
			_ = os.WriteFile(cmd.Args[0], content, 0o644)
		}
	case cmd.Path == "rm" && len(cmd.Args) == 2 && cmd.Args[0] == "-f" && r.failure(cmd) == nil:
		_ = os.Remove(cmd.Args[1])
	}
	r.mu.Lock()
	r.calls = append(r.calls, cmd)
	r.mu.Unlock()
	if r.on != nil {
		r.on(cmd)
	}
	return r.failure(cmd)
}

// failure looks up a canned failure. Keys match the whole command or a prefix
// of it ending on a word boundary, so a test can fail "sudo useradd" without
// spelling out the flags install happens to pass.
func (r *fakeRunner) failure(cmd Command) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	text := cmd.String()
	for key, err := range r.fail {
		if text == key || strings.HasPrefix(text, key+" ") {
			return err
		}
	}
	return nil
}

func (r *fakeRunner) Output(ctx context.Context, cmd Command) (string, error) {
	if err := r.Run(ctx, cmd); err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.output[cmd.String()], nil
}

// ran reports whether a command matching prefix was executed. A prefix ending
// in " " matches a whole word, so "sudo systemctl stop" does not match
// "sudo systemctl stop-something".
func (r *fakeRunner) ran(prefix string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, call := range r.calls {
		if call.String() == prefix || strings.HasPrefix(call.String(), prefix+" ") {
			return true
		}
	}
	return false
}

// ranInOrder reports whether the given prefixes appear in this relative order.
func (r *fakeRunner) ranInOrder(prefixes ...string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := 0
	for _, call := range r.calls {
		if next >= len(prefixes) {
			break
		}
		text := call.String()
		if text == prefixes[next] || strings.HasPrefix(text, prefixes[next]+" ") {
			next++
		}
	}
	return next == len(prefixes)
}

// writtenFile returns the content a command handed to `tee`, which is how a
// file outside the install tree (the systemd unit) is written when the operator
// is not root. The second result reports whether such a command ran.
func (r *fakeRunner) writtenFile(target string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, call := range r.calls {
		if call.Path != "tee" || len(call.Args) == 0 || call.Args[0] != target {
			continue
		}
		reader, ok := call.Stdin.(*bytes.Reader)
		if !ok {
			return "", false
		}
		content := make([]byte, reader.Size())
		if _, err := reader.ReadAt(content, 0); err != nil && err != io.EOF {
			return "", false
		}
		return string(content), true
	}
	return "", false
}

// executed returns the commands that ran, for failure messages.
func (r *fakeRunner) executed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.calls))
	for _, call := range r.calls {
		out = append(out, call.String())
	}
	return out
}

// site is an in-process release server: it publishes a release manifest and the
// artifacts the manifest describes, which is exactly what the real download
// site does.
type site struct {
	*httptest.Server
	Release Release
	// Bodies maps an artifact name to its bytes, so a test can corrupt one and
	// watch the checksum reject it.
	Bodies map[string][]byte
}

// newSite publishes a release holding one artifact for the running platform.
func newSite(t *testing.T, releaseID string, binary []byte) *site {
	t.Helper()
	goos, goarch := currentPlatform()
	name := fmt.Sprintf("gpuworks-%s-%s-%s.tar.gz", releaseID, goos, goarch)
	archive := tarGz(t, map[string]string{binaryName: string(binary)})
	s := &site{
		Release: Release{
			ReleaseID: releaseID,
			Commit:    "deadbeef",
			BuildAt:   "2026-01-01T00:00:00Z",
			Channel:   "preview",
			Files: []ReleaseFile{{
				OS: goos, Arch: goos + "-" + goarch, GoArch: goarch,
				Name: name, Size: int64(len(archive)), SHA256: sha256Hex(archive),
			}},
		},
		Bodies: map[string][]byte{name: archive},
	}
	mux := http.NewServeMux()
	mux.HandleFunc(releasePath, func(w http.ResponseWriter, _ *http.Request) {
		if err := json.NewEncoder(w).Encode(s.Release); err != nil {
			t.Errorf("encode release: %v", err)
		}
	})
	mux.HandleFunc(downloadPath, func(w http.ResponseWriter, req *http.Request) {
		body, ok := s.Bodies[strings.TrimPrefix(req.URL.Path, downloadPath)]
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(body)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// baseURL is what --base-url would be set to.
func (s *site) baseURL() string { return s.Server.URL }

// corrupt swaps the bytes served for an artifact without touching what the
// manifest promises, which is what a broken transfer looks like from here.
func (s *site) corrupt(name string, body []byte) {
	if _, ok := s.Bodies[name]; !ok {
		panic("no artifact named " + name)
	}
	s.Bodies[name] = body
}

// harness wires an Installer to fakes. Every field a test may want to steer is
// settable before the command runs; the zero value is a root install on a
// systemd host that answers health checks immediately.
type harness struct {
	t         *testing.T
	layout    Layout
	run       *fakeRunner
	installer *Installer
	deps      Deps
	site      *site

	io     *IO
	out    bytes.Buffer
	errOut bytes.Buffer

	mu        sync.Mutex
	systemd   bool
	launchd   bool
	terminal  bool
	healthErr error
	// healthVersion is the version /healthz reports; empty is a body that
	// names none, which the readiness wait accepts as any version.
	healthVersion string
	starts        [][]string
	nextPID       int
	freeBytes     uint64
	users         map[string]bool
	binaries      map[string]bool
	now           time.Time
	stdin         string
}

func newHarness(t *testing.T, dir string) *harness {
	t.Helper()
	// Temporary roots may be aliases (macOS /var/folders or a symlink TMPDIR).
	// Ordinary fixtures use canonical paths; symlink tests override them explicitly.
	layout := NewLayout(dir, "", "")
	canonical, err := resolveUninstallPath(layout.Dir)
	if err != nil {
		t.Fatalf("resolve fixture directory: %v", err)
	}
	layout = NewLayout(canonical, "", "")
	h := &harness{
		t:         t,
		layout:    layout,
		run:       newFakeRunner(),
		systemd:   true,
		nextPID:   4242,
		users:     map[string]bool{},
		binaries:  map[string]bool{"sudo": true, "systemctl": true, "useradd": true, "ps": true},
		now:       time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
		freeBytes: 10 << 30,
	}
	h.layout.UnitDir = mustEvalSymlinks(t, t.TempDir())
	h.layout.LaunchdDir = mustEvalSymlinks(t, t.TempDir())
	// A complete process-table fixture with no server from this installation.
	h.run.output["ps -ww -axo pid=,command="] = "1 /sbin/init\n"
	h.deps = Deps{
		Run: h.run,
		Now: func() time.Time { return h.now },
		Sleep: func(context.Context, time.Duration) {
			// Sleeps are counted in attempts, so tests never wait.
		},
		StartDetached: func(_ context.Context, cmd Command) (int, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.starts = append(h.starts, append([]string{cmd.Path}, cmd.Args...))
			return h.nextPID, nil
		},
		// The installer runs as an ordinary operator holding a sudo ticket. A
		// fake 0 here would send writes straight to /etc instead of through the
		// runner the test observes.
		Euid:       func() int { return 1000 },
		LookupUser: func(name string) bool { return h.users[name] },
		// The fixture tree lives under the test's 0700 temporary directory,
		// which only the account running the suite can enter.
		LookupAccount: func(string) (Account, error) {
			return Account{UID: strconv.Itoa(os.Getuid()), GroupIDs: []string{strconv.Itoa(os.Getgid())}}, nil
		},
		LookupGroup:    credentialGroupFixture,
		HasBinary:      func(name string) bool { return h.binaries[name] },
		SystemdRunning: func() bool { return h.systemd },
		LaunchdRunning: func() bool { return h.launchd },
		FreeBytes:      func(string) (uint64, error) { return h.freeBytes, nil },
		Health:         func(context.Context, string) (string, error) { return h.healthVersion, h.healthErr },
		HealthTimeout:  time.Second,
		StopTimeout:    time.Second,
		// A fresh install binds the address of the default route; a fixed one
		// keeps the suite independent of the host.
		Host: serverconfig.Host{RouteIP: func() (net.IP, error) { return net.ParseIP("192.0.2.10"), nil }},
	}
	h.io = &IO{
		In:         &scriptedInput{h: h},
		Out:        &h.out,
		Err:        &h.errOut,
		IsTerminal: func() bool { return h.terminal },
	}
	h.installer = New(h.layout, h.deps, h.io)
	return h
}

// scriptedInput serves the interactive answers a test wrote into h.stdin.
type scriptedInput struct{ h *harness }

func (r *scriptedInput) Read(p []byte) (int, error) {
	if r.h.stdin == "" {
		// A terminal whose input ran out reads like one where the operator
		// pressed Ctrl-D: an empty answer.
		return 0, io.EOF
	}
	n := copy(p, r.h.stdin)
	r.h.stdin = r.h.stdin[n:]
	return n, nil
}

// useSite publishes a release and points the installer's HTTP client at it.
func (h *harness) useSite(releaseID string, binary []byte) *site {
	h.site = newSite(h.t, releaseID, binary)
	h.deps.Client = h.site.Client()
	h.installer = New(h.layout, h.deps, h.io)
	return h.site
}

// installSelf points the installer at a fixture file instead of the test binary.
func (h *harness) installSelf(content string) string {
	path := filepath.Join(h.t.TempDir(), "gpuworks-source")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		h.t.Fatalf("write fixture binary: %v", err)
	}
	h.installer.WithSelf(path)
	return path
}

// quiet returns an install request that never prompts. It carries the defaults
// the command line supplies when the operator leaves the flags out.
func (h *harness) quiet() InstallRequest {
	return InstallRequest{
		Quiet:          true,
		Addr:           h.freeAddr(),
		AddrSet:        true,
		TrustedProxies: DefaultTrustedProxies,
	}
}

// freeAddr is a loopback address nothing is listening on. Preflight really
// binds the port, so a fixed one would make the suite depend on the host.
func (h *harness) freeAddr() string {
	h.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		h.t.Fatalf("reserve a port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// unitFile is the unit content install handed to systemd, whether the process
// wrote it directly or through `sudo tee`.
func (h *harness) unitFile() string {
	h.t.Helper()
	if content, ok := h.run.writtenFile(h.layout.UnitPath()); ok {
		return content
	}
	return readFile(h.t, h.layout.UnitPath())
}

// mustInstall runs an install that is expected to succeed.
func (h *harness) mustInstall(req InstallRequest) InstallReport {
	h.t.Helper()
	report, err := h.installer.Install(context.Background(), req)
	if err != nil {
		h.t.Fatalf("install: %v (stderr: %s)", err, h.errOut.String())
	}
	return report
}

// existingInstall is what writeExistingInstall laid down.
type existingInstall struct {
	ReleaseID string
	Addr      string
}

// writeExistingInstall lays down what an earlier install would have left: the
// binary (which reports releaseID), server.yaml, a systemd unit running as the
// service account, and a database with an admin account in it.
func (h *harness) writeExistingInstall(releaseID string) existingInstall {
	h.t.Helper()
	if err := os.MkdirAll(h.layout.DataDir, 0o755); err != nil {
		h.t.Fatalf("mkdir data: %v", err)
	}
	if err := os.MkdirAll(h.layout.Dir, 0o755); err != nil {
		h.t.Fatalf("mkdir dir: %v", err)
	}
	writeFile(h.t, h.layout.Binary(), "old binary", 0o755)
	writeFile(h.t, filepath.Join(h.layout.DataDir, "gpuworks.db"), "old database", 0o600)
	writeFile(h.t, filepath.Join(h.layout.DataDir, "master.key"), "old key", 0o600)
	// Upgrade really binds the address after stopping the service, so a fixed
	// port would make the suite depend on what the host runs.
	existing := existingInstall{ReleaseID: releaseID, Addr: h.freeAddr()}
	h.writeConfig(serverconfig.File{
		Listen:  serverconfig.Listen{Console: []string{existing.Addr}},
		DataDir: h.layout.DataDir,
		Log:     serverconfig.Log{File: h.layout.LogFile(), MaxSize: DefaultLogMaxSize, Keep: DefaultLogKeep},
	})
	writeFile(h.t, h.layout.UnitPath(), RenderUnit(UnitConfig{
		Binary: h.layout.Binary(), ConfigPath: h.layout.ConfigPath(), DataDir: h.layout.DataDir, ServiceUser: DefaultServiceUser,
	}), 0o644)
	h.reportsRelease(releaseID)
	return existing
}

// writeConfig replaces the installation's server.yaml.
func (h *harness) writeConfig(f serverconfig.File) {
	h.t.Helper()
	if err := serverconfig.Write(h.layout.ConfigPath(), f, 0o664); err != nil {
		h.t.Fatalf("write server.yaml: %v", err)
	}
}

// config reads the installation's server.yaml.
func (h *harness) config() serverconfig.File {
	h.t.Helper()
	f, err := serverconfig.Load(h.layout.ConfigPath())
	if err != nil {
		h.t.Fatalf("read server.yaml: %v", err)
	}
	return f
}

// reportsRelease makes the installed binary answer `version` with releaseID.
func (h *harness) reportsRelease(releaseID string) {
	h.run.mu.Lock()
	defer h.run.mu.Unlock()
	h.run.output[h.layout.Binary()+" version"] = "gpuworks " + releaseID + " (deadbeef, 2026-01-01)\n"
}

// supervisedByNohup removes the unit, which is what marks the systemd mode.
func (h *harness) supervisedByNohup() {
	h.t.Helper()
	if err := os.Remove(h.layout.UnitPath()); err != nil && !os.IsNotExist(err) {
		h.t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// tarGz builds a release archive from member name to content.
func tarGz(t *testing.T, members map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	writer := tar.NewWriter(gz)
	names := make([]string, 0, len(members))
	for name := range members {
		names = append(names, name)
	}
	for _, name := range names {
		content := members[name]
		header := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatalf("tar body %s: %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// The runner maps service identity to this process for local filesystem tests.
// The group lookup must report that same identity, rather than inventing a
// successful chown to an unrelated host account.
func credentialGroupFixture(name string) (*user.Group, error) {
	if name != DefaultCredentialGroup {
		return nil, user.UnknownGroupError(name)
	}
	return &user.Group{Name: name, Gid: strconv.Itoa(os.Getegid())}, nil
}
