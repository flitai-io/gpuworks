package storagemount

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestClassifyMountError(t *testing.T) {
	cases := []struct{ out, want string }{
		{"mount: /mnt/x: bad option; for several filesystems (e.g. nfs, cifs) you might need a /sbin/mount.<type> helper program.", ReasonMissingClient},
		{"mount: /mnt/x: wrong fs type, bad option, bad superblock on host:/p, missing codepage or helper program, or other error.", ReasonMissingClient},
		{"mount: /mnt/x: unknown filesystem type 'nfs'.", ReasonMissingClient},
		{"/bin/sh: 1: mount.nfs: not found", ReasonMissingClient},
		{"mount.nfs: access denied by server while mounting host:/export", ReasonPermission},
		{"mount.nfs: Operation not permitted", ReasonPermission},
		{"mount: only root can do that", ReasonNotRoot},
		{"mount: /mnt/x: must be superuser to use mount.", ReasonNotRoot},
		{"mount.nfs: Connection timed out", ReasonUnreachable},
		{"mount.nfs: No route to host", ReasonUnreachable},
		{"mount.nfs: Connection refused", ReasonUnreachable},
		{"mount.nfs: Failed to resolve server nfs.example: Name or service not known", ReasonUnreachable},
		{"mount.nfs: mounting host:/nope failed, reason given by server: No such file or directory", ReasonPathMissing},
		{"mount.nfs: mount point /mnt/x does not exist", ReasonPathMissing},
		{"mount: something nobody has seen before", ReasonOther},
		{"", ReasonOther},
	}
	for _, c := range cases {
		if got := ClassifyMountError(c.out); got != c.want {
			t.Errorf("ClassifyMountError(%q) = %q, want %q", c.out, got, c.want)
		}
		if got := ClassifyMountError(strings.ToUpper(c.out)); got != c.want {
			t.Errorf("classification must ignore case: %q -> %q, want %q", c.out, got, c.want)
		}
	}
}

func TestIsNetworkFS(t *testing.T) {
	for _, magic := range []uint64{0x6969, 0xFF534D42, 0xFE534D42, 0x65735546, 0x00C36400, 0x0BD00BD0} {
		if !IsNetworkFS(magic) {
			t.Errorf("0x%X must be a network filesystem", magic)
		}
	}
	for _, magic := range []uint64{0xEF53, 0x58465342, 0x9123683E, 0x01021994, 0} { // ext4, xfs, btrfs, tmpfs
		if IsNetworkFS(magic) {
			t.Errorf("0x%X must not be a network filesystem", magic)
		}
	}
}

func TestMountFailureTruncates(t *testing.T) {
	long := strings.Repeat("é", 500)
	_, msg := mountFailure(mountResult{out: []byte(long), err: exitErr{32}}, 0)
	body := strings.TrimPrefix(msg, "mount failed: ")
	if len(body) > 403 || !strings.HasSuffix(body, "...") {
		t.Errorf("output must be cut to 400 bytes plus ellipsis, got %d bytes", len(body))
	}
	if !strings.HasPrefix(body, "é") || strings.ContainsRune(body, '\uFFFD') {
		t.Error("cut must not split a multi-byte rune")
	}
	reason, msg := mountFailure(mountResult{err: exitErr{127}}, 0)
	if reason != ReasonOther || !strings.Contains(msg, "exit status 127") {
		t.Errorf("empty output falls back to the error text, got %q %q", reason, msg)
	}
}

// A mount program that does not exist is known by exec's error, not by its
// wording.
func TestMountFailureKnowsAMissingProgramByType(t *testing.T) {
	missing := &exec.Error{Name: "mount", Err: exec.ErrNotFound}
	if reason, message := mountFailure(mountResult{err: missing}, time.Second); reason != ReasonMissingClient || message != `mount failed: exec: "mount": executable file not found in $PATH` {
		t.Fatalf("missing program = %q %q", reason, message)
	}
	exit := exec.Command("sh", "-c", "exit 32").Run()
	if reason, _ := mountFailure(mountResult{out: []byte("mount.nfs: Connection refused"), err: exit}, time.Second); reason != ReasonUnreachable {
		t.Fatalf("exit with output = %q", reason)
	}
	if reason, _ := mountFailure(mountResult{err: errors.New(`exec: "mount": executable file not found in $PATH`)}, time.Second); reason != ReasonOther {
		t.Fatalf("text alone classified: %q", reason)
	}
}
