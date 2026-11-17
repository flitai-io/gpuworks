package native

import (
	"errors"
	"fmt"
)

// unpackReserve is room left after an unpack for what a launch still writes:
// the driver libraries copied into the rootfs and the container's log.
const unpackReserve = 2 << 30

// checkUnpackSpace refuses an unpack the disk cannot hold. An unpacked image
// is at least as large as its layers as stored (compressed, or already
// inflated), so less free space than that plus unpackReserve can only end in
// a disk that fills up mid-unpack - and a full disk then breaks the launch in
// ways that do not name it (a driver library that cannot be written, a log
// that stays empty). Unknown free space is not a reason to refuse.
// errUnpackSpace marks checkUnpackSpace's refusal so callers can tell a full
// disk from other unpack failures.
var errUnpackSpace = errors.New("not enough disk space to unpack the image")

func (e *Engine) checkUnpackSpace(dir string, layers []Descriptor) error {
	free := e.freeSpace(dir)
	if free < 0 {
		return nil
	}
	need := int64(unpackReserve)
	for _, layer := range layers {
		need += layer.Size
	}
	if free < need {
		return fmt.Errorf("%w under %s: %s free, at least %s needed (its layers plus %s for the launch); free up space on the node",
			errUnpackSpace, dir, humanBytes(free), humanBytes(need), humanBytes(unpackReserve))
	}
	return nil
}

func (e *Engine) freeSpace(path string) int64 {
	if e.FreeSpace != nil {
		return e.FreeSpace(path)
	}
	return availableAt(path)
}
