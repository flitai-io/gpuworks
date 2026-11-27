// Package agentbin prepares the agents embedded in a server build and serves
// complete binaries, with optional operator overrides in the data directory.
package agentbin

import (
	"bytes"
	"embed"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
)

//go:embed all:dl
var embedded embed.FS

// Files is the agent source for one server instance.
type Files struct{ tree fs.FS }

var bundled = Files{embedded}

// compressedSuffix marks an embedded agent stored zstd-compressed.
const compressedSuffix = ".zst"

// Handler serves agent binaries at /dl/<name>. It prefers a real file in
// overrideDir (dev/manual placement); otherwise it serves the embedded copy,
// expanding a compressed one as it streams.
func (f Files) Handler(overrideDir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/dl/")
		if !validName(name) {
			http.NotFound(w, r)
			return
		}
		// Override dir wins (lets ops swap in a patched agent without rebuilding).
		if dir := f.overrideDir(overrideDir, name); dir != "" {
			if f, err := os.Open(filepath.Join(dir, name)); err == nil {
				defer f.Close()
				if st, err := f.Stat(); err == nil && !st.IsDir() {
					w.Header().Set("Content-Type", "application/octet-stream")
					http.ServeContent(w, r, name, st.ModTime(), f)
					return
				}
			}
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		if file, err := f.tree.Open("dl/" + name); err == nil {
			defer file.Close()
			if st, err := file.Stat(); err == nil && st.Mode().IsRegular() {
				w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
				_, _ = io.Copy(w, file)
			} else {
				http.NotFound(w, r)
			}
			return
		} else if errors.Is(err, errAgentUnavailable) {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		compressed, err := fs.ReadFile(f.tree, "dl/"+name+compressedSuffix)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		dec, err := zstd.NewReader(bytes.NewReader(compressed), zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
		if err != nil {
			http.Error(w, "agent binary unreadable", http.StatusInternalServerError)
			return
		}
		defer dec.Close()
		if size, ok := expandedSize(compressed); ok {
			w.Header().Set("Content-Length", strconv.FormatUint(size, 10))
		}
		_, _ = io.Copy(w, dec)
	})
}

// Read returns an agent binary by name, preferring overrideDir over embedded data.
func (f Files) Read(overrideDir, name string) ([]byte, error) {
	if !validName(name) {
		return nil, fs.ErrNotExist
	}
	if dir := f.overrideDir(overrideDir, name); dir != "" {
		path := filepath.Join(dir, name)
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			return os.ReadFile(path)
		}
	}
	if raw, err := fs.ReadFile(f.tree, "dl/"+name); err == nil {
		return raw, nil
	} else if errors.Is(err, errAgentUnavailable) {
		return nil, err
	}
	compressed, err := fs.ReadFile(f.tree, "dl/"+name+compressedSuffix)
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return dec.DecodeAll(compressed, nil)
}

// Has reports whether an embedded agent binary exists (a real build vs the
// placeholder-only tree).
func (f Files) Has(name string) bool {
	if !validName(name) {
		return false
	}
	for _, path := range []string{"dl/" + name, "dl/" + name + compressedSuffix} {
		if info, err := fs.Stat(f.tree, path); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

func validName(name string) bool {
	return name != "" && !strings.Contains(name, "..") && !strings.Contains(name, "/") && !strings.HasSuffix(name, compressedSuffix)
}

// expandedSize is the size a zstd frame records for its content, which the
// packer writes, so a download can state its length before it is expanded.
func expandedSize(compressed []byte) (uint64, bool) {
	var h zstd.Header
	if err := h.Decode(compressed); err != nil || !h.HasFCS {
		return 0, false
	}
	return h.FrameContentSize, true
}
