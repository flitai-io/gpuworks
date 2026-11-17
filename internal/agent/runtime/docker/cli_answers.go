package docker

import (
	"context"
	"errors"
	"os/exec"
	"strings"
)

// The docker CLI reports a missing image, container or object only in its
// combined output (the exit status is 1 for every failure). These helpers are
// the single place that reads that output; callers turn the answer into a
// return value or runtime.ErrImageNotFound right where the CLI ran.

// cliNoSuchImage reports whether docker CLI output says the image is absent
// ("Error: No such image: org/model:v1" / "Error response from daemon: No
// such image: ...").
func cliNoSuchImage(out string) bool {
	return strings.Contains(strings.ToLower(out), "no such image:")
}

// cliNoSuchObject reports whether docker CLI output says `docker inspect`
// found nothing by that name ("Error: No such object: abc").
func cliNoSuchObject(out string) bool {
	return strings.Contains(strings.ToLower(out), "no such object")
}

// cliAnsweredNoSuchContainer reports whether a failed docker command was the
// CLI itself answering that the container does not exist. A docker binary
// that could not run (exec error) is never an answer, whatever err says.
func cliAnsweredNoSuchContainer(err error, out string) bool {
	var exitErr *exec.ExitError
	if err == nil || !errors.As(err, &exitErr) {
		return false
	}
	return cliNoSuchObject(out) || strings.Contains(strings.ToLower(out), "no such container")
}

// cliPermissionDenied reports whether a failed docker command was refused
// for lack of permission on the daemon socket ("Got permission denied while
// trying to connect to the Docker daemon socket ..."). A docker binary that
// could not run is never this answer.
func cliPermissionDenied(err error, out string) bool {
	var exitErr *exec.ExitError
	if err == nil || !errors.As(err, &exitErr) {
		return false
	}
	text := strings.ToLower(out)
	return strings.Contains(text, "permission denied") || strings.Contains(text, "operation not permitted")
}

// daemonNotReady reports whether a failed `docker info` is a daemon that has
// not come up or is too busy to answer, rather than one this agent may not
// use. dockerd starting at boot leaves the socket refusing connections;
// dockerd pulling a large image answers slower than the timeout.
func daemonNotReady(ctxErr error, stderr string) bool {
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		return true
	}
	text := strings.ToLower(stderr)
	if strings.Contains(text, "permission denied") {
		return false
	}
	return strings.Contains(text, "cannot connect to the docker daemon") || strings.Contains(text, "connection refused") || strings.Contains(text, "is the docker daemon running")
}

// socketPermissionDenied reports whether a failed `docker info` is the CLI
// refused the daemon's socket: the agent's account is not in its group.
// Docker words it "permission denied while trying to connect to the Docker
// daemon socket at unix://..." (the docker API, from 25.0 on); the dial
// error under it ends "dial unix ...: connect: permission denied".
func socketPermissionDenied(stderr string) bool {
	text := strings.ToLower(stderr)
	if !strings.Contains(text, "permission denied") {
		return false
	}
	return strings.Contains(text, "while trying to connect to the docker") || strings.Contains(text, "dial unix")
}

func firstLine(stderr string, err error) string {
	if line, _, _ := strings.Cut(strings.TrimSpace(stderr), "\n"); line != "" {
		return line
	}
	return err.Error()
}
