//go:build linux || darwin

package terminal

import (
	"bytes"
	"io"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/ptyio"
	"gpuworks.ai/gpuworks/internal/proto"
)

// go test -run '^TestPumpInputAppliesLatestResize$' ./internal/agent/terminal
func TestPumpInputAppliesLatestResize(t *testing.T) {
	p, err := openPTY()
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()

	var frames bytes.Buffer
	sizes := []struct{ rows, cols uint16 }{{45, 150}, {32, 100}}
	for _, size := range sizes {
		if err := proto.WriteTermFrame(&frames, proto.TermResize, proto.EncodeResize(size.rows, size.cols)); err != nil {
			t.Fatal(err)
		}
	}
	if err := pumpInput(&frames, &Session{pty: p}); err != io.EOF {
		t.Fatalf("pumpInput returned %v, want EOF", err)
	}

	rows, cols, err := ptyio.Size(p.slave)
	if err != nil {
		t.Fatal(err)
	}
	want := sizes[len(sizes)-1]
	if rows != want.rows || cols != want.cols {
		t.Fatalf("terminal size = %d x %d, want %d x %d", rows, cols, want.rows, want.cols)
	}
}
