package udocker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/test/ocifixture"
)

// SaveImage makes a runveil node an image-sync source: what it saves, another
// runveil node loads as the same image and can create containers from, all
// without the udocker CLI on either side.
func TestSaveImageRoundTripsBetweenNodesWithoutCLI(t *testing.T) {
	const image = "registry.example/team/runner:v1"
	archive, err := ocifixture.DockerSaveArchive(ocifixture.DockerSaveOptions{RepoTags: []string{image}, LayerBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	source := cliFreeDriver(t)
	if err := source.LoadImage(context.Background(), image, bytes.NewReader(archive)); err != nil {
		t.Fatal(err)
	}
	var saved bytes.Buffer
	if err := source.SaveImage(context.Background(), image, &saved); err != nil {
		t.Fatalf("SaveImage: %v", err)
	}

	target := cliFreeDriver(t)
	if err := target.LoadImage(context.Background(), image, bytes.NewReader(saved.Bytes())); err != nil {
		t.Fatalf("loading a saved image on another node: %v", err)
	}
	want, err := source.InspectImage(context.Background(), image)
	if err != nil {
		t.Fatal(err)
	}
	got, err := target.InspectImage(context.Background(), image)
	if err != nil {
		t.Fatal(err)
	}
	if want.ID == "" || got.ID != want.ID {
		t.Fatalf("image ID %q on the target, %q on the source", got.ID, want.ID)
	}
	if handled, err := target.nativeCreate(context.Background(), "c", image, nil); !handled || err != nil {
		t.Fatalf("create from the synced image: handled=%v err=%v", handled, err)
	}
	if _, err := os.Stat(filepath.Join(target.udockerDir, "containers", "c", "ROOT", "etc", "gpuworks-fixture")); err != nil {
		t.Fatal(err)
	}
}

func TestSaveImageRefusals(t *testing.T) {
	t.Run("image not present", func(t *testing.T) {
		d := cliFreeDriver(t)
		var out bytes.Buffer
		err := d.SaveImage(context.Background(), "team/missing:v1", &out)
		if err == nil || !strings.Contains(err.Error(), "not in the native image store") || out.Len() != 0 {
			t.Fatalf("err=%v, wrote %d bytes", err, out.Len())
		}
	})
	t.Run("native store disabled", func(t *testing.T) {
		d := cliFreeDriver(t)
		registerNativeImage(t, d, "team/app:v1", `{"config":{}}`)
		t.Setenv(disableNativeEnv, "1")
		err := d.SaveImage(context.Background(), "team/app:v1", &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), disableNativeEnv) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("provisioning fails", func(t *testing.T) {
		base := t.TempDir()
		blocker := filepath.Join(base, "not-a-dir")
		if err := os.WriteFile(blocker, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		d := New(base)
		d.udockerDir = filepath.Join(blocker, "udocker")
		var out bytes.Buffer
		if err := d.SaveImage(context.Background(), "team/app:v1", &out); err == nil || out.Len() != 0 {
			t.Fatalf("SaveImage must fail before touching the store: err=%v, wrote %d bytes", err, out.Len())
		}
	})
}

// HoldsImage is asked of a runtime the node may never have used, so it answers
// from the store on disk and sets nothing up.
func TestHoldsImageReadsTheNativeStoreWithoutProvisioning(t *testing.T) {
	t.Run("never used", func(t *testing.T) {
		d := New(t.TempDir())
		held, err := d.HoldsImage(context.Background(), "team/app:v1")
		if err != nil || held {
			t.Fatalf("held=%v err=%v", held, err)
		}
		if d.ProvisionState().Provisioned {
			t.Fatal("asking provisioned the runtime")
		}
		if _, err := os.Stat(d.udockerDir); !os.IsNotExist(err) {
			t.Fatalf("asking created %s: %v", d.udockerDir, err)
		}
	})
	t.Run("held and missing", func(t *testing.T) {
		d := cliFreeDriver(t)
		registerNativeImage(t, d, "team/app:v1", `{"config":{}}`)
		if held, err := d.HoldsImage(context.Background(), "team/app:v1"); err != nil || !held {
			t.Fatalf("present image: held=%v err=%v", held, err)
		}
		if held, err := d.HoldsImage(context.Background(), "team/app:v2"); err != nil || held {
			t.Fatalf("other tag: held=%v err=%v", held, err)
		}
	})
	t.Run("native store disabled", func(t *testing.T) {
		d := cliFreeDriver(t)
		registerNativeImage(t, d, "team/app:v1", `{"config":{}}`)
		t.Setenv(disableNativeEnv, "1")
		if held, err := d.HoldsImage(context.Background(), "team/app:v1"); err != nil || held {
			t.Fatalf("an image SaveImage cannot stream is not held: held=%v err=%v", held, err)
		}
	})
}
