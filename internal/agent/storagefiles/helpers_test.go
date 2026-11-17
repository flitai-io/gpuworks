package storagefiles

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// testEnv is a storage root in a temporary directory with a Service bound to
// it. Its Do runs the call in place and records the op; a test may make it
// fail instead.
type testEnv struct {
	t       *testing.T
	root    string
	outside string // a directory beside the root, which nothing may reach

	mu      sync.Mutex
	r       Root
	rootErr error
	doErr   error
	ops     []string
	avail   int64
	availFn func(string) (int64, error)

	svc *Service
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	base := t.TempDir()
	e := &testEnv{t: t, root: filepath.Join(base, "storage"), outside: filepath.Join(base, "outside"), avail: 1 << 40}
	for _, d := range []string{e.root, e.outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(e.outside, "secret.txt"), "secret")
	e.r = Root{Dir: e.root, Ready: true, Writable: true}
	e.svc = New(Options{
		Root: func(id uint) (Root, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			if id != 1 {
				return Root{}, errors.New("storage is not configured on this node")
			}
			return e.r, e.rootErr
		},
		Do: func(id uint, op string, timeout time.Duration, fn func() error) error {
			e.mu.Lock()
			e.ops = append(e.ops, op)
			err := e.doErr
			e.mu.Unlock()
			if timeout <= 0 {
				t.Errorf("op %s ran without a time limit", op)
			}
			if err != nil {
				return err
			}
			return fn()
		},
		Available: func(p string) (int64, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.availFn != nil {
				return e.availFn(p)
			}
			return e.avail, nil
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return e
}

func (e *testEnv) path(rel string) string { return filepath.Join(e.root, filepath.FromSlash(rel)) }

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// codeOf is the storage-files code an error carries, as the server reads it
// back from the error text.
func codeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var fe *proto.StorageFilesError
	if !errors.As(err, &fe) {
		t.Fatalf("error %v (%T) carries no storage-files code", err, err)
	}
	if fe.RPCCode() != "STORAGE_FILES_"+strings.ToUpper(fe.Code) || fe.RPCParams()["detail"] != fe.Detail {
		t.Fatalf("code %q does not survive the wire: %q %v", fe.Code, fe.RPCCode(), fe.RPCParams())
	}
	return fe.Code
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if got := codeOf(t, err); got != code {
		t.Fatalf("code = %q, want %q (err %v)", got, code, err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s never happened", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// trashEmpty reports whether the storage's trash holds nothing (the
// background removal finished).
func (e *testEnv) trashEmpty() bool {
	ents, err := os.ReadDir(e.path(".gpuworks/trash"))
	return err == nil && len(ents) == 0
}
