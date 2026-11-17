package nerdctl

import (
	"errors"
	"os/exec"
	"strings"
)

// nerdctl reports a missing image or container only in its combined output
// (exit status 1 for every failure). These helpers are the single place that
// reads that output; callers turn the answer into a return value or
// runtime.ErrImageNotFound right where the CLI ran.

// cliAnswered reports whether err is nerdctl having run and exited non-zero.
// A binary that could not be executed fails with "executable file not found",
// which must never read as "the container is not there".
func cliAnswered(err error) bool {
	var exitErr *exec.ExitError
	return err != nil && errors.As(err, &exitErr)
}

// cliPermissionDenied reports whether nerdctl output says containerd refused
// the agent ("permission denied" on the containerd socket, "operation not
// permitted" for a rootless namespace it may not enter).
func cliPermissionDenied(out string) bool {
	text := strings.ToLower(out)
	return strings.Contains(text, "permission denied") || strings.Contains(text, "operation not permitted")
}

// cliNoSuchImage reports whether nerdctl output says the image is absent
// ("no such image: org/model:v1", or "no such image or container: ..." from
// the generic inspect).
func cliNoSuchImage(out string) bool {
	text := strings.ToLower(out)
	return strings.Contains(text, "no such image:") || strings.Contains(text, "no such image or container:")
}

// cliNoSuchContainer reports whether nerdctl output says the container is
// absent: "no such container: abc" from rm, "no such object" / "... not
// found" from inspect.
func cliNoSuchContainer(out string) bool {
	text := strings.ToLower(out)
	return strings.Contains(text, "no such container") || strings.Contains(text, "no such object") || strings.Contains(text, "not found")
}
