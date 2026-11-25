package inferencecatalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"reflect"
	"testing"
)

type packageDigestReadFS struct {
	fs.FS
	reads  map[string]int
	failAt map[string]int
}

func (f *packageDigestReadFS) ReadFile(name string) ([]byte, error) {
	f.reads[name]++
	if f.failAt[name] == f.reads[name] {
		return nil, fs.ErrPermission
	}
	return fs.ReadFile(f.FS, name)
}

func TestPackageDigestLoadsSemanticCatalogOnce(t *testing.T) {
	fixture := packageFixture(t)
	// The digest is of the archive as published, so the projection beside it
	// is the tolerant one the packer validates.
	expected, _, err := LoadPackageTolerant(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := PackPackage(&archive, fixture); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive.Bytes())
	if separate, err := PackageDigest(fixture); err != nil || separate != hex.EncodeToString(sum[:]) {
		t.Fatalf("package digest %q is not the packed archive's: %v", separate, err)
	}
	input := &packageDigestReadFS{FS: fixture, reads: map[string]int{}, failAt: map[string]int{}}
	got, digest, err := LoadPackageDigest(input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, expected) || digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("projection or exact archive changed: %+v digest=%s", got, digest)
	}
	if count := input.reads["models/org/model/model.yaml"]; count != 2 {
		t.Fatalf("model YAML read %d times; require one semantic validation plus one exact archive read (2)", count)
	}
}

func TestLoadPackageDigestFailureReturnsNoProjection(t *testing.T) {
	for _, tc := range []struct {
		name, leaf        string
		read              int
		badYAML, badAsset bool
	}{
		{name: "semantic-invalid", badYAML: true}, {name: "asset-invalid", badAsset: true},
		{name: "semantic-read-denied", leaf: "models/org/model/model.yaml", read: 1},
		{name: "archive-read-denied", leaf: "models/org/model/model.yaml", read: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := packageFixture(t)
			if tc.badYAML {
				// Not a manifest at all: a tolerant load skips what it cannot
				// read inside a package, never a package it cannot identify.
				fixture["manifest.yaml"].Data = []byte("schema_version: [\n")
			}
			if tc.badAsset {
				fixture["models/org/model/assets/serve.py"].Data = []byte("tampered asset\n")
			}
			input := &packageDigestReadFS{FS: fixture, reads: map[string]int{}, failAt: map[string]int{tc.leaf: tc.read}}
			got, digest, err := LoadPackageDigest(input)
			if err == nil || digest != "" || !reflect.DeepEqual(got, Package{}) {
				t.Fatalf("failure exposed projection/digest: %+v/%s/%v", got, digest, err)
			}
			if tc.read != 0 && !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("read failure identity lost: %v", err)
			}
		})
	}
}

func TestValidatedPackFailureReturnsNoProjection(t *testing.T) {
	got, err := packPackage(packageDigestRejectWriter{}, packageFixture(t))
	if !errors.Is(err, fs.ErrPermission) || !reflect.DeepEqual(got, Package{}) {
		t.Fatalf("archive write failure exposed projection: %+v/%v", got, err)
	}
}

type packageDigestRejectWriter struct{}

func (packageDigestRejectWriter) Write([]byte) (int, error) { return 0, fs.ErrPermission }
