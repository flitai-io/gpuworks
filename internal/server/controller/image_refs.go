package controller

import (
	"context"
	"time"
)

// A catalog change is pushed to the agents as the whole list of enabled image
// refs, and an import publishes two events per image (the image and its
// series): pushed once per event, importing 40 images into a 50-node cluster
// came to 4000 pushes, each re-reading every image and node, all on the
// goroutine that handles the controller's events, where one stalled agent's
// 30 s write held up every other event. Each change now only asks for a push;
// a worker of its own waits for the burst to settle, reads the catalog once
// and hands the list to every node's session (Hub.RequestImageRefs), which
// sends the latest one on its own: a node that stops reading holds up only its
// own write. The full replay a session starts with carries the refs too, so a
// push a node misses reaches it on reconnect.

// imageRefsSettle is how long the push waits after the first change it was
// asked for, gathering the rest of an import into the same push.
const imageRefsSettle = 250 * time.Millisecond

// requestImageRefsPush asks for the catalog's refs to be pushed to every node;
// requests before the push reads the catalog come to that one push.
func (c *Controller) requestImageRefsPush() {
	select {
	case c.imageRefsNotify <- struct{}{}:
	default:
	}
}

// imageRefsWorker pushes the refs when asked, a settled burst at a time, until
// ctx ends. It never waits for an agent: the writes are the sessions'.
func (c *Controller) imageRefsWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.imageRefsNotify:
		}
		timer := time.NewTimer(imageRefsSettle)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		// Requests up to here are answered by this push, which reads the
		// catalog after them; one that comes later leaves a notification for
		// the next push.
		select {
		case <-c.imageRefsNotify:
		default:
		}
		c.pushImageRefs()
	}
}

// pushImageRefs reads the enabled catalog's refs and hands them to every node
// to send; it does not wait for the writes.
func (c *Controller) pushImageRefs() {
	images, err := c.store.ListEnabledImages()
	if err != nil {
		return
	}
	refs := make([]string, 0, len(images))
	for _, image := range images {
		refs = append(refs, image.ImageRef)
	}
	nodes, err := c.store.NodeUUIDsByID()
	if err != nil {
		return
	}
	for _, uuid := range nodes {
		c.hub.RequestImageRefs(uuid, refs)
	}
}
