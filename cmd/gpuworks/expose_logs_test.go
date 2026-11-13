package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

type logMutationWriter struct {
	mutate func() error
}

func (w logMutationWriter) Write(p []byte) (int, error) {
	if err := w.mutate(); err != nil {
		return 0, err
	}
	return len(p), nil
}

func TestExposeLogsReportsStatErrorsButToleratesRotation(t *testing.T) {
	for _, rotate := range []bool{false, true} {
		name := "stat_error"
		if rotate {
			name = "rotation"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "expose.log")
			if err := os.WriteFile(path, []byte("log entry\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			writer := logMutationWriter{mutate: func() error {
				if err := os.Remove(path); err != nil {
					return err
				}
				if rotate {
					return nil
				}
				// A broken path after the initial read must report its error.
				// A self-referential symlink gives ELOOP without privileges or
				// filesystem-permission assumptions.
				return os.Symlink(path, path)
			}}
			err := tailFile(context.Background(), path, 200, false, writer)
			if rotate {
				if err != nil {
					t.Fatalf("rotation should remain retryable: %v", err)
				}
			} else if !errors.Is(err, syscall.ELOOP) {
				t.Fatalf("log path stat error = %v, want ELOOP", err)
			}
		})
	}
}
