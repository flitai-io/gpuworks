// Command zstdpack writes a standalone or raw-dictionary zstd frame when the CLI is unavailable.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

func main() {
	out := flag.String("o", "", "output file")
	base := flag.String("base", "", "raw reference agent for a delta frame")
	flag.Parse()
	if *out == "" || flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: zstdpack -o OUT IN")
		os.Exit(2)
	}
	if err := pack(flag.Arg(0), *out, *base); err != nil {
		fmt.Fprintln(os.Stderr, "zstdpack:", err)
		os.Exit(1)
	}
}

// pack writes in compressed to out through a temporary file, so an
// interrupted build never leaves a truncated archive for go:embed to pick up.
// The whole input is encoded at once, which records its size in the frame
// header for the server to announce as the download's length.
func pack(in, out, base string) error {
	raw, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	// The best level with a window spanning a whole agent binary: built once
	// per release, read on every node download.
	opts := []zstd.EOption{zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithWindowSize(zstd.MaxWindowSize)}
	if base != "" {
		dict, err := os.ReadFile(base)
		if err != nil {
			return err
		}
		opts = append(opts, zstd.WithEncoderDictRaw(0, dict))
	}
	enc, err := zstd.NewWriter(nil, opts...)
	if err != nil {
		return err
	}
	compressed := enc.EncodeAll(raw, nil)
	if err := enc.Close(); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(out), ".zstdpack-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(compressed); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), out)
}
