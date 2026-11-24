package nerdctl

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/grpcwire"
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
if [ "$1" = --help ]; then echo '  --address string  containerd address (default "unix:///containerd.sock")'; exit; fi
[ "$1" = "-n" ]
ns=$2
shift 2
cd "$TEST_IMAGE_STORE/$ns"
mkdir -p gpuworks.internal
if [ "$1" = load ]; then read -r stage; cat > "$stage"; [ "${FAIL_COPY:-}" != load ]; exit; fi
case "$1 ${2:-}" in
  'namespace ls') printf 'default\nk8s.io\n' ;;
  'images --no-trunc') printf 'runner\tv1\t%s\t4 KiB\n<none>\t<none>\t%s\t1 KiB\n' "$TEST_TARGET" "$TEST_TARGET" ;;
  'image inspect') shift 5; [ ! -f "$1" ] || cat "$1" ;;
  'image rm') rm -f -- "$3" ;;
  'tag '*) cp -- "$2" "$3" ;;
  'save '*) [ "${FAIL_COPY:-}" != save ] || exit 2; echo "$2"; cat "$2"; echo copied >> "$TEST_IMAGE_STORE/transfers" ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "nerdctl"), []byte(cli), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("TEST_IMAGE_STORE", dir)
	t.Setenv("TEST_TARGET", target)
	other := strings.Replace(image, config, "sha256:"+strings.Repeat("c", 64), 1)
	var atCreate atomic.Bool
	d := testLocalImagePublisher(t, dir, target, other, &atCreate)
	finalPath := filepath.Join(dir, "k8s.io/runner:v1")
	list, err := d.ListLocalImages(t.Context(), "default")
	if err != nil || list.TargetNamespace != "k8s.io" || list.Namespace != "default" || len(list.Items) != 1 || list.Items[0].Ref != "runner:v1" {
		t.Fatalf("list %+v: %v", list, err)
	}
	p := proto.RuntimeImageSelection{Runtime: list.Runtime, Namespace: list.Namespace, TargetNamespace: list.TargetNamespace, Ref: list.Items[0].Ref, Identity: list.Items[0].Identity}
	job := &runtime.LocalImageCopy{StateDir: dir}
	preview, err := d.ResolveLocalImage(t.Context(), p, nil)
	if err != nil || preview.ID != config || preview.Labels["io.gpuworks.engine"] != "vllm" {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "k8s.io/runner:v1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preview copied an image: %v", err)
	}
	for _, failure := range []string{"save", "load", "socket"} {
		t.Run(failure, func(t *testing.T) {
			t.Setenv("FAIL_COPY", failure)
			driver := d
			if failure == "socket" {
				driver = NewInNamespace(d.ns).WithHostRoot(t.TempDir())
			}
			_, err := driver.ResolveLocalImage(t.Context(), p, job)
			if err == nil {
				t.Fatal("failed import reported success")
			}
			if _, err := os.Stat(finalPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("published target on failure: %v", err)
			}
			for _, ns := range []string{"default", "k8s.io"} {
				stages, err := filepath.Glob(filepath.Join(dir, ns, localImportStagePrefix+"*"))
				if err != nil || len(stages) > 0 {
					t.Fatalf("stage left in %s: %v %v", ns, stages, err)
				}
			}
			if failure == "socket" && job.Bytes.Load() != 0 {
				t.Fatal("streamed before checking socket")
			}
			job.Bytes.Store(0)
		})
	}
	// Simulate a process exit with a durable marker and both staged tags.
	j, err := d.openImportJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	stage, marker, err := journalImportTag(j.Name(), "default")
	j.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"default", "k8s.io"} {
		write(filepath.Join(ns, stage), image)
		write(filepath.Join(ns, localImportStagePrefix+"operator"), image)
	}
	os.Remove(filepath.Join(dir, "transfers")) // the failed load consumed a successful export
	if needed, err := d.NeedsLocalImageCopy(t.Context(), p.Ref, config, dir); err != nil || !needed {
		t.Fatal("missing destination", needed, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preflight did not recover the previous attempt", err)
	}
	for i := 0; i < 2; i++ {
		installed, err := d.ResolveLocalImage(t.Context(), p, job)
		if err != nil || installed.ID != config {
			t.Fatalf("import %d: %+v %v", i, installed, err)
		}
	}
	for _, ns := range []string{"default", "k8s.io"} {
		if _, err := os.Stat(filepath.Join(dir, ns, stage)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale tag in %s", ns)
		}
		if _, err := os.Stat(filepath.Join(dir, ns, localImportStagePrefix+"operator")); err != nil {
			t.Fatal("removed an unowned tag", err)
		}
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale marker", err)
	}
	if needed, err := d.NeedsLocalImageCopy(t.Context(), p.Ref, config, dir); err != nil || needed {
		t.Fatal("destination should be reused", needed, err)
	}
	if job.Bytes.Load() == 0 {
		t.Fatal("no copy progress")
	}
	copies, err := os.ReadFile(filepath.Join(dir, "transfers"))
	if err != nil || string(copies) != "copied\n" {
		t.Fatalf("transfers: %q %v", copies, err)
	}
	if err := os.Remove(finalPath); err != nil {
		t.Fatal(err)
	}
	atCreate.Store(true)
	if _, err := d.ResolveLocalImage(t.Context(), p, job); !errors.Is(err, runtime.ErrLocalImageConflict) {
		t.Fatalf("concurrent create should preserve the other image: %v", err)
	}
	// The ordinary conflict also leaves the competing tag intact.
	if _, err := d.ResolveLocalImage(t.Context(), p, job); !errors.Is(err, runtime.ErrLocalImageConflict) {
		t.Fatalf("expected conflict: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "k8s.io/runner:v1"))
	if err != nil || string(body) != other {
		t.Fatal("conflicting target was modified")
	}
	p.Identity = config
	if _, err := d.ResolveLocalImage(t.Context(), p, job); !errors.Is(err, runtime.ErrLocalImageChanged) {
		t.Fatalf("stale selection: %v", err)
	}
}

// testLocalImagePublisher serves the two containerd image RPCs used by publishLocalImage.
func testLocalImagePublisher(t *testing.T, dir, target, other string, atCreate *atomic.Bool) *Driver {
	t.Helper()
	// A short socket path also works when CI places t.TempDir under a long path.
	host, err := os.MkdirTemp("", "gw-rpc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(host) })
	finalPath := filepath.Join(dir, "k8s.io/runner:v1")
	var descriptor grpcwire.Message
	desc := descriptor.String(2, target).Encode()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail := func(err error) {
			t.Error(err)
			grpcwire.Fail(w, grpcwire.CodeInternal, "fixture failure")
		}
		if ns := r.Header.Get("containerd-namespace"); ns != "k8s.io" {
			fail(fmt.Errorf("namespace %q", ns))
			return
		}
		req, err := grpcwire.ReadFrame(r.Body)
		if err != nil {
			fail(err)
			return
		}
		var name string
		var img grpcwire.Message
		switch r.URL.Path {
		case "/containerd.services.images.v1.Images/Get":
			err = grpcwire.Fields(req, func(n, w int, b []byte) error {
				if n == 1 {
					name = string(b)
				}
				return nil
			})
			if name == "docker.io/library/runner:v1" {
				grpcwire.Fail(w, grpcwire.CodeNotFound, "image not found")
				return
			}
			if err != nil || !strings.HasPrefix(name, localImportStagePrefix) {
				fail(fmt.Errorf("stage %q: %v", name, err))
				return
			}
			if _, err = os.Stat(filepath.Join(dir, "k8s.io", name)); err != nil {
				fail(err)
				return
			}
			var response grpcwire.Message
			response.Bytes(1, img.String(1, name).Bytes(3, desc).Encode())
			grpcwire.StartResponse(w)
			_ = grpcwire.Send(w, response.Encode())
			grpcwire.Finish(w, 0, "")
		case "/containerd.services.images.v1.Images/Create":
			err = grpcwire.Fields(req, func(n, w int, b []byte) error {
				if n != 1 {
					return nil
				}
				return grpcwire.Fields(b, func(field, w int, val []byte) error {
					switch field {
					case 1:
						name = string(val)
					case 3:
						if string(val) != string(desc) {
							return errors.New("wrong descriptor")
						}
					}
					return nil
				})
			})
			if err != nil || name != "docker.io/library/runner:v1" {
				fail(fmt.Errorf("destination %q: %v", name, err))
				return
			}
			if atCreate.Load() {
				if err := os.WriteFile(finalPath, []byte(other), 0600); err != nil {
					fail(err)
					return
				}
			}
			if err := os.Link(filepath.Join(dir, "default/runner:v1"), finalPath); errors.Is(err, os.ErrExist) {
				grpcwire.Fail(w, grpcwire.CodeAlreadyExists, "image already exists")
				return
			} else if err != nil {
				fail(err)
				return
			}
			grpcwire.StartResponse(w)
			grpcwire.Finish(w, 0, "")
		default:
			fail(fmt.Errorf("unexpected RPC %s", r.URL.Path))
		}
	})
	stop, err := grpcwire.Listen(t.Context(), filepath.Join(host, "containerd.sock"), handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return NewInNamespace("k8s.io").WithHostRoot(host)
}
