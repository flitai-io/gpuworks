package goldenimage

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareModelCommandNeverPullsAndHonoursThePrivilegePrefix(t *testing.T) {
	files := []File{{Path: "config.json", URL: "https://example.invalid/config.json", Size: 1}}
	for _, sudo := range []string{"", "sudo -n "} {
		command := PrepareModelCommand(DefaultModelPath, DefaultDockerImage, files, sudo)
		for _, want := range []string{sudo + "docker image inspect", sudo + "python3 /tmp/gpuworks-prepare-model.py", sudo + "sync"} {
			if !strings.Contains(command, want) {
				t.Errorf("sudo=%q: command lacks %q", sudo, want)
			}
		}
		if strings.Contains(command, "docker pull") {
			t.Errorf("sudo=%q: the model step must never pull an image", sudo)
		}
	}
	if command := PrepareModelCommand(DefaultModelPath, DefaultDockerImage, files, ""); strings.Contains(command, "sudo") {
		t.Errorf("a root builder must not need sudo: %s", command)
	}
}

func TestPrepareModelCommandEmbedsTheManifestAndAChecksumWritingScript(t *testing.T) {
	command := PrepareModelCommand("/m", "img", []File{{Path: "a/b.safetensors", URL: "https://x/y", Size: 7}}, "")
	fields := strings.Split(command, "'")
	var decoded []string
	for _, field := range fields {
		if raw, err := base64.StdEncoding.DecodeString(field); err == nil && len(field) > 16 {
			decoded = append(decoded, string(raw))
		}
	}
	joined := strings.Join(decoded, "\n")
	for _, want := range []string{"SHA256SUMS", "unsafe model path", "a/b.safetensors", `"size":7`} {
		if !strings.Contains(joined, want) {
			t.Errorf("embedded script or manifest lacks %q", want)
		}
	}
}

func TestValidationCommandChecksWeightsChecksumsAndTheImage(t *testing.T) {
	command := ValidationCommand("/opt/m", "gpuworks/gpuworks-runner:x", "sudo -n ")
	for _, want := range []string{"sudo -n sh -c", "config.json", "*.safetensors", "sha256sum -c SHA256SUMS", "docker image inspect"} {
		if !strings.Contains(command, want) {
			t.Errorf("validation lacks %q: %s", want, command)
		}
	}
}

func TestShellQuoteSurvivesQuotes(t *testing.T) {
	if got := ShellQuote("it's"); got != `'it'\''s'` {
		t.Fatalf("ShellQuote = %s", got)
	}
}

// fakeDocker puts a docker on PATH that logs each call and answers pull and
// image inspect from the space-separated lists given.
func fakeDocker(t *testing.T, pullable, present string) (string, func() string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "$*" >>"` + log + `"
case "$1 $2" in
"pull "*) for ok in ` + pullable + `; do [ "$2" = "$ok" ] && exit 0; done; exit 1;;
"image inspect") for ok in ` + present + `; do [ "$3" = "$ok" ] && exit 0; done; exit 1;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir, func() string {
		data, _ := os.ReadFile(log)
		return string(data)
	}
}

func TestPullImageCommandFallsThroughSourcesAndTagsTheFirstThatPulls(t *testing.T) {
	_, calls := fakeDocker(t, "mirror/runner:1", "runner:1")
	out, err := exec.Command("sh", "-c", strings.ReplaceAll(PullImageCommand("runner:1", []string{"down/runner:1", "mirror/runner:1", "runner:1"}, ""), "sleep 10", "true")).CombinedOutput()
	if err != nil {
		t.Fatalf("pull command: %v: %s", err, out)
	}
	got := calls()
	if strings.Count(got, "pull down/runner:1") != 3 || !strings.Contains(got, "tag mirror/runner:1 runner:1") || strings.Contains(got, "pull runner:1") {
		t.Fatalf("docker calls:\n%s", got)
	}
}

func TestPullImageCommandFailsWhenNoSourcePulls(t *testing.T) {
	fakeDocker(t, "", "")
	out, err := exec.Command("sh", "-c", strings.ReplaceAll(PullImageCommand("runner:1", []string{"a/runner:1"}, ""), "sleep 10", "true")).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "unavailable from every source") {
		t.Fatalf("err=%v out=%s", err, out)
	}
	if command := PullImageCommand("runner:1", []string{"a/runner:1"}, "sudo -n "); !strings.Contains(command, "sudo -n docker pull") || !strings.Contains(command, "sudo -n docker tag") {
		t.Fatalf("privilege prefix missing: %s", command)
	}
}

func TestRemoveImagesCommandDeletesOnlyPresentImagesAndNeverTheKeptOne(t *testing.T) {
	_, calls := fakeDocker(t, "", "old:1 new:1")
	out, err := exec.Command("sh", "-c", RemoveImagesCommand([]string{"old:1", "absent:1", "new:1", " "}, "new:1", "")).CombinedOutput()
	if err != nil {
		t.Fatalf("remove command: %v: %s", err, out)
	}
	got := calls()
	if !strings.Contains(got, "rmi old:1") || strings.Contains(got, "rmi absent:1") || strings.Contains(got, "new:1") || !strings.Contains(got, "image prune -f") {
		t.Fatalf("docker calls:\n%s", got)
	}
}
