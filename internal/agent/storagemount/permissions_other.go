//go:build !linux && !darwin

package storagemount

import "log/slog"

func PathPermissions(string) ([]slog.Attr, error) {
	return nil, ErrUnsupported
}
