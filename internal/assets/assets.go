// Package assets embeds binary assets shipped inside the agent: the validated
// static-nofd proot for the udocker runtime and the HAMi libvgpu built on
// Rocky 8.4 with a GLIBC_2.17 symbol baseline, with a sha256 manifest for
// integrity verification. Each agent binary embeds the files for its own
// OS and architecture. The on-disk tree under bin/ remains the full set.
package assets

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
)

const (
	UdockerBundleVersion = "v1.1.2"
	udockerExtractFormat = "v2"
)

// udockerBundleAssets maps the host architectures with a validated offline
// udocker bundle to their embedded archive. Only architectures with a
// patched proot that has passed the build's mandatory udocker/AF_UNIX gates
// (see build/runtime-assets/proot/README.md) belong here.
var udockerBundleAssets = map[string]string{
	"amd64": "bin/udocker-offline-x86_64-v1.1.2.tar.gz",
	"arm64": "bin/udocker-offline-arm64-v1.1.2.tar.gz",
}

// udockerBundleAssetForArch returns the embedded offline udocker bundle path
// for goarch, or "" if the architecture has no validated bundle.
func udockerBundleAssetForArch(goarch string) string {
	return udockerBundleAssets[goarch]
}

// libvgpuPlaceholder marks a not-yet-built libvgpu asset. The real asset is
// built on the oldest supported glibc baseline so one binary works in both
// Rocky/RHEL 8.4 and newer Ubuntu containers.
const libvgpuPlaceholder = "PLACEHOLDER"

// ExtractLibvgpu writes the embedded HAMi libvgpu.so to destPath and returns it.
// Returns ErrLibvgpuUnavailable when only the placeholder is embedded (the real
// .so is built from Project-HAMi/HAMi-core and swapped in at build time).
func ExtractLibvgpu(destPath string) (string, error) {
	data, err := files.ReadFile("bin/libvgpu.so")
	if err != nil {
		return "", err
	}
	if len(data) < 4096 && len(data) >= len(libvgpuPlaceholder) && string(data[:len(libvgpuPlaceholder)]) == libvgpuPlaceholder {
		return "", ErrLibvgpuUnavailable
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return "", err
	}
	tmpFile, err := os.CreateTemp(filepath.Dir(destPath), ".libvgpu-*")
	if err != nil {
		return "", err
	}
	tmp := tmpFile.Name()
	defer os.Remove(tmp)
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return "", err
	}
	if err := tmpFile.Chmod(0o755); err != nil {
		_ = tmpFile.Close()
		return "", err
	}
	if err := tmpFile.Close(); err != nil {
		return "", err
	}
	return destPath, os.Rename(tmp, destPath)
}

// LibvgpuAvailable reports whether a real libvgpu.so is embedded.
func LibvgpuAvailable() bool {
	data, err := files.ReadFile("bin/libvgpu.so")
	if err != nil || len(data) < 4096 {
		return false
	}
	return true
}

// ErrLibvgpuUnavailable indicates GPU partitioning is not built into this agent.
var ErrLibvgpuUnavailable = errLibvgpu("libvgpu (HAMi) not embedded; GPU partitioning unavailable")

type errLibvgpu string

func (e errLibvgpu) Error() string { return string(e) }

// ProotVariant identifies which validated proot to extract.
type ProotVariant string

const (
	// ProotStaticNoFD is the fully-static proot validated with the vLLM rootfs.
	ProotStaticNoFD ProotVariant = "static-nofd"
)

// prootAssets maps the host architectures with a validated patched proot to
// their embedded binary. See build/runtime-assets/proot/README.md for the
// build provenance and the mandatory udocker/AF_UNIX gates each one passed.
var prootAssets = map[string]string{
	"amd64": "bin/proot-x86_64-static-nofd",
	"arm64": "bin/proot-arm64-static-nofd",
}

func embeddedName(v ProotVariant) string {
	return embeddedNameForArch(v, goruntime.GOARCH)
}

func embeddedNameForArch(v ProotVariant, goarch string) string {
	switch v {
	case ProotStaticNoFD:
		return prootAssets[goarch]
	default:
		return ""
	}
}

// ExtractProot writes the requested proot variant to destPath (0755) if not
// already present with the correct sha256, and returns the path.
func ExtractProot(v ProotVariant, destPath string) (string, error) {
	name := embeddedName(v)
	data, err := files.ReadFile(name)
	if err != nil {
		return "", err
	}
	want := sha256hex(data)

	if existing, err := os.ReadFile(destPath); err == nil && sha256hex(existing) == want {
		return destPath, nil // already extracted and intact
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return "", err
	}
	tmp := destPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, destPath); err != nil {
		return "", err
	}
	return destPath, nil
}

// ExtractUdockerBundle expands the embedded, self-contained udocker release
// into destDir. The archive contains the Python CLI and englib 1.2.11, so
// provisioning never needs pip or an englib download. Extraction is skipped
// when the on-disk bundle hash already matches the embedded archive.
func ExtractUdockerBundle(destDir string) (string, error) {
	asset := udockerBundleAssetForArch(goruntime.GOARCH)
	if asset == "" {
		return "", fmt.Errorf("no embedded offline udocker bundle for %s", goruntime.GOARCH)
	}
	data, err := files.ReadFile(asset)
	if err != nil {
		return "", err
	}
	want := "v2:" + sha256hex(data)
	marker := filepath.Join(destDir, ".gpuworks-bundle.sha256")
	if existing, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(existing)) == want && validUdockerBundle(destDir) {
		return destDir, nil
	}

	parent := filepath.Dir(destDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(parent, ".udocker-bundle-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("open embedded udocker bundle: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read embedded udocker bundle: %w", err)
		}
		rel, ok := bundleRelativePath(hdr.Name)
		if !ok || rel == "." {
			continue
		}
		target := filepath.Join(tmp, rel)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode).Perm()); err != nil {
				return "", err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode).Perm())
			if err != nil {
				return "", err
			}
			_, copyErr := io.Copy(f, tr)
			closeErr := f.Close()
			if copyErr != nil {
				return "", copyErr
			}
			if closeErr != nil {
				return "", closeErr
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			_ = os.RemoveAll(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return "", err
			}
		case tar.TypeLink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			linkTarget, ok := bundleRelativePath(hdr.Linkname)
			if !ok {
				return "", fmt.Errorf("invalid hardlink target %q in embedded udocker bundle", hdr.Linkname)
			}
			_ = os.RemoveAll(target)
			if err := os.Link(filepath.Join(tmp, linkTarget), target); err != nil {
				return "", err
			}
		default:
			return "", fmt.Errorf("unsupported entry %q in embedded udocker bundle", hdr.Name)
		}
	}
	// MkdirTemp creates the extraction root as 0700. The agent commonly runs as
	// root while udocker itself runs as a dedicated non-root user, so that user
	// must be able to traverse the bundle root and read its Python entrypoint.
	if err := os.Chmod(tmp, 0o755); err != nil {
		return "", err
	}
	if !validUdockerBundle(tmp) {
		return "", fmt.Errorf("embedded udocker bundle is incomplete")
	}
	if err := os.WriteFile(filepath.Join(tmp, ".gpuworks-bundle.sha256"), []byte(want+"\n"), 0o644); err != nil {
		return "", err
	}
	if err := os.RemoveAll(destDir); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, destDir); err != nil {
		return "", err
	}
	return destDir, nil
}

func bundleRelativePath(name string) (string, bool) {
	name = filepath.ToSlash(name)
	name = strings.TrimPrefix(name, "./")
	name = strings.TrimPrefix(name, "udocker-offline/")
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." {
		return clean, true
	}
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if strings.HasPrefix(part, "._") {
			return "", false // macOS AppleDouble metadata is not part of the bundle
		}
	}
	return clean, true
}

func validUdockerBundle(dir string) bool {
	// Extraction roots must be traversable by the dedicated non-root udocker
	// account. Older agents created this directory with MkdirTemp's 0700 mode;
	// treat that layout as stale even when all payload files are present.
	if st, err := os.Stat(dir); err != nil || !st.IsDir() || st.Mode().Perm()&0o005 != 0o005 {
		return false
	}
	// The bundle ships no PRoot: provisioning always installs gpuworks'
	// patched static-nofd PRoot under both names udocker looks for, so the
	// stock englib binaries were dead weight (about 6 MB per agent).
	for _, name := range []string{
		filepath.Join("udocker", "udocker"),
		filepath.Join("englib", "lib", "VERSION"),
	} {
		if st, err := os.Stat(filepath.Join(dir, name)); err != nil || st.IsDir() {
			return false
		}
	}
	// Do not accept archives previously unpacked on macOS with AppleDouble
	// metadata files. They are not Python modules and can confuse tooling.
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Name() != "." && strings.HasPrefix(entry.Name(), "._") {
			return fmt.Errorf("appledouble metadata %s", path)
		}
		return nil
	}); err != nil {
		return false
	}
	return true
}

// Verify checks the assets embedded for this process. Each agent binary
// carries one architecture, so the check follows runtime.GOARCH. Targets
// without those assets (darwin, and any arch we do not ship) pass.
func Verify() error {
	if goruntime.GOOS != "linux" {
		return nil
	}
	goarch := goruntime.GOARCH
	name, ok := prootAssets[goarch]
	if !ok {
		return nil
	}
	if _, err := files.ReadFile("bin/manifest.sha256"); err != nil {
		return err
	}
	if _, err := files.ReadFile(name); err != nil {
		return fmt.Errorf("missing embedded proot for %s: %w", goarch, err)
	}
	asset := udockerBundleAssets[goarch]
	if _, err := files.ReadFile(asset); err != nil {
		return fmt.Errorf("missing embedded udocker bundle %s for %s: %w", UdockerBundleVersion, goarch, err)
	}
	ib := ibWriteBWAssets[goarch]
	if _, err := perftestFiles.ReadFile(ib); err != nil {
		return fmt.Errorf("missing embedded ib_write_bw for %s: %w", goarch, err)
	}
	return nil
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
