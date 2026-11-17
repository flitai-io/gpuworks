package storagefiles

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

const stagingRel = ".gpuworks/incoming/cp1/upload-u1"

// uploadConn is the agent's end of an upload stream: the body to read and
// what the agent writes back. A broken stream fails its read after the body.
type uploadConn struct {
	body    io.Reader
	readErr error // returned instead of EOF
	read    int64
	out     bytes.Buffer
}

func (c *uploadConn) Read(p []byte) (int, error) {
	n, err := c.body.Read(p)
	c.read += int64(n)
	if err == io.EOF && c.readErr != nil {
		return n, c.readErr
	}
	return n, err
}

func (c *uploadConn) Write(p []byte) (int, error) { return c.out.Write(p) }

func (c *uploadConn) status(t *testing.T) proto.StorageFilesStatus {
	t.Helper()
	line, rest, ok := strings.Cut(c.out.String(), "\n")
	if !ok || rest != "" {
		t.Fatalf("the agent must answer with exactly one line, wrote %q", c.out.String())
	}
	var st proto.StorageFilesStatus
	if err := json.Unmarshal([]byte(line), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func upload(t *testing.T, e *testEnv, hdr proto.StreamHeader, body []byte) (*uploadConn, proto.StorageFilesStatus, error) {
	t.Helper()
	return uploadCtx(t, bg, e, hdr, &uploadConn{body: bytes.NewReader(body)})
}

func uploadCtx(t *testing.T, ctx context.Context, e *testEnv, hdr proto.StreamHeader, c *uploadConn) (*uploadConn, proto.StorageFilesStatus, error) {
	t.Helper()
	if hdr.StorageID == 0 {
		hdr.StorageID = 1
	}
	if hdr.Staging == "" {
		hdr.Staging = stagingRel
	}
	hdr.Kind = proto.StreamStorageUpload
	err := e.svc.Upload(ctx, hdr, c)
	st := c.status(t)
	if (err == nil) != (st.State == proto.StorageFilesDone) {
		t.Fatalf("returned %v but answered %+v", err, st)
	}
	if err != nil && (st.Code != codeOf(t, err) || st.Error != err.Error()) {
		t.Fatalf("answer %+v does not carry the error's code %v", st, err)
	}
	// Whatever happened, no staging is left behind.
	if exists(e.path(hdr.Staging)) {
		t.Fatalf("staging %s left behind", hdr.Staging)
	}
	return c, st, err
}

func TestUploadStoresAFileAndAnswersAfterTheRename(t *testing.T) {
	e := newEnv(t)
	_, st, err := upload(t, e, proto.StreamHeader{Rel: "files/sub/a.txt", Size: 5}, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if st != (proto.StorageFilesStatus{State: proto.StorageFilesDone, Bytes: 5, Files: 1}) {
		t.Fatalf("status = %+v", st)
	}
	if readFile(t, e.path("files/sub/a.txt")) != "hello" {
		t.Fatal("content")
	}
	if info, _ := os.Stat(e.path("files/sub/a.txt")); info.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v", info.Mode())
	}
	// Unknown size, the apps region, an empty file.
	if _, st, err := upload(t, e, proto.StreamHeader{Rel: ".gpuworks/apps/eval/x.bin", Size: -1}, []byte("abc")); err != nil || st.Bytes != 3 {
		t.Fatalf("unknown size: %+v %v", st, err)
	}
	if _, _, err := upload(t, e, proto.StreamHeader{Rel: "files/empty", Size: 0}, nil); err != nil || readFile(t, e.path("files/empty")) != "" {
		t.Fatalf("empty file: %v", err)
	}
	if strings.Join(e.ops, ",") != "files.upload,files.upload.commit,files.upload,files.upload.commit,files.upload,files.upload.commit" {
		t.Fatalf("guarded ops = %v", e.ops)
	}
}

func TestUploadSizeMismatchFailsAndLeavesNothing(t *testing.T) {
	e := newEnv(t)
	_, st, err := upload(t, e, proto.StreamHeader{Rel: "files/a.txt", Size: 10}, []byte("short"))
	wantCode(t, err, proto.StorageFilesErrFailed)
	if st.Bytes != 5 || exists(e.path("files/a.txt")) {
		t.Fatalf("short body: %+v", st)
	}
	c, st, err := upload(t, e, proto.StreamHeader{Rel: "files/a.txt", Size: 3}, bytes.Repeat([]byte("x"), 100000))
	wantCode(t, err, proto.StorageFilesErrFailed)
	if exists(e.path("files/a.txt")) || c.read != 100000 {
		t.Fatalf("long body: target written or body not drained (read %d): %+v", c.read, st)
	}
	_, _, err = upload(t, e, proto.StreamHeader{Rel: "files/a.txt", Size: -7}, []byte("x"))
	wantCode(t, err, proto.StorageFilesErrFailed)
}

func TestUploadExistsAndOverwrite(t *testing.T) {
	e := newEnv(t)
	writeFile(t, e.path("files/a.txt"), "old")
	body := bytes.Repeat([]byte("n"), 1<<20)
	c, st, err := upload(t, e, proto.StreamHeader{Rel: "files/a.txt", Size: int64(len(body))}, body)
	wantCode(t, err, proto.StorageFilesErrExists)
	if readFile(t, e.path("files/a.txt")) != "old" {
		t.Fatal("the existing file changed")
	}
	// The refusal is answered at once, and the body is still read to its end
	// so a server that reads the answer only after sending sees it.
	if c.read != int64(len(body)) || st.Bytes != 0 {
		t.Fatalf("read %d bytes, status %+v", c.read, st)
	}
	if _, _, err := upload(t, e, proto.StreamHeader{Rel: "files/a.txt", Size: 3, Overwrite: true}, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if readFile(t, e.path("files/a.txt")) != "new" {
		t.Fatal("overwrite did not replace the file")
	}
	eventually(t, "the old file left the trash", e.trashEmpty)
	// A directory is replaced by a file too.
	writeFile(t, e.path("files/d/inner"), "x")
	if _, _, err := upload(t, e, proto.StreamHeader{Rel: "files/d", Size: 1, Overwrite: true}, []byte("f")); err != nil {
		t.Fatal(err)
	}
	if readFile(t, e.path("files/d")) != "f" {
		t.Fatal("directory not replaced")
	}
}

func TestUploadRefusesTargetsOutsideTheRegions(t *testing.T) {
	e := newEnv(t)
	writeFile(t, e.path("models/m/config.json"), "{}")
	symlink(t, e.outside, e.path("files/out"))
	symlink(t, e.path("models/m"), e.path("files/to-model"))
	cases := map[string]string{
		"files":                    proto.StorageFilesErrRegionReadonly,
		".gpuworks/apps":           proto.StorageFilesErrRegionReadonly,
		"models/m/config.json":     proto.StorageFilesErrRegionReadonly,
		".gpuworks/trash/x":        proto.StorageFilesErrRegionReadonly,
		"":                         proto.StorageFilesErrRegionReadonly,
		"../outside/x":             proto.StorageFilesErrPathInvalid,
		"files/out/x":              proto.StorageFilesErrPathInvalid,
		"files/to-model/weights":   proto.StorageFilesErrRegionReadonly,
		"files/to-model/../../etc": proto.StorageFilesErrPathInvalid,
	}
	for rel, code := range cases {
		_, _, err := upload(t, e, proto.StreamHeader{Rel: rel, Size: 1}, []byte("x"))
		if got := codeOf(t, err); got != code {
			t.Errorf("upload to %q: code %s, want %s (%v)", rel, got, code, err)
		}
	}
	if exists(filepath.Join(e.outside, "x")) || exists(e.path("models/m/weights")) || readFile(t, e.path("models/m/config.json")) != "{}" {
		t.Fatal("an upload wrote outside the writable regions")
	}
}

func TestUploadChecksTheStagingPath(t *testing.T) {
	e := newEnv(t)
	for _, staging := range []string{"files/stage", ".gpuworks/incoming/cp1/other-u1", ".gpuworks/incoming/cp1/upload-", ".gpuworks/incoming/upload-u1",
		".gpuworks/incoming/cp1/upload-u1/deeper", "../.gpuworks/incoming/cp1/upload-u1", ".gpuworks/trash/cp1/upload-u1"} {
		_, _, err := upload(t, e, proto.StreamHeader{Rel: "files/a", Size: 1, Staging: staging}, []byte("x"))
		wantCode(t, err, proto.StorageFilesErrPathInvalid)
	}
	// A staging directory reached through a link out of the storage.
	symlink(t, e.outside, e.path(".gpuworks/incoming/cp1"))
	_, _, err := upload(t, e, proto.StreamHeader{Rel: "files/a", Size: 1}, []byte("x"))
	wantCode(t, err, proto.StorageFilesErrPathInvalid)
	if ents, _ := os.ReadDir(e.outside); len(ents) != 1 {
		t.Fatalf("staging was created outside the storage: %v", ents)
	}
	// A staging path already in use belongs to another upload: it is kept.
	e2 := newEnv(t)
	writeFile(t, e2.path(stagingRel+"/body"), "other upload")
	err = e2.svc.Upload(bg, proto.StreamHeader{StorageID: 1, Rel: "files/a", Staging: stagingRel, Size: 1}, &uploadConn{body: strings.NewReader("x")})
	wantCode(t, err, proto.StorageFilesErrFailed)
	if readFile(t, e2.path(stagingRel+"/body")) != "other upload" {
		t.Fatal("another upload's staging was touched")
	}
}

func TestUploadBrokenStreamOrCancelLeavesTheTargetAlone(t *testing.T) {
	e := newEnv(t)
	writeFile(t, e.path("files/a.txt"), "old")
	broken := errors.New("stream reset")
	c := &uploadConn{body: strings.NewReader("partial"), readErr: broken}
	_, st, err := uploadCtx(t, bg, e, proto.StreamHeader{Rel: "files/a.txt", Size: 100, Overwrite: true}, c)
	wantCode(t, err, proto.StorageFilesErrFailed)
	if !strings.Contains(err.Error(), "stream reset") || st.Bytes != 7 {
		t.Fatalf("err %v status %+v", err, st)
	}
	if readFile(t, e.path("files/a.txt")) != "old" {
		t.Fatal("a broken upload replaced the target")
	}
	// The same for an archive.
	c = &uploadConn{body: bytes.NewReader(tarArchive(t, tarEntry{name: "d/a", body: "x"})[:600]), readErr: broken}
	_, _, err = uploadCtx(t, bg, e, proto.StreamHeader{Rel: "files/x", Size: -1, Extract: proto.StorageArchiveTar}, c)
	wantCode(t, err, proto.StorageFilesErrFailed)
	if exists(e.path("files/x")) {
		t.Fatal("a broken extraction left a target")
	}
	ctx, cancel := context.WithCancel(bg)
	cancel()
	_, _, err = uploadCtx(t, ctx, e, proto.StreamHeader{Rel: "files/b.txt", Size: 3}, &uploadConn{body: strings.NewReader("abc")})
	wantCode(t, err, proto.StorageFilesErrFailed)
	if !strings.Contains(err.Error(), "cancel") || exists(e.path("files/b.txt")) {
		t.Fatalf("cancelled upload: %v", err)
	}
}

func TestUploadOnAStorageThatCannotTakeIt(t *testing.T) {
	e := newEnv(t)
	e.r.Writable = false
	c, _, err := upload(t, e, proto.StreamHeader{Rel: "files/a", Size: 4}, []byte("abcd"))
	wantCode(t, err, proto.StorageFilesErrNotWritable)
	if c.read != 4 {
		t.Fatal("the refused body was not drained")
	}
	e.r.Writable = true
	_, _, err = upload(t, e, proto.StreamHeader{Rel: "files/a", Size: 1, Extract: "rar"}, []byte("x"))
	wantCode(t, err, proto.StorageFilesErrFailed)
	_, _, err = upload(t, e, proto.StreamHeader{Rel: "files/a", Size: 1, StorageID: 9}, []byte("x"))
	wantCode(t, err, proto.StorageFilesErrNotWritable)
}

func TestUploadReportsAStorageWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	e := newEnv(t)
	if err := os.MkdirAll(e.path(".gpuworks/incoming/cp1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(e.path(".gpuworks/incoming/cp1"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(e.path(".gpuworks/incoming/cp1"), 0o755) })
	_, _, err := upload(t, e, proto.StreamHeader{Rel: "files/a", Size: 1}, []byte("x"))
	wantCode(t, err, proto.StorageFilesErrFailed)
	// The rename into place fails: the staged file is dropped, the target untouched.
	e2 := newEnv(t)
	if err := os.MkdirAll(e2.path("files/locked"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(e2.path("files/locked"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(e2.path("files/locked"), 0o755) })
	_, _, err = upload(t, e2, proto.StreamHeader{Rel: "files/locked/a", Size: 1}, []byte("x"))
	wantCode(t, err, proto.StorageFilesErrFailed)
}

// ---- archives ----

type tarEntry struct {
	name     string
	body     string
	typeflag byte
	link     string
	mode     int64
}

func tarArchive(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, en := range entries {
		h := &tar.Header{Name: en.name, Typeflag: en.typeflag, Linkname: en.link, Mode: en.mode}
		if en.typeflag == tar.TypeXGlobalHeader {
			h = &tar.Header{Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "global"}}
			if err := tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(en.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := io.WriteString(tw, en.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipped(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type zipEntry struct {
	name string
	body string
	mode os.FileMode
}

func zipArchive(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, en := range entries {
		fh := &zip.FileHeader{Name: en.name, Method: zip.Deflate}
		if en.mode != 0 {
			fh.SetMode(en.mode)
		}
		w, err := zw.CreateHeader(fh)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, en.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func uploadArchive(t *testing.T, e *testEnv, rel, format string, body []byte) (proto.StorageFilesStatus, error) {
	t.Helper()
	_, st, err := upload(t, e, proto.StreamHeader{Rel: rel, Size: int64(len(body)), Extract: format}, body)
	return st, err
}

func TestUploadUnpacksTarTarGzAndZip(t *testing.T) {
	plain := tarArchive(t,
		tarEntry{name: "./", typeflag: tar.TypeDir, mode: 0o755},
		tarEntry{name: "data/", typeflag: tar.TypeDir, mode: 0o755},
		tarEntry{name: "data/a.jsonl", body: "{}\n"},
		tarEntry{name: "run.sh", body: "#!/bin/sh\n", mode: 0o755},
		tarEntry{name: "deep/er/b.txt", body: "b"},
		tarEntry{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader},
	)
	archives := map[string][]byte{
		proto.StorageArchiveTar:   plain,
		proto.StorageArchiveTarGz: gzipped(t, plain),
		proto.StorageArchiveZip: zipArchive(t, zipEntry{name: "data/", mode: os.ModeDir | 0o755}, zipEntry{name: "data/a.jsonl", body: "{}\n"},
			zipEntry{name: "run.sh", body: "#!/bin/sh\n", mode: 0o755}, zipEntry{name: "deep/er/b.txt", body: "b"}),
	}
	for format, body := range archives {
		e := newEnv(t)
		st, err := uploadArchive(t, e, "files/ds", format, body)
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if st.Files != 3 || st.Bytes != int64(len(body)) {
			t.Fatalf("%s: status %+v", format, st)
		}
		if readFile(t, e.path("files/ds/data/a.jsonl")) != "{}\n" || readFile(t, e.path("files/ds/deep/er/b.txt")) != "b" {
			t.Fatalf("%s: content", format)
		}
		if info, _ := os.Stat(e.path("files/ds/run.sh")); info.Mode().Perm() != 0o755 {
			t.Fatalf("%s: executable mode lost: %v", format, info.Mode())
		}
		if info, _ := os.Stat(e.path("files/ds")); info.Mode().Perm() != 0o755 {
			t.Fatalf("%s: target directory mode %v", format, info.Mode())
		}
		// The zip body is not left beside the result.
		if exists(e.path("files/ds/body.zip")) {
			t.Fatalf("%s: archive copy left in the target", format)
		}
	}
}

func TestUploadRefusesHostileArchives(t *testing.T) {
	hostileTar := map[string][]byte{
		"absolute":  tarArchive(t, tarEntry{name: "/etc/evil", body: "x"}),
		"dotdot":    tarArchive(t, tarEntry{name: "a/../../evil", body: "x"}),
		"dotdot2":   tarArchive(t, tarEntry{name: "../evil", body: "x"}),
		"symlink":   tarArchive(t, tarEntry{name: "link", typeflag: tar.TypeSymlink, link: "/etc"}, tarEntry{name: "link/passwd", body: "x"}),
		"hardlink":  tarArchive(t, tarEntry{name: "hl", typeflag: tar.TypeLink, link: "/etc/passwd"}),
		"chardev":   tarArchive(t, tarEntry{name: "dev", typeflag: tar.TypeChar}),
		"blockdev":  tarArchive(t, tarEntry{name: "blk", typeflag: tar.TypeBlock}),
		"fifo":      tarArchive(t, tarEntry{name: "pipe", typeflag: tar.TypeFifo}),
		"backslash": tarArchive(t, tarEntry{name: `..\evil`, body: "x"}),
		"duplicate": tarArchive(t, tarEntry{name: "a", body: "1"}, tarEntry{name: "a", body: "2"}),
		"rootfile":  tarArchive(t, tarEntry{name: ".", body: "1"}),
		"garbage":   bytes.Repeat([]byte("not a tar archive "), 100),
		"truncated": tarArchive(t, tarEntry{name: "a", body: strings.Repeat("x", 4096)})[:1500],
	}
	for name, body := range hostileTar {
		e := newEnv(t)
		_, err := uploadArchive(t, e, "files/x", proto.StorageArchiveTar, body)
		if got := codeOf(t, err); got != proto.StorageFilesErrExtractFailed {
			t.Errorf("tar %s: code %s (%v)", name, got, err)
		}
		if exists(e.path("files/x")) || exists(e.path("evil")) || exists(filepath.Join(filepath.Dir(e.root), "evil")) {
			t.Errorf("tar %s: something was written", name)
		}
	}
	e := newEnv(t)
	_, err := uploadArchive(t, e, "files/x", proto.StorageArchiveTarGz, []byte("not gzip"))
	wantCode(t, err, proto.StorageFilesErrExtractFailed)
	corrupt := gzipped(t, tarArchive(t, tarEntry{name: "a", body: "x"}))
	corrupt[len(corrupt)-6] ^= 0xff // the gzip checksum
	_, err = uploadArchive(t, e, "files/x", proto.StorageArchiveTarGz, corrupt)
	wantCode(t, err, proto.StorageFilesErrExtractFailed)

	hostileZip := map[string][]byte{
		"zipslip":  zipArchive(t, zipEntry{name: "../../evil", body: "x"}),
		"absolute": zipArchive(t, zipEntry{name: "/evil", body: "x"}),
		"symlink":  zipArchive(t, zipEntry{name: "link", body: "/etc", mode: os.ModeSymlink | 0o777}),
		"device":   zipArchive(t, zipEntry{name: "dev", mode: os.ModeDevice | 0o644}),
		"pipe":     zipArchive(t, zipEntry{name: "p", mode: os.ModeNamedPipe | 0o644}),
		"garbage":  []byte("PK not really"),
	}
	for name, body := range hostileZip {
		e := newEnv(t)
		_, err := uploadArchive(t, e, "files/x", proto.StorageArchiveZip, body)
		if got := codeOf(t, err); got != proto.StorageFilesErrExtractFailed {
			t.Errorf("zip %s: code %s (%v)", name, got, err)
		}
		if exists(e.path("files/x")) || exists(filepath.Join(filepath.Dir(e.root), "evil")) {
			t.Errorf("zip %s: something was written", name)
		}
	}
	// A zip whose stored checksum does not match its data.
	bad := zipArchive(t, zipEntry{name: "a.txt", body: strings.Repeat("abc", 100)})
	idx := bytes.LastIndex(bad, []byte("a.txt")) - 30 // the central directory's crc32
	bad[idx] ^= 0xff
	e = newEnv(t)
	_, err = uploadArchive(t, e, "files/x", proto.StorageArchiveZip, bad)
	wantCode(t, err, proto.StorageFilesErrExtractFailed)
}

func TestUploadBoundsWhatAnArchiveUnpacksTo(t *testing.T) {
	// A small gzip that expands to 4 MiB of zeros, against a 1 MiB bound.
	bomb := gzipped(t, tarArchive(t, tarEntry{name: "zeros", body: string(make([]byte, 4<<20))}))
	e := newEnv(t)
	e.avail = DefaultExtractReserve + 1<<20
	st, err := uploadArchive(t, e, "files/x", proto.StorageArchiveTarGz, bomb)
	wantCode(t, err, proto.StorageFilesErrExtractFailed)
	if exists(e.path("files/x")) || st.Bytes >= int64(len(bomb))+1 {
		t.Fatalf("bomb: %+v", st)
	}
	// The same through zip (declared sizes are checked first; the written
	// bytes are what counts).
	zbomb := zipArchive(t, zipEntry{name: "zeros", body: string(make([]byte, 4<<20))})
	_, err = uploadArchive(t, e, "files/x", proto.StorageArchiveZip, zbomb)
	wantCode(t, err, proto.StorageFilesErrExtractFailed)
	// Many small files add up.
	var many []tarEntry
	for i := 0; i < 5; i++ {
		many = append(many, tarEntry{name: "f" + string(rune('a'+i)), body: strings.Repeat("x", 300<<10)})
	}
	_, err = uploadArchive(t, e, "files/x", proto.StorageArchiveTar, tarArchive(t, many...))
	wantCode(t, err, proto.StorageFilesErrExtractFailed)
	// Less free space than the reserve: nothing non-empty fits.
	e.avail = 10
	_, err = uploadArchive(t, e, "files/x", proto.StorageArchiveTar, tarArchive(t, tarEntry{name: "a", body: "x"}))
	wantCode(t, err, proto.StorageFilesErrExtractFailed)
	// An unknown free space leaves only the entry bound.
	e.availFn = func(string) (int64, error) { return 0, errors.New("statfs failed") }
	if _, err := uploadArchive(t, e, "files/x", proto.StorageArchiveTar, tarArchive(t, tarEntry{name: "a", body: "x"})); err != nil {
		t.Fatal(err)
	}
	// A plain file upload is never bounded by the extraction limit.
	e.availFn = nil
	if _, _, err := upload(t, e, proto.StreamHeader{Rel: "files/big", Size: 1 << 20}, make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
}

func TestUploadBoundsTheNumberOfEntries(t *testing.T) {
	e := newEnv(t)
	e.svc.o.MaxExtractEntries = 3
	var entries []tarEntry
	var zentries []zipEntry
	for i := 0; i < 4; i++ {
		entries = append(entries, tarEntry{name: "f" + string(rune('a'+i))})
		zentries = append(zentries, zipEntry{name: "f" + string(rune('a'+i))})
	}
	_, err := uploadArchive(t, e, "files/x", proto.StorageArchiveTar, tarArchive(t, entries...))
	wantCode(t, err, proto.StorageFilesErrExtractFailed)
	_, err = uploadArchive(t, e, "files/x", proto.StorageArchiveZip, zipArchive(t, zentries...))
	wantCode(t, err, proto.StorageFilesErrExtractFailed)
	if _, err := uploadArchive(t, e, "files/x", proto.StorageArchiveTar, tarArchive(t, entries[:3]...)); err != nil {
		t.Fatalf("at the bound: %v", err)
	}
	if d := New(Options{}); d.o.MaxExtractEntries != DefaultMaxExtractEntries || d.o.ExtractReserve != DefaultExtractReserve {
		t.Fatalf("defaults = %+v", d.o)
	}
}
