package seed

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

// The server persists Package() at startup; gpuworks-dist publishes Published().
// Both consumers must identify the same bytes, including the manifest.
func TestPublishedSeedMatchesServerPackage(t *testing.T) {
	root, serverDigest, err := Package()
	if err != nil {
		t.Fatal(err)
	}
	published, channelDigest, manifest, err := Published()
	if err != nil {
		t.Fatal(err)
	}
	var stored bytes.Buffer
	if err := inferencecatalog.PackPackage(&stored, root); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(published)
	if !bytes.Equal(stored.Bytes(), published) || serverDigest != channelDigest || channelDigest != hex.EncodeToString(sum[:]) {
		t.Fatal("server and channel have different package identities")
	}
	archive, err := inferencecatalog.ReadPackageArchive(bytes.NewReader(published))
	if err != nil {
		t.Fatal(err)
	}
	if manifest != archive.Package.Manifest || manifest.Models != len(archive.Package.Catalog.Models) {
		t.Fatalf("channel manifest differs from the installable package: %+v", manifest)
	}
	source, err := fs.ReadFile(root, "manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	packed, err := fs.ReadFile(archive.FS, "manifest.yaml")
	if err != nil || !bytes.Equal(source, packed) {
		t.Fatal("the release archive did not preserve the server manifest")
	}
	if file := os.Getenv("SEED_CATALOG_MANIFEST"); file != "" {
		expected, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(expected, packed) {
			t.Fatal("the release archive did not embed the build's input manifest")
		}
	}
	t.Logf("server and channel version %s", inferencecatalog.CatalogVersion(manifest.PublishedAt, channelDigest))
}
