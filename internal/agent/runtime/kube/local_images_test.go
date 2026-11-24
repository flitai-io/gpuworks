package kube

import (
	"context"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

type localImageFixture struct {
	fakeImages
	namespace string
	selected  proto.RuntimeImageSelection
	copy      *runtime.LocalImageCopy
}

func (f *localImageFixture) ListLocalImages(_ context.Context, ns string) (proto.RuntimeImageList, error) {
	f.namespace = ns
	return proto.RuntimeImageList{Runtime: "nerdctl", Namespace: ns, TargetNamespace: "k8s.io"}, nil
}
func (f *localImageFixture) ResolveLocalImage(_ context.Context, p proto.RuntimeImageSelection, job *runtime.LocalImageCopy) (runtime.ImageInspection, error) {
	f.selected, f.copy = p, job
	return runtime.ImageInspection{ID: p.Identity}, nil
}
func (f *localImageFixture) NeedsLocalImageCopy(_ context.Context, ref, id, state string) (bool, error) {
	return ref == f.selected.Ref && id == f.selected.Identity && state == "/agent", nil
}
func TestKubeRuntimeImageDelegation(t *testing.T) {
	f := &localImageFixture{}
	d := &Driver{o: Options{Images: f}}
	list, err := d.ListLocalImages(t.Context(), "default")
	if err != nil || f.namespace != "default" || list.TargetNamespace != "k8s.io" {
		t.Fatal("namespace delegation", list, err)
	}
	p := proto.RuntimeImageSelection{Namespace: "default", TargetNamespace: "k8s.io", Ref: "runner:v1", Identity: "digest", Runtime: list.Runtime}
	for _, job := range []*runtime.LocalImageCopy{nil, {StateDir: t.TempDir()}} {
		img, err := d.ResolveLocalImage(t.Context(), p, job)
		if err != nil || f.copy != job || f.selected != p || img.ID != p.Identity {
			t.Fatal("incorrect delegated selection or copy", err)
		}
	}
	if needed, err := d.NeedsLocalImageCopy(t.Context(), p.Ref, p.Identity, "/agent"); err != nil || !needed {
		t.Fatal("target preflight not delegated", needed, err)
	}
	d.o.Images = &fakeImages{}
	if _, err := d.ListLocalImages(t.Context(), "default"); !errors.Is(err, runtime.ErrLocalImageUnsupported) {
		t.Fatal(err)
	}
	if _, err := d.ResolveLocalImage(t.Context(), p, nil); !errors.Is(err, runtime.ErrLocalImageUnsupported) {
		t.Fatal(err)
	}
}
