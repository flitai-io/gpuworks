// Package catalogasset validates and extracts the small runtime assets carried
// by an executable inference catalog. It deliberately accepts data/config
// files only; framework binaries and model payloads belong in images or model
// repositories.
package catalogasset

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	MaxFileBytes          int64 = 1 << 20
	MaxModelExpandedBytes int64 = 4 << 20
	MaxPackageBytes       int64 = 16 << 20
)

var allowedExtensions = []string{
	".py", ".sh", ".patch", ".diff", ".json", ".yaml", ".yml", ".toml", ".txt", ".html", ".md",
}

func AllowedFile(name string) bool {
	lower := strings.ToLower(name)
	for _, extension := range allowedExtensions {
		if strings.HasSuffix(lower, extension) {
			return true
		}
	}
	return false
}

// ValidateTarGzip validates an asset bundle without writing it. It returns the
// expanded regular-file byte count for the owning model's aggregate limit.
func ValidateTarGzip(reader io.Reader) (int64, error) {
	gz, err := gzip.NewReader(reader)
	if err != nil {
		return 0, fmt.Errorf("open gzip asset: %w", err)
	}
	gz.Multistream(false)
	defer gz.Close()
	return walkTar(tar.NewReader(gz), nil)
}

// ExtractTarGzip creates destination atomically. The destination must not
// exist; callers can use a SHA-addressed path and treat an existing directory
// as a cache hit after separately verifying its marker.
func ExtractTarGzip(reader io.Reader, destination string) error {
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("asset destination already exists: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect asset destination: %w", err)
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create asset destination parent: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".catalog-asset-*")
	if err != nil {
		return fmt.Errorf("create asset staging directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()

	gz, err := gzip.NewReader(reader)
	if err != nil {
		return fmt.Errorf("open gzip asset: %w", err)
	}
	gz.Multistream(false)
	_, walkErr := walkTar(tar.NewReader(gz), func(header *tar.Header, contents io.Reader) error {
		target := filepath.Join(staging, filepath.FromSlash(header.Name))
		if header.Typeflag == tar.TypeDir {
			return os.MkdirAll(target, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(file, contents)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	closeErr := gz.Close()
	if walkErr != nil {
		return walkErr
	}
	if closeErr != nil {
		return fmt.Errorf("close gzip asset: %w", closeErr)
	}
	if err := os.Rename(staging, destination); err != nil {
		return fmt.Errorf("publish asset directory: %w", err)
	}
	published = true
	return nil
}

func walkTar(reader *tar.Reader, consume func(*tar.Header, io.Reader) error) (int64, error) {
	seen := map[string]struct{}{}
	var expanded int64
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return expanded, nil
		}
		if err != nil {
			return 0, fmt.Errorf("read tar asset: %w", err)
		}
		rawName := strings.ReplaceAll(header.Name, "\\", "/")
		expectedName := rawName
		if header.Typeflag == tar.TypeDir {
			expectedName = strings.TrimSuffix(expectedName, "/")
		}
		name := path.Clean(rawName)
		if name == "." || name != expectedName || !fs.ValidPath(name) || path.IsAbs(rawName) || strings.HasPrefix(name, "../") {
			return 0, fmt.Errorf("unsafe tar asset path %q", header.Name)
		}
		if _, duplicate := seen[name]; duplicate {
			return 0, fmt.Errorf("duplicate tar asset path %q", name)
		}
		seen[name] = struct{}{}
		switch header.Typeflag {
		case tar.TypeDir:
			if consume != nil {
				if err := consume(header, io.LimitReader(reader, 0)); err != nil {
					return 0, fmt.Errorf("create tar asset directory %s: %w", name, err)
				}
			}
		case tar.TypeReg:
			if !AllowedFile(name) {
				return 0, fmt.Errorf("tar asset %q has a forbidden file type", name)
			}
			if header.Size < 0 || header.Size > MaxFileBytes {
				return 0, fmt.Errorf("tar asset %q exceeds %d bytes", name, MaxFileBytes)
			}
			expanded += header.Size
			if expanded > MaxModelExpandedBytes {
				return 0, fmt.Errorf("tar asset expands beyond %d bytes", MaxModelExpandedBytes)
			}
			if consume != nil {
				if err := consume(header, io.LimitReader(reader, header.Size)); err != nil {
					return 0, fmt.Errorf("write tar asset %s: %w", name, err)
				}
			}
		default:
			return 0, fmt.Errorf("tar asset %q uses forbidden entry type %d", name, header.Typeflag)
		}
	}
}
