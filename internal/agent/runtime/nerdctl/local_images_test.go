package nerdctl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestRuntimeImageImportAcrossNamespaces(t *testing.T) {
	dir := t.TempDir()
	for _, ns := range []string{"default", "k8s.io"} {
		if err := os.Mkdir(filepath.Join(dir, ns), 0700); err != nil {
			t.Fatal(err)
		}
	}
	target := "sha256:" + strings.Repeat("a", 64)
	config := "sha256:" + strings.Repeat("b", 64)
	image := fmt.Sprintf(`{"Image":{"target":{"digest":%q}},"ImageConfigDesc":{"digest":%q},"ImageConfig":{"architecture":"amd64","config":{"Labels":{"io.gpuworks.engine":"vllm"}}}}`, target, config)
	write := func(path, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, path), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("default/runner:v1", image)
	// Each directory is one namespace. Save/load pass the pinned tag and its
	// contents over stdin, so the test also observes when a copy occurs.
	const cli = `#!/bin/sh
set -eu
[ "$1" = "-n" ]
ns=$2
shift 2
cd "$TEST_IMAGE_STORE/$ns"
mkdir -p gpuworks.internal
if [ "$1" = load ]; then read -r stage; cat > "$stage"; exit; fi
case "$1 ${2:-}" in
  'namespace ls') printf 'default\nk8s.io\n' ;;
  'images --no-trunc') printf 'runner\tv1\t%s\t4 KiB\n<none>\t<none>\t%s\t1 KiB\n' "$TEST_TARGET" "$TEST_TARGET" ;;
  'image inspect') shift 5; [ ! -f "$1" ] || cat "$1" ;;
  'image rm') rm -f -- "$3" ;;
  'tag '*) cp -- "$2" "$3" ;;
  'save '*) echo "$2"; cat "$2"; echo copied >> "$TEST_IMAGE_STORE/transfers" ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "nerdctl"), []byte(cli), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("TEST_IMAGE_STORE", dir)
	t.Setenv("TEST_TARGET", target)
	d := NewInNamespace("k8s.io")
	list, err := d.ListLocalImages(t.Context(), "default")
	if err != nil || list.TargetNamespace != "k8s.io" || list.Namespace != "default" || len(list.Items) != 1 || list.Items[0].Ref != "runner:v1" {
		t.Fatalf("list %+v: %v", list, err)
	}
	p := proto.RuntimeImageSelection{Runtime: list.Runtime, Namespace: list.Namespace, TargetNamespace: list.TargetNamespace, Ref: list.Items[0].Ref, Identity: list.Items[0].Identity}
	preview, err := d.ResolveLocalImage(t.Context(), p, false)
	if err != nil || preview.ID != config || preview.Labels["io.gpuworks.engine"] != "vllm" {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "k8s.io/runner:v1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preview copied an image: %v", err)
	}
	for i := 0; i < 2; i++ {
		installed, err := d.ResolveLocalImage(t.Context(), p, true)
		if err != nil || installed.ID != config {
			t.Fatalf("import %d: %+v %v", i, installed, err)
		}
	}
	copies, err := os.ReadFile(filepath.Join(dir, "transfers"))
	if err != nil || string(copies) != "copied\n" {
		t.Fatalf("transfers: %q %v", copies, err)
	}
	other := strings.Replace(image, config, "sha256:"+strings.Repeat("c", 64), 1)
	write("k8s.io/runner:v1", other)
	if _, err := d.ResolveLocalImage(t.Context(), p, true); !errors.Is(err, runtime.ErrLocalImageConflict) {
		t.Fatalf("expected conflict: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "k8s.io/runner:v1"))
	if err != nil || string(body) != other {
		t.Fatal("conflicting target was modified")
	}
	p.Identity = config
	if _, err := d.ResolveLocalImage(t.Context(), p, true); !errors.Is(err, runtime.ErrLocalImageChanged) {
		t.Fatalf("stale selection: %v", err)
	}
}
