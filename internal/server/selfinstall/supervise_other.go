//go:build !linux && !darwin

package selfinstall

import (
	"context"
	"errors"
)

// errSuperviseUnsupported is a platform without the process controls the
// supervisor relies on.
var errSuperviseUnsupported = errors.New("serve --supervise is only supported on Linux and macOS")

// Supervise is not available on this platform.
func Supervise(context.Context, SuperviseOptions) error { return errSuperviseUnsupported }
