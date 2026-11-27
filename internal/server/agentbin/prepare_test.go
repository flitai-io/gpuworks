package agentbin

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"sync"
	"testing"
	"testing/fstest"

	"github.com/klauspost/compress/zstd"
)

// Make a real dictionary frame; raw targets are independent references for
// the HTTP, provisioning and verified upgrade readers.
func packedTree(t *testing.T, names ...string) (fstest.MapFS, map[string][]byte) {
	t.Helper()
	tree := fstest.MapFS{packDir + "base": {Data: []byte(names[0])}}
	m := Manifest{Version: "fixture", Binaries: map[string]ManifestBinary{}}
	raw := map[string][]byte{}
	for i, name := range names {
		data := bytes.Repeat([]byte("shared agent instructions and data\n"), 4096)
		copy(data[len(data)/2:], name)
		opts := []zstd.EOption{zstd.WithEncoderConcurrency(1)}
		if i > 0 {
			opts = append(opts, zstd.WithEncoderDictRaw(0, raw[names[0]]))
		}
		enc, err := zstd.NewWriter(nil, opts...)
		if err != nil {
			t.Fatal(err)
		}
		tree[packDir+name+compressedSuffix] = &fstest.MapFile{Data: enc.EncodeAll(data, nil)}
		enc.Close()
		raw[name] = data
		m.Binaries[name] = ManifestBinary{Size: int64(len(data)), SHA256: fmt.Sprintf("%x", sha256.Sum256(data))}
	}
	manifest, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	tree["dl/"+ManifestName] = &fstest.MapFile{Data: manifest}
	return tree, raw
}

func TestPreparedAgentsServeVerifiedFilesUntilRestart(t *testing.T) {
	const base, target = "gpuworks-agent-arm", "gpuworks-agent-other"
	tree, raw := packedTree(t, base, target)
	input := Files{tree}
	dir := t.TempDir()
	files, err := input.prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !files.Available("", target, "fixture") {
		t.Fatal("cold preparation left the delta agent unavailable")
	}
	// No request or warm restart should need to open either packed frame.
	delete(tree, packDir+base+compressedSuffix)
	delete(tree, packDir+target+compressedSuffix)
	files, err = input.prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range raw {
		if !files.Available("", name, "fixture") {
			t.Fatalf("%s unavailable", name)
		}
		r := httptest.NewRecorder()
		files.Handler("").ServeHTTP(r, httptest.NewRequest("GET", "/dl/"+name, nil))
		if r.Code != 200 || r.Header().Get("Content-Length") != strconv.Itoa(len(want)) || !bytes.Equal(r.Body.Bytes(), want) {
			t.Fatalf("%s download mismatch", name)
		}
		got, err := files.ReadVerified("", name, "fixture")
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("verified %s: %v", name, err)
		}
	}
	for _, path := range []string{"pack/base", "pack/" + target + compressedSuffix, ".prepare-x"} {
		r := httptest.NewRecorder()
		files.Handler("").ServeHTTP(r, httptest.NewRequest("GET", "/dl/"+path, nil))
		if r.Code != http.StatusNotFound {
			t.Fatalf("private path %s: %d", path, r.Code)
		}
	}
	if _, err := files.ReadVerified("", target, "previous"); err == nil {
		t.Fatal("accepted another build")
	}
}

func TestPreparationRepairsDamageAndSupportsVerifiedRecovery(t *testing.T) {
	const base = "gpuworks-agent-single"
	tree, raw := packedTree(t, base)
	dir := t.TempDir()
	input := Files{tree}
	_, err := input.prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := filepath.Glob(filepath.Join(dir, "agent-cache", "*", base))
	if err != nil || len(cache) != 1 {
		t.Fatalf("cache paths %v: %v", cache, err)
	}
	damage := bytes.Repeat([]byte("!"), len(raw[base]))
	if err := os.WriteFile(cache[0], damage, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := input.prepare(dir); err != nil {
		t.Fatal(err)
	}
	check, err := os.ReadFile(cache[0])
	if err != nil || !bytes.Equal(check, raw[base]) {
		t.Fatal("cache repair failed")
	}
	if err := os.Remove(cache[0]); err != nil {
		t.Fatal(err)
	}
	tree[packDir+base+compressedSuffix].Data = []byte("corrupt")
	if _, err := input.prepare(dir); err == nil {
		t.Fatal("accepted corrupt generation")
	}
	override := filepath.Join(dir, "dl")
	if err := os.Mkdir(override, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(override, base), raw[base], 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := input.prepare(dir); err != nil {
		t.Fatalf("same-build executable alone must recover strict startup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(override, ManifestName), tree["dl/"+ManifestName].Data, 0600); err != nil {
		t.Fatal(err)
	}
	files, err := input.prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := files.ReadVerified("", base, "fixture"); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedGenerationsAndConcurrentStartup(t *testing.T) {
	const base, target = "gpuworks-agent-one", "gpuworks-agent-two"
	tree, raw := packedTree(t, base, target)
	dir := t.TempDir()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := (Files{tree}).prepare(dir); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	old, err := (Files{tree}).prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	single, _ := packedTree(t, base)
	files, err := (Files{single}).prepare(dir)
	if err != nil || files.Has(target) {
		t.Fatalf("single-platform generation contains stale target: %v", err)
	}
	if got, err := old.ReadVerified("", target, "fixture"); err != nil || !bytes.Equal(got, raw[target]) {
		t.Fatal("previous generation lost")
	}
	delete(single, packDir+"base")
	if _, err := (Files{single}).prepare(dir); err == nil {
		t.Fatal("accepted a pack without a base declaration")
	}
	loose := Files{fstest.MapFS{"dl/" + target: {Data: []byte("developer fixture")}}}
	dev, err := loose.prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := dev.Read("", target)
	if err != nil || string(got) != "developer fixture" {
		t.Fatal("lost loose build")
	}
	if _, err := fs.Stat(old.tree, "dl/"+target); err != nil {
		t.Fatal(err)
	}
}

func TestServerDegradedStartupAndLiveRecovery(t *testing.T) {
	const base, bad, good = "gpuworks-agent-base", "gpuworks-agent-bad", "gpuworks-agent-good"
	tree, raw := packedTree(t, base, bad, good)
	tree[packDir+bad+compressedSuffix].Data = []byte("broken frame")
	useTree(t, tree)
	dir := t.TempDir()
	dl := filepath.Join(dir, "dl")
	if err := os.Mkdir(dl, 0700); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	for _, v := range []string{"", "false", "typo"} {
		t.Setenv(degradedStartupEnv, v)
		if _, err := PrepareForServer(dir, log); err == nil {
			t.Fatalf("allowed startup with %q", v)
		}
	}
	t.Setenv(degradedStartupEnv, "1")
	files, err := PrepareForServer(dir, log)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"agent restoration failed", bad, filepath.Join(dl, bad), "expected_sha256", "allow_degraded_startup=true"} {
		if !strings.Contains(logs.String(), s) {
			t.Fatalf("startup log missing %q", s)
		}
	}
	download := func(name string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		files.Handler(dl).ServeHTTP(r, httptest.NewRequest("GET", "/dl/"+name, nil))
		return r
	}
	if r := download(good); r.Code != 200 || !bytes.Equal(r.Body.Bytes(), raw[good]) {
		t.Fatal("healthy target stopped working")
	}
	checkUnavailable := func() {
		t.Helper()
		if r := download(bad); r.Code != http.StatusServiceUnavailable {
			t.Fatalf("unavailable HTTP status %d", r.Code)
		}
		if files.Available(dl, bad, "fixture") {
			t.Fatal("upgrade offered unavailable binary")
		}
		if _, err := files.Read(dl, bad); !errors.Is(err, errAgentUnavailable) || errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("offline build should retry instead of caching a partial package: %v", err)
		}
	}
	checkUnavailable()
	if err := os.WriteFile(filepath.Join(dl, bad), raw[good], 0600); err != nil {
		t.Fatal(err)
	}
	checkUnavailable()
	if err := os.WriteFile(filepath.Join(dl, bad), raw[bad], 0600); err != nil {
		t.Fatal(err)
	}
	if r := download(bad); r.Code != 200 || !bytes.Equal(r.Body.Bytes(), raw[bad]) {
		t.Fatal("manual binary not served")
	}
	if !files.Available(dl, bad, "fixture") {
		t.Fatal("manual recovery unavailable for upgrade")
	}
	got, err := files.ReadVerified(dl, bad, "fixture")
	if err != nil || !bytes.Equal(got, raw[bad]) {
		t.Fatalf("manual upgrade read: %v", err)
	}
	t.Setenv(degradedStartupEnv, "false")
	strict, err := PrepareForServer(dir, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strict.ReadVerified(dl, bad, "fixture"); err != nil {
		t.Fatal(err)
	}
}

func TestFailedReferenceCannotExposeCorruptCachedTarget(t *testing.T) {
	const base, target = "gpuworks-agent-reference", "gpuworks-agent-target"
	tree, raw := packedTree(t, base, target)
	f := Files{tree}
	dir := t.TempDir()
	if _, err := f.prepare(dir); err != nil {
		t.Fatal(err)
	}
	gen, _ := filepath.Glob(filepath.Join(dir, "agent-cache", "*"))
	for _, name := range []string{base, target} {
		if err := os.WriteFile(filepath.Join(gen[0], name), []byte("bad cache"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	tree[packDir+base+compressedSuffix].Data = []byte("bad base")
	files, err := f.preparePack(dir, true, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if files.Has(target) || files.Has(base) {
		t.Fatal("offered broken cached bytes")
	}
	dl := filepath.Join(dir, "dl")
	if err := os.Mkdir(dl, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dl, target), raw[target], 0600); err != nil {
		t.Fatal(err)
	}
	got, err := files.ReadVerified(dl, target, "fixture")
	if err != nil || !bytes.Equal(got, raw[target]) {
		t.Fatal("target must recover independently of base")
	}
}

func TestUnavailableCacheCanUseACompleteManualAgent(t *testing.T) {
	const name = "gpuworks-agent-one"
	tree, raw := packedTree(t, name)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent-cache"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	dl := filepath.Join(dir, "dl")
	if err := os.Mkdir(dl, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dl, name), raw[name], 0600); err != nil {
		t.Fatal(err)
	}
	files, err := (Files{tree}).prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := files.ReadVerified(dl, name, "fixture")
	if err != nil || !bytes.Equal(got, raw[name]) {
		t.Fatalf("verified recovery with unavailable cache: %v", err)
	}
}

func TestManifestWithoutPackRequiresVerifiedLegacyFiles(t *testing.T) {
	const base, other = "gpuworks-agent-one", "gpuworks-agent-two"
	tree, raw := packedTree(t, base, other)
	// The reference's standalone frame is also the old .zst format.
	tree["dl/"+base+compressedSuffix] = tree[packDir+base+compressedSuffix]
	tree["dl/"+base] = &fstest.MapFile{Data: []byte("obsolete loose fixture")}
	for name := range tree {
		if strings.HasPrefix(name, packDir) {
			delete(tree, name)
		}
	}
	input := Files{tree}
	dir := t.TempDir()
	if _, err := input.prepare(dir); err == nil {
		t.Fatal("startup accepted a missing manifest member")
	}
	files, err := input.preparePack(dir, true, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if files.Available("", other, "fixture") || !files.Available("", base, "fixture") {
		t.Fatal("incorrect legacy platform availability")
	}
	tree["dl/"+other] = &fstest.MapFile{Data: raw[base]}
	if _, err := input.prepare(dir); err == nil {
		t.Fatal("startup accepted a legacy file with the wrong hash")
	}
	tree["dl/"+other].Data = raw[other]
	files, err = input.prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range raw {
		got, err := files.ReadVerified("", name, "fixture")
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("legacy %s: %v", name, err)
		}
	}
}

func TestManifestMembersAlwaysUseThePreparedSource(t *testing.T) {
	const name = "gpuworks-agent-one"
	tree, raw := packedTree(t, name)
	tree["dl/"+name] = &fstest.MapFile{Data: []byte("stale build output")}
	input := Files{tree}
	dir := t.TempDir()
	files, err := input.prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	get := func(overrides string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		files.Handler(overrides).ServeHTTP(r, httptest.NewRequest("GET", "/dl/"+name, nil))
		return r
	}
	if r := get(""); r.Code != 200 || !bytes.Equal(r.Body.Bytes(), raw[name]) {
		t.Fatal("download bypassed verified source")
	}
	if _, err := files.ReadVerified("", name, "fixture"); err != nil {
		t.Fatal(err)
	}
	override := filepath.Join(dir, "dl")
	if err := os.Mkdir(override, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(override, name), []byte("operator"), 0600); err != nil {
		t.Fatal(err)
	}
	if r := get(override); r.Code != 200 || r.Body.String() != "operator" {
		t.Fatal("lost explicit override")
	}
	// A stale loose file must not make a failed platform look available, either.
	delete(tree, packDir+name+compressedSuffix)
	files, err = input.preparePack(t.TempDir(), true, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if r := get(""); r.Code != 503 || files.Has(name) {
		t.Fatal("failed platform used a stale fixture")
	}
}

func TestRecoveryLogContainsBothFailures(t *testing.T) {
	const name = "gpuworks-agent-one"
	for _, tc := range []struct{ label, version, reason string }{
		{"wrong standalone bytes", "", "SHA256"},
		{"wrong manifest version", "other", "does not match target"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			tree, raw := packedTree(t, name)
			tree[packDir+name+compressedSuffix].Data = []byte("broken")
			dir := t.TempDir()
			dl := filepath.Join(dir, "dl")
			if err := os.Mkdir(dl, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dl, name), bytes.Repeat([]byte("!"), len(raw[name])), 0600); err != nil {
				t.Fatal(err)
			}
			if tc.version != "" {
				if err := os.WriteFile(filepath.Join(dl, ManifestName), manifestBytes(t, tc.version, name, raw[name]), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var buf bytes.Buffer
			_, err := (Files{tree}).preparePack(dir, false, slog.New(slog.NewTextHandler(&buf, nil)))
			if err == nil {
				t.Fatal("accepted invalid recovery")
			}
			for _, s := range []string{name, "recovery_error", tc.reason, "magic"} {
				if !strings.Contains(buf.String(), s) {
					t.Fatalf("missing %q in %s", s, buf.String())
				}
			}
		})
	}
}
