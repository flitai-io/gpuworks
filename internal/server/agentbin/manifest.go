package agentbin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ManifestName is produced alongside the embedded cross-compiled agents.
const ManifestName = "agent-manifest.json"

// Manifest records which release produced each exact, uncompressed binary.
// Override binaries must carry their own manifest rather than borrowing the
// embedded release's identity.
type Manifest struct {
	Version  string                    `json:"version"`
	Binaries map[string]ManifestBinary `json:"binaries"`
}

type ManifestBinary struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// ReadVerified reads from the same source as Read, but requires its release
// manifest to name the requested target and the actual bytes being returned.
func ReadVerified(overrideDir, name, targetVersion string) ([]byte, error) {
	source, entry, err := manifestEntry(overrideDir, name, targetVersion)
	if err != nil {
		return nil, err
	}
	raw, err := Read(source, name)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	if int64(len(raw)) != entry.Size || !strings.EqualFold(hex.EncodeToString(sum[:]), entry.SHA256) {
		return nil, fmt.Errorf("agent binary %s does not match its build manifest", name)
	}
	return raw, nil
}

// Available checks release metadata and file presence without expanding or hashing the binary.
func Available(dir, name, target string) bool {
	source, entry, err := manifestEntry(dir, name, target)
	if err != nil {
		return false
	}
	if source == "" {
		return Has(name)
	}
	info, err := os.Stat(filepath.Join(source, name))
	return err == nil && info.Mode().IsRegular() && info.Size() == entry.Size
}

func manifestEntry(overrideDir, name, targetVersion string) (string, ManifestBinary, error) {
	if !validName(name) || !strings.HasPrefix(name, "gpuworks-agent-") {
		return "", ManifestBinary{}, fs.ErrNotExist
	}
	targetVersion = strings.TrimSpace(targetVersion)
	if targetVersion == "" {
		return "", ManifestBinary{}, errors.New("agent target version is empty")
	}
	var manifestRaw []byte
	var err error
	source := ""
	if overrideDir != "" {
		st, statErr := os.Stat(filepath.Join(overrideDir, name))
		switch {
		case statErr == nil && !st.IsDir():
			source = overrideDir
		case statErr != nil && !errors.Is(statErr, fs.ErrNotExist):
			return "", ManifestBinary{}, fmt.Errorf("read agent override: %w", statErr)
		}
	}
	if source != "" {
		manifestRaw, err = os.ReadFile(filepath.Join(source, ManifestName))
	} else {
		manifestRaw, err = fs.ReadFile(binaries, "dl/"+ManifestName)
	}
	if err != nil {
		return "", ManifestBinary{}, fmt.Errorf("read agent build manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return "", ManifestBinary{}, fmt.Errorf("decode agent build manifest: %w", err)
	}
	if manifest.Version != targetVersion {
		return "", ManifestBinary{}, fmt.Errorf("agent build manifest version %q does not match target %q", manifest.Version, targetVersion)
	}
	entry, ok := manifest.Binaries[name]
	if !ok || entry.Size <= 0 || entry.Size > 1<<30 {
		return "", ManifestBinary{}, fmt.Errorf("agent build manifest has no valid entry for %s", name)
	}
	want, err := hex.DecodeString(entry.SHA256)
	if err != nil || len(want) != sha256.Size {
		return "", ManifestBinary{}, fmt.Errorf("agent build manifest has no valid SHA256 for %s", name)
	}
	return source, entry, nil
}

// WriteManifest records only the explicitly built files: scanning the output
// directory would accidentally bless a stale binary from an earlier platform
// matrix as part of this release.
func WriteManifest(version, output string, files []string) error {
	if strings.TrimSpace(version) == "" || len(files) == 0 {
		return errors.New("agent manifest requires a version and at least one binary")
	}
	manifest := Manifest{Version: version, Binaries: make(map[string]ManifestBinary, len(files))}
	for _, file := range files {
		name := filepath.Base(file)
		if !validName(name) || !strings.HasPrefix(name, "gpuworks-agent-") {
			return fmt.Errorf("invalid agent binary name %q", name)
		}
		if _, exists := manifest.Binaries[name]; exists {
			return fmt.Errorf("duplicate agent binary %q", name)
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		if len(raw) == 0 || len(raw) > 1<<30 {
			return fmt.Errorf("agent binary %s has invalid size", name)
		}
		sum := sha256.Sum256(raw)
		manifest.Binaries[name] = ManifestBinary{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(raw))}
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(output), ".agent-manifest-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), output)
}
