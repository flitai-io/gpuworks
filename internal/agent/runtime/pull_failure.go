package runtime

import (
	"errors"
	"strings"
	"syscall"
)

// Pull failures the image puller acts on. A driver marks its pull error with
// one of these where the runtime ran; the puller uses errors.Is and never
// reads the error text.
var (
	// ErrNoSpace: the node's image store ran out of disk. Every source
	// unpacks onto the same filesystem, so no other registry can help.
	ErrNoSpace = errors.New("no space left for the image on this node")
	// ErrRegistryRefused: the registry answered the pull — the tag or
	// repository is not there, or the login was refused. That says nothing
	// about how well the node reaches the registry.
	ErrRegistryRefused = errors.New("registry refused the pull")
)

// MarkPullFailure returns err classified as kind (ErrNoSpace or
// ErrRegistryRefused) without changing its text: errors.Is matches both kind
// and everything err wraps. A nil kind or err returns err unchanged.
func MarkPullFailure(err, kind error) error {
	if err == nil || kind == nil || errors.Is(err, kind) {
		return err
	}
	return &markedPullFailure{err: err, kind: kind}
}

type markedPullFailure struct{ err, kind error }

func (e *markedPullFailure) Error() string   { return e.err.Error() }
func (e *markedPullFailure) Unwrap() []error { return []error{e.err, e.kind} }

// DiskFull reports whether err is the operating system refusing a write
// because the filesystem or the user's quota is full.
func DiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

// PullOutputFailure classifies the output of a failed `docker pull` or
// `nerdctl pull`. The container CLIs report registry and storage failures only
// as text, so the CLI drivers call this on the output they captured, right
// where the CLI ran, and mark the error with the result. It returns nil when
// the output names neither.
func PullOutputFailure(output string) error {
	text := strings.ToLower(output)
	// containerd/dockerd: "write /var/lib/...: no space left on device".
	if strings.Contains(text, "no space left on device") || strings.Contains(text, "disk quota exceeded") {
		return ErrNoSpace
	}
	// "permission denied" is the local socket or filesystem refusing this
	// agent, not the registry answering.
	text = strings.ReplaceAll(text, "permission denied", "")
	// Registry answers as docker and containerd print them (distribution
	// error codes MANIFEST_UNKNOWN, NAME_UNKNOWN, UNAUTHORIZED, DENIED, and
	// Docker Hub's "repository does not exist or may require 'docker
	// login'"/"was deleted or has expired").
	for _, answer := range []string{
		"not found", "manifest unknown", "name unknown", "was deleted or has expired",
		"unauthorized", "authentication required", "denied", "forbidden",
	} {
		if strings.Contains(text, answer) {
			return ErrRegistryRefused
		}
	}
	return nil
}
