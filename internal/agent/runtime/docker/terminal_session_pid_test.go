package docker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func waitTerminalPID(ctx context.Context, read func() ([]byte, error)) (string, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var last []byte
	for {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("terminal PID not ready, last read %q: %w", last, err)
		}
		var err error
		last, err = read()
		if err == nil {
			value := strings.TrimSpace(string(last))
			if pid, parseErr := strconv.Atoi(value); parseErr == nil && pid > 1 {
				return value, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("terminal PID not ready, last read %q: %w", last, ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestWaitTerminalPIDWaitsForTheWriter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Shell redirection creates/truncates the file before printf writes $$.
		observed := []string{"", " \n", "0", "1", "incomplete", "42\n"}
		reads := 0
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		pid, err := waitTerminalPID(ctx, func() ([]byte, error) {
			value := observed[min(reads, len(observed)-1)]
			reads++
			return []byte(value), nil
		})
		if err != nil || pid != "42" || reads != len(observed) {
			t.Fatalf("PID=%q err=%v reads=%d; file existence alone does not mean its writer finished", pid, err, reads)
		}
	})
}

func TestWaitTerminalPIDTimesOutWithEmptyFile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		pid, err := waitTerminalPID(ctx, func() ([]byte, error) { return nil, nil })
		if pid != "" || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("empty PID file: pid=%q err=%v", pid, err)
		}
	})
}

func TestWaitTerminalPIDRetriesMissingFile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reads := 0
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		pid, err := waitTerminalPID(ctx, func() ([]byte, error) {
			reads++
			if reads == 1 {
				return nil, errors.New("file not created yet")
			}
			return []byte("42"), nil
		})
		if err != nil || pid != "42" || reads != 2 {
			t.Fatalf("PID=%q err=%v reads=%d", pid, err, reads)
		}
	})
}
