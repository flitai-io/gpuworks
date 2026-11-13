package native

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestRemoveExistingReportsFilesystemInspectionFailures(t *testing.T) {
	root := t.TempDir()
	// A name beyond the filesystem component limit fails even as root, unlike
	// permission-bit fixtures whose errors disappear in privileged CI jobs.
	if err := removeExisting(filepath.Join(root, strings.Repeat("x", 300))); !errors.Is(err, syscall.ENAMETOOLONG) {
		t.Fatalf("remove existing with an uninspectable name = %v, want ENAMETOOLONG", err)
	}
	if err := removeExisting(filepath.Join(root, "absent")); err != nil {
		t.Fatalf("missing entry is already removed: %v", err)
	}
	link := filepath.Join(root, "dangling")
	if err := os.Symlink("absent", link); err != nil {
		t.Fatal(err)
	}
	if err := removeExisting(link); err != nil {
		t.Fatalf("replace dangling symlink: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("dangling symlink survived replacement: %v", err)
	}
}

func TestWhiteoutsReportFilesystemInspectionFailures(t *testing.T) {
	for _, marker := range []string{"loop/.wh.child", "loop/.wh..wh..opq"} {
		t.Run(marker, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Symlink("loop", filepath.Join(root, "loop")); err != nil {
				t.Fatal(err)
			}
			u := Unpacker{Root: root, Workers: 1}
			layer := entryModeLayer(t, []entry{{name: marker}})
			if err := u.Unpack(context.Background(), []LayerSource{layer}); !errors.Is(err, syscall.ELOOP) {
				t.Fatalf("whiteout with uninspectable target = %v, want ELOOP", err)
			}
		})
	}
}

func TestWhiteoutsOfAbsentTargetsAreIdempotent(t *testing.T) {
	u := Unpacker{Root: t.TempDir(), Workers: 1}
	layer := entryModeLayer(t, []entry{{name: ".wh.absent"}, {name: "missing/.wh.child"}, {name: "missing/.wh..wh..opq"}})
	if err := u.Unpack(context.Background(), []LayerSource{layer}); err != nil {
		t.Fatalf("whiteouts of absent targets: %v", err)
	}
}

func TestWhiteoutsKeepADirectoryReplacedByAHigherFile(t *testing.T) {
	root := unpackLayers(t, 1,
		[]entry{{name: "etc", dir: true}, {name: "etc/.wh.old"}, {name: "etc/.wh..wh..opq"}},
		[]entry{{name: "etc", body: "higher layer file"}},
	)
	if got := readFile(t, root, "etc"); got != "higher layer file" {
		t.Fatalf("higher layer replaced the directory with %q", got)
	}
}
