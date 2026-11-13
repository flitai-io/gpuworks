package packages

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
)

func TestNewRejectsBroadRootAndAssetPathEscapes(t *testing.T) {
	if _, err := New("."); err == nil {
		t.Fatal("broad package root was accepted")
	}
	store, err := New(t.TempDir() + "/packages")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadAsset("bad", "org/model", "assets/a.py"); err == nil {
		t.Fatal("invalid digest was accepted")
	}
	if _, err := store.ReadAsset("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "org/model", "../a.py"); err == nil {
		t.Fatal("escaping asset was accepted")
	}
	if _, _, err := store.Put(fstest.MapFS{"unexpected": {Data: []byte("x")}}); err == nil {
		t.Fatal("invalid package was persisted")
	}
}

func TestPruneRemovesOnlyUnreferencedDigestDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "packages")
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	kept := strings.Repeat("a", 64)
	removedA := strings.Repeat("b", 64)
	removedB := strings.Repeat("c", 64)
	for _, name := range []string{kept, removedA, removedB, ".package-staging", "not-a-digest"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	digestFile := strings.Repeat("d", 64)
	if err := os.WriteFile(filepath.Join(root, digestFile), []byte("not a package directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := store.Prune([]string{" " + strings.ToUpper(kept) + " ", "invalid"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{removedA, removedB}; !reflect.DeepEqual(got, want) {
		t.Fatalf("removed=%v want=%v", got, want)
	}
	for _, name := range []string{kept, ".package-staging", "not-a-digest", digestFile} {
		if _, err := os.Lstat(filepath.Join(root, name)); err != nil {
			t.Fatalf("non-eligible entry %q was removed: %v", name, err)
		}
	}
}

// PutLoaded stores the embedded package under the digest its caller computed,
// exactly where Put of the same files would, and refuses a malformed digest.
func TestPutLoadedStoresTheEmbeddedPackageAsPutWould(t *testing.T) {
	root := filepath.Join(t.TempDir(), "packages")
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	fsys, digest, err := seed.Package()
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "../escape", strings.ToUpper(digest), digest + "0"} {
		if _, err := store.PutLoaded(fsys, bad); err == nil {
			t.Fatalf("digest %q was accepted", bad)
		}
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("a refused digest left %v (%v)", entries, err)
	}
	created, err := store.PutLoaded(fsys, digest)
	if err != nil || !created {
		t.Fatalf("PutLoaded = %v, %v; want created", created, err)
	}
	again, err := store.PutLoaded(fsys, digest)
	if err != nil || again {
		t.Fatalf("second PutLoaded = %v, %v; want an existing package", again, err)
	}
	putDigest, putCreated, err := store.Put(fsys)
	if err != nil || putDigest != digest || putCreated {
		t.Fatalf("Put = %q, %v, %v; want the stored %q", putDigest, putCreated, err, digest)
	}
	if _, err := store.Load(digest); err != nil {
		t.Fatalf("stored package does not load: %v", err)
	}
}
