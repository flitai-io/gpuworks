package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

var pdfHeader = regexp.MustCompile(`^%PDF-(1\.[0-7]|2\.0)[\r\n]`)

// readReleaseDocs validates the optional documentation sidecar. Its own
// checksums leave archive-only SHA256SUMS compatible with existing releases.
// Return the original bytes so republishing preserves immutable PDF objects.
func readReleaseDocs(dir, releaseID string) (map[string][]byte, error) {
	docs := filepath.Join(dir, "docs")
	info, err := os.Lstat(docs)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s must be a directory, not a symlink or file", docs)
	}
	expected := map[string]bool{
		"SHA256SUMS":                        true,
		"gpuworks-" + releaseID + "-zh.pdf": true,
		"gpuworks-" + releaseID + "-en.pdf": true,
	}
	entries, err := os.ReadDir(docs)
	if err != nil {
		return nil, err
	}
	if len(entries) != len(expected) {
		return nil, fmt.Errorf("%s must contain exactly two release PDFs and SHA256SUMS", docs)
	}
	files := make(map[string][]byte, len(expected))
	for _, entry := range entries {
		if !expected[entry.Name()] {
			return nil, fmt.Errorf("%s contains unexpected file %q", docs, entry.Name())
		}
		path := filepath.Join(docs, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s must be a regular file", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		files[entry.Name()] = data
	}
	sums, err := readSums(filepath.Join(docs, "SHA256SUMS"))
	if err != nil {
		return nil, fmt.Errorf("documentation checksums: %w", err)
	}
	if len(sums) != 2 {
		return nil, fmt.Errorf("documentation SHA256SUMS must list the two release PDFs")
	}
	for name, data := range files {
		if name == "SHA256SUMS" {
			continue
		}
		want, exists := sums[name]
		if !exists {
			return nil, fmt.Errorf("documentation checksum missing for %s", name)
		}
		if got := sha256Hex(data); got != want {
			return nil, fmt.Errorf("%s sha256 %s, documentation SHA256SUMS lists %s", name, got, want)
		}
		if !pdfHeader.Match(data) || !bytes.HasSuffix(bytes.TrimSpace(data), []byte("%%EOF")) {
			return nil, fmt.Errorf("%s has no complete PDF header and end marker", name)
		}
	}
	return files, nil
}
