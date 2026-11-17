package provision

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runMacCheck runs one macOS precheck command under sh with uname, sw_vers
// and sysctl answering as the given Mac would (output shapes recorded on an
// M4 Max, macOS 26.5).
func runMacCheck(t *testing.T, name, arch, version, chip string) (string, bool) {
	t.Helper()
	var command string
	for _, c := range macPrecheckCommands() {
		if c.id == name {
			if !c.required {
				t.Fatalf("%s must be required", name)
			}
			command = c.command
		}
	}
	bin := t.TempDir()
	for tool, body := range map[string]string{
		"uname":   `[ "$1" = -m ] && echo "` + arch + `"`,
		"sw_vers": `[ "$1" = -productVersion ] && echo "` + version + `"`,
		"sysctl":  `[ "$1 $2" = "-n machdep.cpu.brand_string" ] && echo "` + chip + `"`,
	} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := exec.Command("sh", "-c", command)
	c.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
	out, err := c.CombinedOutput()
	return string(out), err == nil
}

func TestMacPrecheckOSAndArch(t *testing.T) {
	for _, tc := range []struct {
		arch, version string
		ok            bool
		says          string
	}{
		{"arm64", "26.5", true, "macOS 26.5 arm64"},
		{"arm64", "15.4.1", true, "macOS 15.4.1 arm64"},
		{"arm64", "14.7.2", false, "macOS 15 or later"},
		{"x86_64", "15.4.1", false, "Intel Mac"},
	} {
		out, ok := runMacCheck(t, CheckIDOSArch, tc.arch, tc.version, "Apple M4 Max")
		if ok != tc.ok || !strings.Contains(out, tc.says) {
			t.Errorf("%s %s: ok=%v out=%q, want ok=%v and %q", tc.arch, tc.version, ok, out, tc.ok, tc.says)
		}
	}
}

func TestMacPrecheckGPU(t *testing.T) {
	if out, ok := runMacCheck(t, CheckIDGPU, "arm64", "26.5", "Apple M4 Max"); !ok || strings.TrimSpace(out) != "Apple M4 Max" {
		t.Fatalf("Apple silicon: ok=%v out=%q", ok, out)
	}
	if out, ok := runMacCheck(t, CheckIDGPU, "x86_64", "15.4.1", "Intel(R) Core(TM) i9-9980HK CPU @ 2.40GHz"); ok || !strings.Contains(out, "no Apple silicon GPU") {
		t.Fatalf("Intel: ok=%v out=%q", ok, out)
	}
}
