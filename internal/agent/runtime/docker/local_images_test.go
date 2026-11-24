package docker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestDockerRuntimeImageListAndResolve(t *testing.T) {
	dir := t.TempDir()
	const ref = "example:5000/runner:v1"
	id := "sha256:" + strings.Repeat("a", 64)
	t.Setenv("IMAGE_ID", id)
	const cli = `#!/bin/sh
set -eu
case "$1 $2" in
  "image ls") [ "$3" = --no-trunc ] && [ "$4" = --format ] || exit 2
              printf 'example:5000/runner\tv1\t%s\t4 KiB\n' "$IMAGE_ID";;
  "image inspect") [ "$5" = 'example:5000/runner:v1' ] || exit 2
              printf '{"Id":"%s","Architecture":"amd64","Size":4096,"Config":{"Labels":{"engine":"test"}}}' "$IMAGE_ID";;
  *) exit 2;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(cli), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	d := New()
	list, err := d.ListLocalImages(t.Context(), "")
	if err != nil || list.Runtime != "docker" || len(list.Items) != 1 || list.Items[0].Identity != id || list.Items[0].Ref != ref {
		t.Fatalf("list: %+v %v", list, err)
	}
	if _, err := d.ListLocalImages(t.Context(), "other"); !errors.Is(err, runtime.ErrLocalImageChanged) {
		t.Fatal("accepted namespace", err)
	}
	selection := proto.RuntimeImageSelection{Runtime: "docker", Ref: ref, Identity: id}
	for _, job := range []*runtime.LocalImageCopy{nil, {StateDir: dir}} {
		image, err := d.ResolveLocalImage(t.Context(), selection, job)
		if err != nil || image.ID != id || image.Labels["engine"] != "test" || image.RuntimeSizeBytes == nil || *image.RuntimeSizeBytes != 4096 {
			t.Fatalf("inspect: %+v %v", image, err)
		}
	}
	selection.Identity = "other"
	if _, err = d.ResolveLocalImage(t.Context(), selection, nil); !errors.Is(err, runtime.ErrLocalImageChanged) {
		t.Fatal("accepted stale identity", err)
	}
	selection.Identity, selection.TargetNamespace = id, "k8s.io"
	if _, err = d.ResolveLocalImage(t.Context(), selection, nil); !errors.Is(err, runtime.ErrLocalImageChanged) {
		t.Fatal("accepted namespace", err)
	}
}
