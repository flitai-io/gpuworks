package native

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestStreamFollowsBlobAfterRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "download")
	const content = "verified layer"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	var published string
	d := Downloader{Stream: func(_ context.Context, _ Descriptor, r io.Reader) (func(bool), error) {
		data, err := io.ReadAll(r)
		return func(verified bool) {
			if verified {
				published = string(data)
			}
		}, err
	}}
	size := int64(len(content))
	growth := newGrowingFile(size, size, 1)
	stream := d.startStream(t.Context(), Descriptor{}, path, growth)
	growth.advance(0, size)
	growth.finish(nil)
	defer stream.stop()
	if err := os.Rename(path, filepath.Join(dir, "verified")); err != nil {
		t.Fatal(err)
	}
	stream.finish()
	if published != content {
		t.Fatalf("stream published %q, want %q", published, content)
	}
}
