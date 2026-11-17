//go:build linux || darwin

package storagemount

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// PathPermissions describes the directory and asks the kernel about access;
// mode bits alone cannot account for ACLs or NFS identity mapping.
func PathPermissions(path string) ([]slog.Attr, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	stat := info.Sys().(*syscall.Stat_t)
	attrs := []slog.Attr{
		slog.String("mode", info.Mode().String()),
		slog.String("mode_octal", fmt.Sprintf("%#o", stat.Mode&0o7777)),
		slog.Uint64("uid", uint64(stat.Uid)), slog.Uint64("gid", uint64(stat.Gid)),
	}
	for _, check := range []struct {
		name string
		mode uint32
	}{{"readable", unix.R_OK}, {"writable", unix.W_OK}, {"searchable", unix.X_OK}} {
		// access(2) uses real IDs. Do not report them as the agent's effective
		// access if it is running with a different effective identity.
		if os.Getuid() != os.Geteuid() || os.Getgid() != os.Getegid() {
			attrs = append(attrs, slog.String(check.name, "unknown"), slog.String(check.name+"_error", "real and effective identities differ"))
			continue
		}
		err := unix.Access(path, check.mode)
		switch {
		case err == nil:
			attrs = append(attrs, slog.Bool(check.name, true))
		case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM), errors.Is(err, unix.EROFS):
			attrs = append(attrs, slog.Bool(check.name, false))
		default:
			attrs = append(attrs, slog.String(check.name, "unknown"), slog.Any(check.name+"_error", err))
		}
	}
	return attrs, nil
}
