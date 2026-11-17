package storagefiles

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func download(t *testing.T, e *testEnv, rel, format string) (proto.StorageDownloadHead, []byte, error) {
	t.Helper()
	var out bytes.Buffer
	err := e.svc.Download(bg, proto.StreamHeader{Kind: proto.StreamStorageDownload, StorageID: 1, Rel: rel, Format: format}, &out)
	r := bufio.NewReader(&out)
	line, rerr := r.ReadBytes('\n')
	if rerr != nil {
		t.Fatalf("no head line: %q", out.String())
	}
	var head proto.StorageDownloadHead
	if err := json.Unmarshal(line, &head); err != nil {
		t.Fatal(err)
	}
	rest, _ := io.ReadAll(r)
	if head.Code != "" {
		if len(rest) != 0 {
			t.Fatalf("bytes after a refusal: %q", rest)
		}
		if err == nil || codeOf(t, err) != head.Code || head.Error != err.Error() {
			t.Fatalf("head %+v vs returned %v", head, err)
		}
	}
	return head, rest, err
}

func zipContents(t *testing.T, b []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = string(body)
	}
	return out
}

func tarGzContents(t *testing.T, b []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeDir && h.Typeflag != tar.TypeReg {
			t.Fatalf("archive entry %s has type %q", h.Name, h.Typeflag)
		}
		body, _ := io.ReadAll(tr)
		out[h.Name] = string(body)
	}
}

func keys(m map[string]string) string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestDownloadAFileAsIs(t *testing.T) {
	e := newEnv(t)
	writeFile(t, e.path("models/m/config.json"), `{"a":1}`)
	head, body, err := download(t, e, "models/m/config.json", "")
	if err != nil {
		t.Fatal(err)
	}
	if head != (proto.StorageDownloadHead{Size: 7, Name: "config.json"}) || string(body) != `{"a":1}` {
		t.Fatalf("head %+v body %q", head, body)
	}
	// Read-only storages can still be downloaded from.
	e.r.Writable = false
	if _, _, err := download(t, e, "models/m/config.json", ""); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadADirectoryPacksItWithoutFollowingLinks(t *testing.T) {
	e := newEnv(t)
	writeFile(t, e.path("files/ds/a.txt"), "A")
	writeFile(t, e.path("files/ds/sub/b.txt"), "B")
	if err := os.MkdirAll(e.path("files/ds/empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, filepath.Join(e.outside, "secret.txt"), e.path("files/ds/leak"))
	symlink(t, e.outside, e.path("files/ds/sub/leakdir"))
	if err := syscall.Mkfifo(e.path("files/ds/pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantSkipped := "leak\npipe (not a regular file)\nsub/leakdir\n"

	head, body, err := download(t, e, "files/ds", "")
	if err != nil {
		t.Fatal(err)
	}
	if head != (proto.StorageDownloadHead{Dir: true, Size: -1, Name: "ds"}) {
		t.Fatalf("head = %+v", head)
	}
	got := zipContents(t, body)
	if keys(got) != "ds/,ds/SKIPPED_SYMLINKS.txt,ds/a.txt,ds/empty/,ds/sub/,ds/sub/b.txt" {
		t.Fatalf("zip entries = %s", keys(got))
	}
	if got["ds/a.txt"] != "A" || got["ds/sub/b.txt"] != "B" || got["ds/SKIPPED_SYMLINKS.txt"] != wantSkipped {
		t.Fatalf("zip contents = %q", got)
	}
	for _, v := range got {
		if strings.Contains(v, "secret") {
			t.Fatal("a link was followed")
		}
	}

	head, body, err = download(t, e, "files/ds", proto.StorageArchiveTarGz)
	if err != nil || !head.Dir {
		t.Fatalf("tar.gz: %+v %v", head, err)
	}
	got = tarGzContents(t, body)
	if keys(got) != "ds/,ds/SKIPPED_SYMLINKS.txt,ds/a.txt,ds/empty/,ds/sub/,ds/sub/b.txt" || got["ds/SKIPPED_SYMLINKS.txt"] != wantSkipped {
		t.Fatalf("tar.gz contents = %q", got)
	}
	// Plain tar is accepted too.
	if _, body, err := download(t, e, "files/ds/sub", proto.StorageArchiveTar); err != nil || !bytes.Contains(body, []byte("sub/b.txt")) {
		t.Fatalf("tar: %v", err)
	}
}

func TestDownloadKeepsAUserFileNamedLikeTheSkipList(t *testing.T) {
	e := newEnv(t)
	writeFile(t, e.path("files/ds/"+SkippedLinksFile), "mine")
	symlink(t, e.outside, e.path("files/ds/out"))
	_, body, err := download(t, e, "files/ds", "")
	if err != nil {
		t.Fatal(err)
	}
	got := zipContents(t, body)
	if got["ds/"+SkippedLinksFile] != "mine" || got["ds/SKIPPED_SYMLINKS-1.txt"] != "out\n" {
		t.Fatalf("contents = %q", got)
	}
}

func TestDownloadTheRootAndEmptyDirectories(t *testing.T) {
	e := newEnv(t)
	writeFile(t, e.path("files/a.txt"), "A")
	head, body, err := download(t, e, "", proto.StorageArchiveZip)
	if err != nil || head.Name != "storage" || !head.Dir {
		t.Fatalf("root: %+v %v", head, err)
	}
	if got := zipContents(t, body); got["storage/files/a.txt"] != "A" {
		t.Fatalf("root contents = %q", got)
	}
}

func TestDownloadRefusals(t *testing.T) {
	e := newEnv(t)
	writeFile(t, e.path("files/a.txt"), "A")
	symlink(t, filepath.Join(e.outside, "secret.txt"), e.path("files/leak"))
	symlink(t, e.path("files/a.txt"), e.path("files/inner-link"))
	symlink(t, e.outside, e.path("files/outdir"))
	if err := syscall.Mkfifo(e.path("files/pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"files/missing":           proto.StorageFilesErrNotFound,
		"files/a.txt/x":           proto.StorageFilesErrNotFound,
		"files/leak":              proto.StorageFilesErrPathInvalid,
		"files/inner-link":        proto.StorageFilesErrPathInvalid, // links are not followed, even inside the storage
		"files/outdir":            proto.StorageFilesErrPathInvalid,
		"files/outdir/secret.txt": proto.StorageFilesErrPathInvalid,
		"../outside/secret.txt":   proto.StorageFilesErrPathInvalid,
		"files/pipe":              proto.StorageFilesErrPathInvalid,
	}
	for rel, code := range cases {
		head, _, err := download(t, e, rel, "")
		if head.Code != code {
			t.Errorf("download %q: head %+v (%v), want %s", rel, head, err, code)
		}
	}
	head, _, _ := download(t, e, "files", "rar")
	if head.Code != proto.StorageFilesErrFailed {
		t.Fatalf("unknown format: %+v", head)
	}
	e.r.Ready = false
	head, _, _ = download(t, e, "files/a.txt", "")
	if head.Code != proto.StorageFilesErrNotWritable {
		t.Fatalf("storage not ready: %+v", head)
	}
	e.r.Ready = true
	e.doErr = errors.New("guard says no")
	head, _, _ = download(t, e, "files/a.txt", "")
	if head.Code != proto.StorageFilesErrFailed || !strings.Contains(head.Error, "guard says no") {
		t.Fatalf("guard failure: %+v", head)
	}
}

// failingWriter accepts n bytes, then fails.
type failingWriter struct {
	n   int
	buf bytes.Buffer
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.n {
		return 0, errors.New("server went away")
	}
	return w.buf.Write(p)
}

func TestDownloadFailuresAfterTheHeadAreNotRefusals(t *testing.T) {
	e := newEnv(t)
	writeFile(t, e.path("files/big.bin"), strings.Repeat("x", 1<<20))
	writeFile(t, e.path("files/ds/a.bin"), strings.Repeat("y", 1<<20))
	for _, rel := range []string{"files/big.bin", "files/ds"} {
		w := &failingWriter{n: 100} // past the head, before the end of even a compressed archive
		err := e.svc.Download(bg, proto.StreamHeader{StorageID: 1, Rel: rel}, w)
		var fe *proto.StorageFilesError
		if err == nil || errors.As(err, &fe) {
			t.Fatalf("%s: a broken transfer returned %v; it must not look like a refusal", rel, err)
		}
		if !bytes.HasPrefix(w.buf.Bytes(), []byte(`{"dir":`)) {
			t.Fatalf("%s: the head was not sent first: %q", rel, w.buf.String()[:20])
		}
	}
	// A head that cannot be written.
	if err := e.svc.Download(bg, proto.StreamHeader{StorageID: 1, Rel: "files/big.bin"}, &failingWriter{}); err == nil {
		t.Fatal("unwritable head succeeded")
	}
	// A refusal whose head cannot be written is still a refusal.
	err := e.svc.Download(bg, proto.StreamHeader{StorageID: 1, Rel: "files/missing"}, &failingWriter{})
	wantCode(t, err, proto.StorageFilesErrNotFound)
	// Cancelled while packing.
	ctx, cancel := context.WithCancel(bg)
	cancel()
	for _, rel := range []string{"files/ds", "files/big.bin"} {
		err = e.svc.Download(ctx, proto.StreamHeader{StorageID: 1, Rel: rel}, io.Discard)
		var fe *proto.StorageFilesError
		if !errors.Is(err, context.Canceled) || errors.As(err, &fe) {
			t.Fatalf("%s cancelled: %v", rel, err)
		}
	}
	// A file that shrinks after the head was sent fails instead of sending
	// fewer bytes than announced.
	src, err := e.svc.openDownload(proto.StreamHeader{StorageID: 1, Rel: "files/big.bin"})
	if err != nil {
		t.Fatal(err)
	}
	src.file.Close()
	if err := os.Truncate(e.path("files/big.bin"), 10); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(e.path("files/big.bin"))
	defer f.Close()
	if _, err := io.CopyN(io.Discard, f, src.head.Size); err == nil {
		t.Fatal("copying a shrunk file reported no error")
	}
}

func TestSweepIncomingRemovesOnlyStaleUploadStaging(t *testing.T) {
	e := newEnv(t)
	if n, err := SweepIncoming(e.root, IncomingMaxAge, time.Now()); n != 0 || err != nil {
		t.Fatalf("no incoming directory: %d %v", n, err)
	}
	old := time.Now().Add(-48 * time.Hour)
	stale := e.path(".gpuworks/incoming/cp1/upload-stale")
	writeFile(t, filepath.Join(stale, "body"), "x")
	busy := e.path(".gpuworks/incoming/cp2/upload-busy") // old directory, body still being written
	writeFile(t, filepath.Join(busy, "body"), "x")
	fresh := e.path(".gpuworks/incoming/cp1/upload-fresh")
	writeFile(t, filepath.Join(fresh, "body"), "x")
	model := e.path(".gpuworks/incoming/cp1/models/llama") // a model write's staging: not ours
	writeFile(t, filepath.Join(model, "config.json"), "{}")
	writeFile(t, e.path(".gpuworks/incoming/stray-file"), "x")
	for _, p := range []string{filepath.Join(stale, "body"), stale, busy, model, filepath.Join(model, "config.json")} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	n, err := SweepIncoming(e.root, IncomingMaxAge, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("swept %d, %v", n, err)
	}
	if exists(stale) || !exists(busy) || !exists(fresh) || !exists(model) {
		t.Fatalf("stale=%v busy=%v fresh=%v model=%v", exists(stale), exists(busy), exists(fresh), exists(model))
	}
	if os.Geteuid() != 0 {
		locked := e.path(".gpuworks/incoming/cp3")
		if err := os.MkdirAll(locked, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		if _, err := SweepIncoming(e.root, IncomingMaxAge, time.Now()); err == nil {
			t.Fatal("an unreadable control-plane directory reported no error")
		}
	}
}
