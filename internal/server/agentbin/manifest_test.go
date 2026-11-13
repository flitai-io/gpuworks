package agentbin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func manifestBytes(t *testing.T, version, name string, raw []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(raw)
	b, err := json.Marshal(Manifest{Version: version, Binaries: map[string]ManifestBinary{name: {SHA256: hex.EncodeToString(sum[:]), Size: int64(len(raw))}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReadVerifiedUsesExpandedEmbeddedArtifact(t *testing.T) {
	name := "gpuworks-agent-linux-amd64"
	raw := bytes.Repeat([]byte("binary"), 512)
	tree := testTree(t, raw)
	tree["dl/"+ManifestName] = &fstest.MapFile{Data: manifestBytes(t, "dev-build", name, raw)}
	useTree(t, tree)
	if !Available("", name, "dev-build") || Available("", name, "other") {
		t.Fatal("embedded availability must follow the target manifest")
	}
	got, err := ReadVerified("", name, "dev-build")
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("verified embedded=%d bytes err=%v", len(got), err)
	}
	for _, tc := range []struct{ name, version string }{{"../secret", "dev-build"}, {name, ""}, {name, "other-build"}, {"gpuworks-agent-linux-arm64", "dev-build"}} {
		if _, err := ReadVerified("", tc.name, tc.version); err == nil {
			t.Fatalf("unexpected verified artifact: %+v", tc)
		}
	}
}

func TestReadVerifiedRefusesAnUnmanifestedOverride(t *testing.T) {
	name := "gpuworks-agent-linux-amd64"
	original := []byte("embedded agent")
	tree := testTree(t, original)
	tree["dl/"+ManifestName] = &fstest.MapFile{Data: manifestBytes(t, "target", name, original)}
	useTree(t, tree)
	dir := t.TempDir()
	if _, err := ReadVerified(dir, name, "target"); err != nil {
		t.Fatal(err)
	}
	raw := []byte("patched agent")
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o755); err != nil {
		t.Fatal(err)
	}
	if Available(dir, name, "target") {
		t.Fatal("override without its own manifest was offered")
	}
	if _, err := ReadVerified(dir, name, "target"); err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("unmanifested override accepted: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestName), manifestBytes(t, "target", name, raw), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadVerified(dir, name, "target")
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("override=%q err=%v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("changed later"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadVerified(dir, name, "target"); err == nil {
		t.Fatal("manifest did not reject changed binary")
	}
}

func TestReadVerifiedRejectsInvalidManifestFacts(t *testing.T) {
	name := "gpuworks-agent-linux-amd64"
	raw := []byte("agent")
	for _, tc := range []struct{ name, manifest string }{
		{"malformed", "{"},
		{"wrong build", string(manifestBytes(t, "old", name, raw))},
		{"missing entry", `{"version":"target","binaries":{}}`},
		{"invalid digest", `{"version":"target","binaries":{"gpuworks-agent-linux-amd64":{"sha256":"nope","size":5}}}`},
		{"unknown size", `{"version":"target","binaries":{"gpuworks-agent-linux-amd64":{"sha256":"0000000000000000000000000000000000000000000000000000000000000000","size":0}}}`},
		{"wrong size", strings.Replace(string(manifestBytes(t, "target", name, raw)), `"size":5`, `"size":6`, 1)},
		{"wrong digest", string(manifestBytes(t, "target", name, []byte("other")))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTree(t, fstest.MapFS{"dl/" + name: &fstest.MapFile{Data: raw}, "dl/" + ManifestName: &fstest.MapFile{Data: []byte(tc.manifest)}})
			if _, err := ReadVerified("", name, "target"); err == nil {
				t.Fatal("invalid build facts accepted")
			}
		})
	}
}

func TestWriteManifestNamesOnlyExplicitBuildOutputs(t *testing.T) {
	dir := t.TempDir()
	name := "gpuworks-agent-linux-amd64"
	file := filepath.Join(dir, name)
	if err := os.WriteFile(file, []byte("built target"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gpuworks-agent-linux-arm64"), []byte("stale build"), 0o755); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, ManifestName)
	if err := WriteManifest("target", output, []string{file}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadVerified(dir, name, "target")
	if err != nil || string(got) != "built target" {
		t.Fatalf("produced manifest=%q err=%v", got, err)
	}
	var manifest Manifest
	encoded, _ := os.ReadFile(output)
	if err := json.Unmarshal(encoded, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Binaries) != 1 {
		t.Fatalf("stale platform was blessed: %+v", manifest)
	}
	if err := WriteManifest("target", output, []string{file, file}); err == nil {
		t.Fatal("duplicate build input accepted")
	}
	if err := WriteManifest("", output, []string{file}); err == nil {
		t.Fatal("empty version accepted")
	}
	after, _ := os.ReadFile(output)
	if !bytes.Equal(after, encoded) {
		t.Fatal("failed generation damaged previous manifest")
	}
}
