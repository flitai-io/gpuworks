//go:build linux

package store

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"gpuworks.ai/gpuworks/internal/securefile"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestOwnerDatabaseRetainsSafeInheritedACLWithoutGrantingGroupAccess(t *testing.T) {
	dir := t.TempDir()
	// A default named-user ACL is inherited with mask000 by mode0600
	// files. It grants no extra access under the owner-only policy.
	entries := []struct {
		tag, perm uint16
		id        uint32
	}{
		{1, 7, ^uint32(0)}, {2, 6, 65534}, {4, 7, ^uint32(0)}, {16, 6, ^uint32(0)}, {32, 0, ^uint32(0)},
	}
	acl := make([]byte, 4+8*len(entries))
	binary.LittleEndian.PutUint32(acl, 2)
	for i, entry := range entries {
		offset := 4 + 8*i
		binary.LittleEndian.PutUint16(acl[offset:], entry.tag)
		binary.LittleEndian.PutUint16(acl[offset+2:], entry.perm)
		binary.LittleEndian.PutUint32(acl[offset+4:], entry.id)
	}
	if err := unix.Setxattr(dir, "system.posix_acl_default", acl, 0); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "management.db")
	s, err := Open(path, bus.New())
	if err != nil {
		t.Fatalf("safe owner-only inherited ACL must remain usable: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != securefile.OwnerMode {
			t.Fatalf("%s mode=%#o, want0600", suffix, info.Mode().Perm())
		}
	}
	assertSQLiteStoreLocks(t, path, "owner-only inherited ACL")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	group, err := securefile.TrustedGroup(os.Getegid())
	if err != nil {
		t.Fatal(err)
	}
	if other, err := Open(path, bus.New(), group); err == nil {
		_ = other.Close()
		t.Fatal("inherited named-user ACL must not be reactivated by granting group access")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != securefile.OwnerMode {
		t.Fatalf("rejected group policy changed owner-only protection: %v %v", info, err)
	}
}
