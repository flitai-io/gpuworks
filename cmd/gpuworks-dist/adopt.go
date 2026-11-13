package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// A release id names one set of archives for good: files under releases/ are
// served as immutable, so a CDN keeps whatever it saw first. A pipeline that
// rebuilds a commit already published gets other bytes (the build time is in
// them) and must republish the archives the sources already hold instead of
// its own. adopt checks a release directory copied back from a source and
// reports the build time its archives carry.

func releaseIDCommand(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("release-id", flag.ContinueOnError)
	var dist string
	fs.StringVar(&dist, "dist", "dist", "directory holding gpuworks-<id>-<os>-<arch>.tar.gz")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, releaseID, err := findArchives(dist, "")
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, releaseID)
	return nil
}

func adoptCommand(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("adopt", flag.ContinueOnError)
	var dir, releaseID string
	fs.StringVar(&dir, "dir", "", "release directory copied from a source (releases/<id>/)")
	fs.StringVar(&releaseID, "release-id", "", "release id the directory must hold")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if dir == "" || releaseID == "" {
		return errors.New("-dir and -release-id are required")
	}
	builtAt, err := adopt(dir, releaseID)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, builtAt)
	return nil
}

// adopt checks that dir holds exactly the archives its SHA256SUMS lists, all
// of releaseID, and returns the build time they were packaged with.
func adopt(dir, releaseID string) (string, error) {
	sums, err := readSums(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return "", err
	}
	archives, _, err := findArchives(dir, releaseID)
	if err != nil {
		return "", err
	}
	if len(archives) != len(sums) {
		return "", fmt.Errorf("SHA256SUMS lists %d archives, %s holds %d", len(sums), dir, len(archives))
	}
	if _, err := readReleaseDocs(dir, releaseID); err != nil {
		return "", err
	}
	builtAt := ""
	for _, archive := range archives {
		want, ok := sums[archive.name]
		if !ok {
			return "", fmt.Errorf("%s is not in SHA256SUMS", archive.name)
		}
		file := filepath.Join(dir, archive.name)
		got, err := fileSHA256(file)
		if err != nil {
			return "", err
		}
		if got != want {
			return "", fmt.Errorf("%s sha256 %s, SHA256SUMS lists %s", archive.name, got, want)
		}
		at, err := archiveBuildTime(file)
		if err != nil {
			return "", fmt.Errorf("%s: %w", archive.name, err)
		}
		if builtAt != "" && at != builtAt {
			return "", fmt.Errorf("%s was built %s, the others %s", archive.name, at, builtAt)
		}
		builtAt = at
	}
	return builtAt, nil
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// readSums parses sha256sum output: "<hex>  <name>" per line.
func readSums(file string) (map[string]string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	sums := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		digest, name, ok := strings.Cut(line, "  ")
		if !ok || !sha256Pattern.MatchString(digest) || name == "" {
			return nil, fmt.Errorf("SHA256SUMS line %q is not \"<sha256>  <name>\"", line)
		}
		if _, exists := sums[name]; exists {
			return nil, fmt.Errorf("SHA256SUMS lists %q twice", name)
		}
		sums[name] = digest
	}
	return sums, nil
}

func fileSHA256(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// archiveBuildTime reads the "Build time:" line scripts/package_release.sh
// writes into each archive's INSTALL.txt; the binary beside it was built with
// the same time.
func archiveBuildTime(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return "", errors.New("no INSTALL.txt")
		}
		if err != nil {
			return "", err
		}
		if path.Base(hdr.Name) != "INSTALL.txt" {
			continue
		}
		scanner := bufio.NewScanner(tr)
		for scanner.Scan() {
			if value, ok := strings.CutPrefix(scanner.Text(), "Build time:"); ok {
				value = strings.TrimSpace(value)
				if _, err := time.Parse(time.RFC3339, value); err != nil {
					return "", fmt.Errorf("INSTALL.txt build time %q: %w", value, err)
				}
				return value, nil
			}
		}
		if err := scanner.Err(); err != nil {
			return "", err
		}
		return "", errors.New("INSTALL.txt has no build time")
	}
}
