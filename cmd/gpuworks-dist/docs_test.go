package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeDocs(t *testing.T, dir, releaseID string) map[string][]byte {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	var sums strings.Builder
	for _, locale := range []string{"zh", "en"} {
		name := fmt.Sprintf("gpuworks-%s-%s.pdf", releaseID, locale)
		data := []byte("%PDF-1.4\n% fixture " + locale + "\n%%EOF\n")
		files[name] = data
		fmt.Fprintf(&sums, "%s  %s\n", sha256Hex(data), name)
	}
	files["SHA256SUMS"] = []byte(sums.String())
	for name, data := range files {
		writeFile(t, filepath.Join(dir, "docs", name), string(data))
	}
	return files
}

func TestAssembleCopiesReleaseDocsWithoutChangingArchiveChecksums(t *testing.T) {
	in := realInput(t, distDir(t, abcArchives...))
	want := writeDocs(t, in.DistDir, "abc123")
	index, err := assemble(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(index.Server.Files) != len(abcArchives) {
		t.Fatalf("docs changed platform assets: %+v", index.Server.Files)
	}
	for name, data := range want {
		got := readTree(t, in.OutDir, "releases/abc123/docs/"+name)
		if !bytes.Equal(got, data) {
			t.Fatalf("%s changed during publication", name)
		}
	}
	sums := string(readTree(t, in.OutDir, "releases/abc123/SHA256SUMS"))
	if strings.Contains(sums, ".pdf") || strings.Count(sums, "\n") != len(abcArchives) {
		t.Fatalf("archive SHA256SUMS includes docs: %s", sums)
	}
}

func TestAdoptPreservesPublishedReleaseDocs(t *testing.T) {
	first := realInput(t, buildDist(t, "2026-11-10T00:00:00Z", abcArchives...))
	writeDocs(t, first.DistDir, "abc123")
	if _, err := assemble(first); err != nil {
		t.Fatal(err)
	}
	published := filepath.Join(first.OutDir, "releases", "abc123")
	builtAt, err := adopt(published, "abc123")
	if err != nil || builtAt != "2026-11-10T00:00:00Z" {
		t.Fatalf("adopt = %q, %v", builtAt, err)
	}
	second := realInput(t, published)
	second.BuiltAt = builtAt
	if _, err := assemble(second); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gpuworks-abc123-zh.pdf", "gpuworks-abc123-en.pdf", "SHA256SUMS"} {
		rel := "releases/abc123/docs/" + name
		if !bytes.Equal(readTree(t, first.OutDir, rel), readTree(t, second.OutDir, rel)) {
			t.Fatalf("%s changed during immutable republish", rel)
		}
	}
}

func TestReleaseDocsRejectInvalidSidecars(t *testing.T) {
	const name = "gpuworks-abc123-zh.pdf"
	base := realInput(t, "")
	cases := []struct {
		name   string
		mutate func(t *testing.T, dir string)
		want   string
	}{
		{"missing PDF", func(t *testing.T, dir string) { removeDocsFile(t, dir, name) }, "exactly"},
		{"missing checksums", func(t *testing.T, dir string) { removeDocsFile(t, dir, "SHA256SUMS") }, "exactly"},
		{"extra file", func(t *testing.T, dir string) { writeFile(t, filepath.Join(dir, "extra.pdf"), "%PDF-1.4\n%%EOF\n") }, "exactly"},
		{"wrong release", func(t *testing.T, dir string) {
			removeDocsFile(t, dir, name)
			writeFile(t, filepath.Join(dir, "gpuworks-other-zh.pdf"), "%PDF-1.4\n%%EOF\n")
		}, "unexpected"},
		{"corrupt PDF", func(t *testing.T, dir string) { writeFile(t, filepath.Join(dir, name), "%PDF-1.4\nchanged\n%%EOF\n") }, "sha256"},
		{"empty PDF", func(t *testing.T, dir string) { replaceDocsPDF(t, dir, name, "") }, "PDF"},
		{"non PDF", func(t *testing.T, dir string) { replaceDocsPDF(t, dir, name, "<html>error page</html>") }, "PDF"},
		{"truncated PDF", func(t *testing.T, dir string) { replaceDocsPDF(t, dir, name, "%PDF-1.4\nunfinished") }, "PDF"},
		{"missing checksum entry", func(t *testing.T, dir string) {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(dir, "SHA256SUMS"), sha256Hex(data)+"  "+name+"\n")
		}, "two"},
		{"duplicate checksum entry", func(t *testing.T, dir string) {
			path := filepath.Join(dir, "SHA256SUMS")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, path, string(data)+strings.Split(string(data), "\n")[0]+"\n")
		}, "twice"},
		{"checksum path traversal", func(t *testing.T, dir string) {
			path := filepath.Join(dir, "SHA256SUMS")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, path, strings.ReplaceAll(string(data), name, "../"+name))
		}, "checksum"},
		{"PDF symlink", func(t *testing.T, dir string) { docsSymlink(t, filepath.Join(dir, name)) }, "regular"},
		{"checksum symlink", func(t *testing.T, dir string) { docsSymlink(t, filepath.Join(dir, "SHA256SUMS")) }, "regular"},
		{"PDF directory", func(t *testing.T, dir string) {
			removeDocsFile(t, dir, name)
			if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "regular"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			in.DistDir = buildDist(t, "2026-11-10T00:00:00Z", abcArchives...)
			in.OutDir = filepath.Join(t.TempDir(), "tree")
			var sums strings.Builder
			for _, archive := range abcArchives {
				data, err := os.ReadFile(filepath.Join(in.DistDir, archive))
				if err != nil {
					t.Fatal(err)
				}
				fmt.Fprintf(&sums, "%s  %s\n", sha256Hex(data), archive)
			}
			writeFile(t, filepath.Join(in.DistDir, "SHA256SUMS"), sums.String())
			writeDocs(t, in.DistDir, "abc123")
			tc.mutate(t, filepath.Join(in.DistDir, "docs"))
			if _, err := assemble(in); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("assemble error = %v, want %q", err, tc.want)
			}
			if _, err := os.Stat(in.OutDir); !os.IsNotExist(err) {
				t.Fatalf("invalid docs created an output tree: %v", err)
			}
			if _, err := adopt(in.DistDir, "abc123"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("adopt error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestReleaseDocsRejectDirectorySymlink(t *testing.T) {
	dir := t.TempDir()
	writeDocs(t, dir, "abc123")
	docsSymlink(t, filepath.Join(dir, "docs"))
	if _, err := readReleaseDocs(dir, "abc123"); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("docs symlink error = %v", err)
	}
}

func removeDocsFile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}

func replaceDocsPDF(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, "SHA256SUMS")
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, name), body)
	writeFile(t, path, strings.ReplaceAll(string(sums), sha256Hex(data), sha256Hex([]byte(body))))
}

func docsSymlink(t *testing.T, path string) {
	t.Helper()
	target := filepath.Join(t.TempDir(), "original")
	if err := os.Rename(path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}
