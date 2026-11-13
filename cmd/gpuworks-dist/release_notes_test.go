package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A release build refuses to start without the version's release notes, so
// a tag cannot publish packages the documentation does not describe. The
// script runs from a copy in a scratch repository whose docs are arranged per
// case; an unsupported architecture stops it right after the notes check, so
// passing that check is visible without building anything.
func TestPackageReleaseRequiresReleaseNotes(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "package_release.sh"))
	if err != nil {
		t.Fatal(err)
	}
	const id = "v26.11.9"
	page := "# " + id + "\n"
	row := "| [" + id + "](" + id + ".md) | 2026-11-30 | 补丁版本 | 修复 |\n"
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"no page", map[string]string{"docs/releases/README.md": row}, "add docs/releases/" + id + ".md before tagging"},
		{"no English page", map[string]string{"docs/releases/README.md": row, "docs/releases/" + id + ".md": page}, "add docs-en/releases/" + id + ".md before tagging"},
		{"not listed", map[string]string{"docs/releases/README.md": "| [v26.11.8](v26.11.8.md) |\n", "docs/releases/" + id + ".md": page, "docs-en/releases/" + id + ".md": page}, "is not listed in docs/releases/README.md"},
		{"complete", map[string]string{"docs/releases/README.md": row, "docs/releases/" + id + ".md": page, "docs-en/releases/" + id + ".md": page}, "unsupported release arch: none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			files := map[string]string{"scripts/package_release.sh": string(script)}
			for name, body := range tc.files {
				files[name] = body
			}
			for name, body := range files {
				path := filepath.Join(repo, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("sh", filepath.Join(repo, "scripts", "package_release.sh"))
			cmd.Env = append(os.Environ(), "GPUWORKS_RELEASE_CHANNEL=release", "GPUWORKS_RELEASE_ID="+id,
				"GPUWORKS_RELEASE_ARCHES=none", "GPUWORKS_BUILD_COMMIT=0123456789abcdef", "TMPDIR="+t.TempDir())
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(string(out), tc.want) {
				t.Fatalf("err=%v output=%q, want exit 2 with %q", err, out, tc.want)
			}
		})
	}
}

// A nightly build needs no release notes.
func TestPackageReleaseNightlySkipsReleaseNotes(t *testing.T) {
	cmd := exec.Command("sh", filepath.Join("..", "..", "scripts", "package_release.sh"))
	cmd.Env = append(os.Environ(), "GPUWORKS_RELEASE_CHANNEL=nightly", "GPUWORKS_RELEASE_ID=0123abcd",
		"GPUWORKS_RELEASE_ARCHES=none", "TMPDIR="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "unsupported release arch: none") || strings.Contains(string(out), "release notes") {
		t.Fatalf("err=%v output=%q", err, out)
	}
}
