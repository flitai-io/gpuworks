//go:build unix

package app

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The rewritten record keeps the owner, group and mode of the one it
// replaces: a command run as root on a member whose server runs under its own
// account must leave a record that account can still read at its next start.
func TestRecordStorageTierKeepsTheRecordsOwnerAndMode(t *testing.T) {
	dir := t.TempDir()
	enrollment := filepath.Join(dir, "cluster-enrollment")
	if err := os.MkdirAll(enrollment, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(enrollment, "config.json")
	if err := os.WriteFile(path, []byte(`{"cluster_id":"c","ca_fingerprint":"f","protocol":1,"listen_address":"a","public_url":"u"}`), 0640); err != nil {
		t.Fatal(err)
	}
	// As root the record is given to another account; otherwise to another
	// of this account's groups, the only owner change it may make.
	uid, gid := os.Getuid(), os.Getgid()
	if os.Geteuid() == 0 {
		uid, gid = 4242, 4343
	} else if groups, err := os.Getgroups(); err == nil {
		for _, group := range groups {
			if group != gid {
				gid = group
				break
			}
		}
	}
	if err := os.Chown(path, uid, gid); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordStorageTier(dir, "slow-storage@100ms"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	if int(st.Uid) != uid || int(st.Gid) != gid || info.Mode().Perm() != 0640 {
		t.Fatalf("record is uid %d gid %d mode %v, want uid %d gid %d mode 0640", st.Uid, st.Gid, info.Mode().Perm(), uid, gid)
	}
	if tier, err := RecordedStorageTier(dir); err != nil || tier != "slow-storage@100ms" {
		t.Fatalf("recorded %q, %v", tier, err)
	}
	entries, err := os.ReadDir(enrollment)
	if err != nil || len(entries) != 1 {
		t.Fatalf("the directory holds %d entries (%v), want the record alone", len(entries), err)
	}
}
