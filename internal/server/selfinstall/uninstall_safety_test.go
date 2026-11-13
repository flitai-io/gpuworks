package selfinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestUninstallRejectsSharedServiceBinary(t *testing.T) {
	for _, manager := range []string{"systemd", "launchd", "symlink", "override"} {
		t.Run(manager, func(t *testing.T) {
			h := uninstallFixture(t)
			switch manager {
			case "systemd":
				writeFile(t, h.layout.UnitPath(), RenderUnit(UnitConfig{Binary: h.layout.Binary(), ConfigPath: "/another/server.yaml", DataDir: "/another/data"}), 0o644)
			case "launchd":
				h.supervisedByNohup()
				writeFile(t, h.layout.LaunchdPath(), RenderPlist(UnitConfig{Binary: h.layout.Binary(), ConfigPath: "/another/server.yaml"}), 0o644)
			case "symlink":
				raw := readFile(t, h.layout.UnitPath())
				if err := os.Remove(h.layout.UnitPath()); err != nil {
					t.Fatal(err)
				}
				actual := filepath.Join(t.TempDir(), "gpuworks.service")
				writeFile(t, actual, raw, 0o644)
				if err := os.Symlink(actual, h.layout.UnitPath()); err != nil {
					t.Fatal(err)
				}
			case "override":
				writeFile(t, filepath.Join(h.layout.UnitPath()+".d", "override.conf"), "[Service]\nExecStart=/another/gpuworks serve\n", 0o644)
			}
			before := uninstallSnapshot(t, h.layout.Dir, h.layout.UnitDir, h.layout.LaunchdDir)
			if _, err := h.installer.Uninstall(t.Context(), UninstallRequest{Resources: emptyUninstallResources, Yes: true, Purge: true}, nil); err == nil {
				t.Fatal("ambiguous service ownership was accepted")
			}
			if after := uninstallSnapshot(t, h.layout.Dir, h.layout.UnitDir, h.layout.LaunchdDir); !reflect.DeepEqual(before, after) {
				t.Fatal("ownership failure changed files")
			}
		})
	}
}

func TestUninstallDoesNotDeleteWhenProcessesCannotBeConfirmedGone(t *testing.T) {
	for _, table := range []string{"", "broken", "42 /elsewhere/gpuworks serve\n43 TARGET serve --config /different/server.yaml\n"} {
		h := uninstallFixture(t)
		h.supervisedByNohup()
		h.run.output["ps -ww -axo pid=,command="] = strings.ReplaceAll(table, "TARGET", h.layout.Binary())
		if _, err := h.installer.Uninstall(t.Context(), UninstallRequest{Resources: emptyUninstallResources, Yes: true, Purge: true}, nil); err == nil {
			t.Fatalf("uninstall accepted process table %q", table)
		}
		if readFile(t, h.layout.Binary()) != "old binary" || readFile(t, filepath.Join(h.layout.DataDir, "gpuworks.db")) != "old database" {
			t.Fatal("unconfirmed process shutdown removed program or data")
		}
	}
}

func TestUninstallRejectsDamagedPIDFile(t *testing.T) {
	for _, content := range []string{"", "0\n", "-1", "broken"} {
		h := uninstallFixture(t)
		h.supervisedByNohup()
		writeFile(t, h.layout.PIDFile(), content, 0o644)
		if _, err := h.installer.Uninstall(t.Context(), UninstallRequest{Resources: emptyUninstallResources, Yes: true}, nil); err == nil || !strings.Contains(err.Error(), "invalid pid") {
			t.Fatalf("pid %q: %v", content, err)
		}
		if readFile(t, h.layout.PIDFile()) != content {
			t.Fatal("damaged PID file removed")
		}
	}
}

func TestUninstallRestoresDisabledServiceDefinitionWhenReloadFails(t *testing.T) {
	h := uninstallFixture(t)
	original := readFile(t, h.layout.UnitPath())
	h.run.fail["sudo systemctl daemon-reload"] = errors.New("injected reload failure")
	if _, err := h.installer.Uninstall(context.Background(), UninstallRequest{Resources: emptyUninstallResources, Yes: true, Purge: true}, nil); err == nil {
		t.Fatal("reload failure hidden")
	}
	if readFile(t, h.layout.UnitPath()) != original || readFile(t, h.layout.Binary()) != "old binary" {
		t.Fatal("failed reload lost installation identity or program")
	}
	delete(h.run.fail, "sudo systemctl daemon-reload")
	if _, err := h.installer.Uninstall(t.Context(), UninstallRequest{Resources: emptyUninstallResources, Yes: true, Purge: true}, nil); err != nil {
		t.Fatalf("retry after reload failure: %v", err)
	}
}
