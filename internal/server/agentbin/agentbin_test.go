package agentbin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"testing/fstest"

	"github.com/klauspost/compress/zstd"
)

func TestReadPrefersOverrideDir(t *testing.T) {
	dir := t.TempDir()
	name := "gpuworks-agent-linux-test"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("override"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := bundled.Read(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "override" {
		t.Fatalf("Read = %q", string(got))
	}
}

func TestReadRejectsTraversal(t *testing.T) {
	if _, err := bundled.Read(t.TempDir(), "../secret"); err == nil {
		t.Fatal("Read should reject traversal")
	}
}

// testTree is an embedded dl/ as a release build leaves it: one agent stored
// zstd-compressed (as zstdpack writes it), one plain (a test's own build).
func testTree(t *testing.T, agent []byte) fstest.MapFS {
	t.Helper()
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	return fstest.MapFS{
		"dl/gpuworks-agent-linux-amd64.zst": &fstest.MapFile{Data: enc.EncodeAll(agent, nil)},
		"dl/gpuworks-agent-linux-arm64":     &fstest.MapFile{Data: []byte("plain arm64 agent")},
	}
}

func useTree(t *testing.T, tree fs.FS) {
	t.Helper()
	saved := bundled
	bundled = Files{tree}
	t.Cleanup(func() { bundled = saved })
}

func TestACompressedAgentIsServedExpanded(t *testing.T) {
	agent := bytes.Repeat([]byte("ELF agent bytes "), 4096)
	useTree(t, testTree(t, agent))

	got, err := bundled.Read("", "gpuworks-agent-linux-amd64")
	if err != nil || !bytes.Equal(got, agent) {
		t.Fatalf("Read = %d bytes, %v", len(got), err)
	}
	res := httptest.NewRecorder()
	bundled.Handler("").ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/dl/gpuworks-agent-linux-amd64", nil))
	if res.Code != http.StatusOK || !bytes.Equal(res.Body.Bytes(), agent) {
		t.Fatalf("GET = %d, %d bytes", res.Code, res.Body.Len())
	}
	// The length is known before the body is expanded: curl and the agent's
	// self-upgrade see an ordinary download.
	if got := res.Header().Get("Content-Length"); got != strconv.Itoa(len(agent)) {
		t.Fatalf("Content-Length = %q, want %d", got, len(agent))
	}
	if !bundled.Has("gpuworks-agent-linux-amd64") {
		t.Fatal("Has does not see the compressed agent")
	}
}

func TestAPlainAgentIsServedAsItIs(t *testing.T) {
	useTree(t, testTree(t, []byte("x")))
	res := httptest.NewRecorder()
	bundled.Handler("").ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/dl/gpuworks-agent-linux-arm64", nil))
	if res.Code != http.StatusOK || res.Body.String() != "plain arm64 agent" || res.Header().Get("Content-Length") != "17" {
		t.Fatalf("GET = %d %q %v", res.Code, res.Body.String(), res.Header())
	}
}

// The compressed file itself is not a name a node can ask for, nor is anything
// outside dl/; an unknown agent is a 404.
func TestOnlyAgentNamesAreServed(t *testing.T) {
	useTree(t, testTree(t, []byte("x")))
	for _, name := range []string{"gpuworks-agent-linux-amd64.zst", "../agentbin.go", "gpuworks-agent-darwin-amd64", ""} {
		res := httptest.NewRecorder()
		bundled.Handler("").ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/dl/"+name, nil))
		if res.Code != http.StatusNotFound {
			t.Errorf("%q: status %d", name, res.Code)
		}
		if _, err := bundled.Read("", name); err == nil {
			t.Errorf("%q: Read succeeded", name)
		}
		if bundled.Has(name) {
			t.Errorf("%q: Has", name)
		}
	}
}

// Check every platform advertised by the actual build, including delta frames.
func TestTheEmbeddedAgentsExpand(t *testing.T) {
	raw, err := fs.ReadFile(embedded, "dl/"+ManifestName)
	if errors.Is(err, fs.ErrNotExist) {
		if _, err := fs.Stat(embedded, "dl/pack"); err == nil {
			t.Fatal("packed build has no manifest")
		}
		return // source-only build
	}
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err = json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	files, err := Prepare(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name := range m.Binaries {
		if _, err := files.ReadVerified("", name, m.Version); err != nil {
			t.Fatal(err)
		}
	}
}
