package app

import (
	"context"
	"errors"

	"gpuworks.ai/gpuworks/internal/proto"
)

// imageOperationTimeout bounds one image operation end to end — a registry
// pull, or a download-and-load of an archive. A runtime CLI that never returns
// must not hold the node's single image slot forever: the transfer fails
// visibly instead of blocking every later one behind it.
// It is a backstop, not the first line of defence: an operator can cancel a
// transfer, and the server abandons one that stops reporting. This bounds the
// node's image slot even when nobody is watching.
const imageOperationTimeout = proto.ImageOperationTimeout

// imageOperationError turns the two sentinel failures into something an
// operator can act on. Everything else keeps the runtime's own words.
func imageOperationError(ctx context.Context, err error) string {
	switch {
	case ctx != nil && errors.Is(ctx.Err(), context.Canceled):
		return "the transfer was cancelled before it finished"
	case ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "the transfer did not finish within " + imageOperationTimeout.String() + " and was stopped"
	default:
		return err.Error()
	}
}
