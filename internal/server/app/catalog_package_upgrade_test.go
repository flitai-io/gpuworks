package app

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
)

func TestNewMaterializesEmbeddedCatalogForFutureUpgradeRecovery(t *testing.T) {
	dir := t.TempDir()
	server, err := New(Config{
		Addr: "127.0.0.1:0", DataDir: dir,
		DBPath: filepath.Join(dir, "gpuworks.db"), AdminUser: "admin", AdminPassword: "gpuworks-test-password-2026",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// New starts the proxy's route-table goroutine outside of Run; this test
	// never calls Run, so close it explicitly or it leaks for the rest of the
	// test binary (see the same note in adminbootstrap_test.go).
	t.Cleanup(func() { server.proxy.Close(); _ = server.Store().Close() })

	archive, err := seed.Archive()
	if err != nil {
		t.Fatal(err)
	}
	packageStore, err := packages.New(filepath.Join(dir, "catalog-packages"))
	if err != nil {
		t.Fatal(err)
	}
	digest, created, err := packageStore.PutArchive(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("New did not store the embedded package: storing its archive afterwards created it")
	}
	packageDir := filepath.Join(dir, "catalog-packages", digest)
	if info, err := os.Stat(packageDir); err != nil || !info.IsDir() {
		t.Fatalf("embedded package was not materialized at %s: info=%v err=%v", packageDir, info, err)
	}
	pkg, err := packageStore.Load(digest)
	if err != nil {
		t.Fatal(err)
	}
	// Derive the expectation from the embedded catalog rather than pinning a
	// literal count: this test is about the package being materialised and
	// loadable, so a magic number here only has to be re-bumped every time the
	// catalog grows. (The pinned summary lives in the seed package's own test.)
	catalog, err := seed.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	specs := 0
	for _, model := range catalog.Models {
		specs += len(model.Specs)
	}
	if pkg.Manifest.Models != len(catalog.Models) || pkg.Manifest.Specs != specs {
		t.Fatalf("materialized package summary=%d/%d, embedded catalog=%d/%d",
			pkg.Manifest.Models, pkg.Manifest.Specs, len(catalog.Models), specs)
	}
}
