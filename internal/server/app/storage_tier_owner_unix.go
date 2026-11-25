//go:build unix

package app

import (
	"fmt"
	"os"
	"syscall"
)

// likeRecord gives the staged configuration the owner, group and mode of the
// record it replaces. The server reads the record when it starts, as the
// account its service runs under; a command run as another account (with
// sudo, say) would otherwise leave a record that account cannot read, and the
// member would not start again. An owner this process cannot give is an
// error, before the record is touched.
func likeRecord(stage *os.File, record os.FileInfo) error {
	if err := stage.Chmod(record.Mode().Perm()); err != nil {
		return err
	}
	want, ok := record.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	info, err := stage.Stat()
	if err != nil {
		return err
	}
	if have, ok := info.Sys().(*syscall.Stat_t); ok && have.Uid == want.Uid && have.Gid == want.Gid {
		return nil
	}
	if err := stage.Chown(int(want.Uid), int(want.Gid)); err != nil {
		return fmt.Errorf("record storage tier: the record belongs to uid %d gid %d and this account cannot write one that does; run the command as the account the server runs under: %w", want.Uid, want.Gid, err)
	}
	return nil
}
