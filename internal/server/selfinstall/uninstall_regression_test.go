package selfinstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUninstallPurgeIncludesLogsRotatedDuringShutdown(t *testing.T) {
	h := uninstallFixture(t)
	rotated := h.layout.LogFile() + ".1"
	unrelated := h.layout.LogFile() + ".backup"
	h.run.on = func(cmd Command) {
		if cmd.Path == "systemctl" && len(cmd.Args) == 2 && cmd.Args[0] == "stop" {
			writeFile(t, rotated, "last shutdown records", 0o600)
			writeFile(t, unrelated, "operator backup", 0o600)
		}
	}
	report, err := h.installer.Uninstall(t.Context(), UninstallRequest{Resources: emptyUninstallResources, Yes: true, Purge: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertUninstallAbsent(t, rotated)
	if readFile(t, unrelated) != "operator backup" {
		t.Fatal("purge removed an unrelated log backup")
	}
	if !strings.Contains(strings.Join(report.Removed, "\n"), rotated) {
		t.Fatalf("report omitted shutdown rotation: %+v", report)
	}
}

func TestUninstallRefusesAmbiguousRelativeServeProcess(t *testing.T) {
	h := uninstallFixture(t)
	h.supervisedByNohup()
	h.run.output["ps -ww -axo pid=,command="] = "42 ./gpuworks serve --addr 127.0.0.1:8802\n"
	if _, err := h.installer.Uninstall(t.Context(), UninstallRequest{Resources: emptyUninstallResources, Yes: true, Purge: true}, nil); err == nil {
		t.Fatal("purge accepted a server whose relative executable cannot be identified")
	}
	if _, err := os.Stat(h.layout.Binary()); err != nil {
		t.Fatalf("program was deleted: %v", err)
	}
	if readFile(t, filepath.Join(h.layout.DataDir, "gpuworks.db")) != "old database" {
		t.Fatal("data was deleted")
	}
}

func TestUninstallIgnoresUnrelatedServerCommands(t *testing.T) {
	for _, command := range []string{"python3 server.py", "node server.js", "npm run serve", "python3 serve.py", "./server-helper server"} {
		t.Run(command, func(t *testing.T) {
			h := uninstallFixture(t)
			h.supervisedByNohup()
			h.run.output["ps -ww -axo pid=,command="] = "2147483647 " + command + "\n"
			if _, err := h.installer.Uninstall(t.Context(), UninstallRequest{Resources: emptyUninstallResources, Yes: true}, nil); err != nil {
				t.Fatalf("unrelated process blocked uninstall: %v", err)
			}
		})
	}
}

func TestUninstallIdentifiesAbsoluteTargetWithSpacesWithoutKernelIdentity(t *testing.T) {
	h := newHarness(t, filepath.Join(t.TempDir(), "my server", "gpuworks server"))
	h.writeExistingInstall("previous")
	h.supervisedByNohup()
	h.run.output["ps -ww -axo pid=,command="] = "2147483647 " + h.layout.Binary() + " serve --addr 127.0.0.1:8802\n"
	if _, err := h.installer.Uninstall(t.Context(), UninstallRequest{Resources: emptyUninstallResources, Yes: true, Purge: true}, nil); err == nil {
		t.Fatal("purge accepted an absolute target containing spaces")
	}
	if _, err := os.Stat(h.layout.Binary()); err != nil {
		t.Fatalf("program was deleted: %v", err)
	}
}

func TestUninstallRefusesSpacedAliasesWithoutKernelIdentity(t *testing.T) {
	for _, name := range []string{"absolute alias", "absolute serve alias", "relative alias", "relative alias without dot"} {
		t.Run(name, func(t *testing.T) {
			h := uninstallFixture(t)
			h.supervisedByNohup()
			program := "./my server/alias"
			if name == "relative alias without dot" {
				program = "my server/alias"
			}
			if !strings.HasPrefix(name, "relative alias") {
				dir := filepath.Join(t.TempDir(), "my server alias")
				if name == "absolute serve alias" {
					dir = filepath.Join(t.TempDir(), "my serve alias")
				}
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				program = filepath.Join(dir, "alias")
				if err := os.Symlink(h.layout.Binary(), program); err != nil {
					t.Fatal(err)
				}
			}
			h.run.output["ps -ww -axo pid=,command="] = "2147483647 " + program + " serve --addr 127.0.0.1:8802\n"
			if _, err := h.installer.Uninstall(t.Context(), UninstallRequest{Resources: emptyUninstallResources, Yes: true, Purge: true}, nil); err == nil {
				t.Fatal("purge accepted a spaced executable alias")
			}
			if _, err := os.Stat(h.layout.Binary()); err != nil {
				t.Fatalf("program was deleted: %v", err)
			}
		})
	}
}
