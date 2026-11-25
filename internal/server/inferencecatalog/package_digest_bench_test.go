package inferencecatalog_test

import (
	"os"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
)

func BenchmarkPackageDigestRealCatalog(b *testing.B) {
	source, _, err := seed.Package()
	if err != nil {
		b.Fatal(err)
	}
	dir := b.TempDir()
	if err := os.CopyFS(dir, source); err != nil {
		b.Fatal(err)
	}
	input := os.DirFS(dir)
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		pkg, digest, err := inferencecatalog.LoadPackageDigest(input)
		if err != nil || digest == "" || len(pkg.Catalog.Models) == 0 {
			b.Fatalf("catalog proof failed: %v", err)
		}
	}
}
