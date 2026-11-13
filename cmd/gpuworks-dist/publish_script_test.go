package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the actual publisher and adopt command. Only object storage and
// the build step are local fixtures; stop at assembly before any upload.
func TestPublishScriptRequiresDocsOnlyForUnpublishedReleases(t *testing.T) {
	const id = "v26.11.9"
	const archive = "gpuworks-" + id + "-linux-amd64.tar.gz"
	const oldTime, newTime = "2026-11-01T00:00:00Z", "2026-11-10T00:00:00Z"
	cases := []struct {
		name                        string
		published, oldDocs, newDocs bool
		corrupt, laterSource        bool
	}{
		{name: "legacy release without rebuilt docs", published: true},
		{name: "legacy release does not backfill rebuilt docs", published: true, newDocs: true},
		{name: "published docs survive rebuild without docs", published: true, oldDocs: true},
		{name: "new release without docs is refused"},
		{name: "new release with docs proceeds", newDocs: true},
		{name: "invalid published release is not adopted", published: true, corrupt: true},
		{name: "legacy release on a later source", published: true, laterSource: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, bin := t.TempDir(), t.TempDir()
			for _, name := range []string{"publish_dist.sh", "dist_remotes.sh"} {
				data, err := os.ReadFile(filepath.Join("..", "..", "scripts", name))
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(repo, "scripts", name)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				writeFile(t, path, string(data))
			}
			dist := buildDist(t, newTime, archive)
			if tc.newDocs {
				writeDocs(t, dist, id)
			}
			published := t.TempDir()
			if tc.published {
				packageArchive(t, published, archive, oldTime)
				digest, err := fileSHA256(filepath.Join(published, archive))
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(published, "SHA256SUMS"), digest+"  "+archive+"\n")
				if tc.oldDocs {
					writeDocs(t, published, id)
				}
				if tc.corrupt {
					writeFile(t, filepath.Join(published, "SHA256SUMS"), strings.Repeat("0", 64)+"  "+archive+"\n")
				}
			}
			writeExecutable(t, filepath.Join(bin, "go"), `#!/bin/sh
[ "$1" = build ] && [ "$2" = -o ] || exit 97
cp "$DIST_TEST_WRAPPER" "$3"
chmod +x "$3"
`)
			wrapper := filepath.Join(bin, "dist-helper")
			writeExecutable(t, wrapper, `#!/bin/sh
exec "$DIST_TEST_EXECUTABLE" -test.run '^TestPublishScriptProcess$' -- "$@"
`)
			storage := filepath.Join(bin, "rclone")
			writeExecutable(t, storage, `#!/bin/sh
case "$1" in
  copy)
    case "$2" in
      "$DIST_TEST_REMOTE":*/releases/*) mkdir -p "$3"; cp -R "$DIST_TEST_PUBLISHED/." "$3/" ;;
      */releases/*) exit 3 ;;
      *) exit 97 ;;
    esac ;;
  cat) exit 3 ;;
  *) exit 97 ;;
esac
`)
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			capture := filepath.Join(repo, "assembly-input")
			remote := "oss"
			if tc.laterSource {
				remote = "obs"
			}
			cmd := exec.Command("sh", filepath.Join(repo, "scripts", "publish_dist.sh"), "release", dist)
			cmd.Env = append(os.Environ(),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "RCLONE="+storage,
				"DIST_TEST_PROCESS=1", "DIST_TEST_EXECUTABLE="+executable, "DIST_TEST_WRAPPER="+wrapper,
				"DIST_TEST_PUBLISHED="+published, "DIST_TEST_CAPTURE="+capture,
				"DIST_TEST_REMOTE="+remote,
				"GPUWORKS_BUILD_COMMIT=abcdef0123456789", "GPUWORKS_BUILT_AT="+newTime,
				"GPUWORKS_DIST_SIGNING_KEY=fixture", "GPUWORKS_DIST_SIGNING_KEY_ID=fixture",
				"GPUWORKS_DIST_OSS_ACCESS_KEY_ID=fixture", "GPUWORKS_DIST_OSS_SECRET_ACCESS_KEY=fixture",
				"GPUWORKS_DIST_OBS_ACCESS_KEY_ID=fixture", "GPUWORKS_DIST_OBS_SECRET_ACCESS_KEY=fixture",
				"GPUWORKS_DIST_R2_ACCESS_KEY_ID=fixture", "GPUWORKS_DIST_R2_SECRET_ACCESS_KEY=fixture",
				"GPUWORKS_DIST_R2_ENDPOINT=https://fixture.invalid", "TMPDIR="+t.TempDir())
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("expected a gate failure or the assembly sentinel: %v\n%s", err, out)
			}
			adopted := tc.published && !tc.corrupt
			if !adopted && !tc.newDocs {
				if exit.ExitCode() != 1 || !strings.Contains(string(out), "formal releases require") {
					t.Fatalf("missing-docs gate: exit=%d\n%s", exit.ExitCode(), out)
				}
				if _, err := os.Stat(capture); !os.IsNotExist(err) {
					t.Fatalf("missing docs reached assembly: %v", err)
				}
				return
			}
			if exit.ExitCode() != 91 {
				t.Fatalf("should reach assembly: exit=%d\n%s", exit.ExitCode(), out)
			}
			wantDir, wantTime, wantDocs := dist, newTime, tc.newDocs
			if adopted {
				wantDir, wantTime, wantDocs = published, oldTime, tc.oldDocs
			}
			digest, err := fileSHA256(filepath.Join(wantDir, archive))
			if err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("%s\n%s\n%t\n", wantTime, digest, wantDocs)
			if got := string(readTree(t, repo, "assembly-input")); got != want {
				t.Fatalf("assembly input = %q, want published bytes/config %q", got, want)
			}
		})
	}
}

func TestPublishScriptProcess(t *testing.T) {
	if os.Getenv("DIST_TEST_PROCESS") != "1" {
		return
	}
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) == 0 {
		os.Exit(97)
	}
	switch args[0] {
	case "newer":
		// All fixture sources have no channel pointer, matching rclone's cat.
		os.Exit(1)
	case "assemble":
		flags := map[string]string{}
		for i := 1; i+1 < len(args); i += 2 {
			flags[args[i]] = args[i+1]
		}
		archives, _, err := findArchives(flags["-dist"], flags["-release-id"])
		if err != nil || len(archives) != 1 {
			os.Exit(97)
		}
		digest, err := fileSHA256(filepath.Join(flags["-dist"], archives[0].name))
		if err != nil {
			os.Exit(97)
		}
		_, docsErr := os.Stat(filepath.Join(flags["-dist"], "docs", "SHA256SUMS"))
		body := fmt.Sprintf("%s\n%s\n%t\n", flags["-built-at"], digest, docsErr == nil)
		if err := os.WriteFile(os.Getenv("DIST_TEST_CAPTURE"), []byte(body), 0o644); err != nil {
			os.Exit(97)
		}
		os.Exit(91)
	default:
		os.Exit(run(args, os.Stdin, os.Stdout, os.Stderr))
	}
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
