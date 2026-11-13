//go:build linux

package store

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"gpuworks.ai/gpuworks/internal/securefile"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func databaseNamedUserACL() []byte {
	entries := []struct {
		tag, perm uint16
		id        uint32
	}{{1, 7, ^uint32(0)}, {2, 4, 65534}, {4, 7, ^uint32(0)}, {16, 7, ^uint32(0)}, {32, 0, ^uint32(0)}}
	data := make([]byte, 4+8*len(entries))
	binary.LittleEndian.PutUint32(data, 2)
	for i, e := range entries {
		off := 4 + 8*i
		binary.LittleEndian.PutUint16(data[off:], e.tag)
		binary.LittleEndian.PutUint16(data[off+2:], e.perm)
		binary.LittleEndian.PutUint32(data[off+4:], e.id)
	}
	return data
}

func TestTrustedGroupDatabaseRejectsACLBeforeFileCreation(t *testing.T) {
	for _, attribute := range []string{"system.posix_acl_default", "system.posix_acl_access"} {
		t.Run(attribute, func(t *testing.T) {
			dir := t.TempDir()
			if err := unix.Setxattr(dir, attribute, databaseNamedUserACL(), 0); err != nil {
				t.Fatal(err)
			}
			policy, err := securefile.TrustedGroup(os.Getgid())
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "custom.db")
			s, err := Open(path, bus.New(), policy)
			if s != nil {
				s.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "ACL") {
				t.Errorf("unsafe native SQLite creation accepted: %v", err)
			}
			for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
				if _, statErr := os.Stat(path + suffix); !errors.Is(statErr, os.ErrNotExist) {
					t.Errorf("created database or sidecar before ACL refusal: %s %v", suffix, statErr)
				}
			}
			if _, err := unix.Getxattr(dir, attribute, nil); err != nil {
				t.Fatalf("parent ACL was changed: %v", err)
			}
		})
	}
}
