package hub

import (
	"sync"
	"time"
)

// A syncWorker is one session's coalescing worker for one kind of push: the
// network policy (network_sync.go), the full desired state (node_resync.go),
// the catalog's image refs (image_refs.go). A caller only asks; the worker
// pushes the latest state after the session's first replay, one push at a
// time, so the caller never waits for the agent, a stalled peer holds up only
// its own node, and any number of requests while a push is pending or under
// way come to one more push. It is session work (the pushes read the store)
// and ends with the session.
type syncWorker struct {
	once   sync.Once
	notify chan struct{}
}

// syncRetry is how long a push that failed waits before it is tried again,
// unless another request comes first.
const syncRetry = time.Second

// request asks w for a push, starting it on the session's first request. A
// request for a session that has closed is dropped: the node's next session
// starts with a full replay.
func (s *Session) request(w *syncWorker, push func() error, what string) {
	select {
	case <-s.closed:
		return
	default:
	}
	w.once.Do(func() {
		w.notify = make(chan struct{}, 1)
		s.hub.trackSessionWork(func() { s.runSyncWorker(w, push, what) })
	})
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

// runSyncWorker pushes each time it is asked until the session closes. A
// failed push is tried again after syncRetry, or at once on a new request: a
// transient store failure must not lose the only request. What failed is
// logged by node only; a push may carry credentials.
func (s *Session) runSyncWorker(w *syncWorker, push func() error, what string) {
	for {
		select {
		case <-s.closed:
			return
		case <-w.notify:
		}
		// The session's own first replay goes first; a request made before
		// it landed still pushes after it, since its change may have committed
		// after that replay's read.
		select {
		case <-s.closed:
			return
		case <-s.initialPushDone:
		}
		for {
			// Requests received before this push's read are answered by it.
			// One that comes during the read or the write leaves another
			// notification for the next iteration.
			select {
			case <-w.notify:
			default:
			}
			if err := push(); err == nil {
				break
			}
			select {
			case <-s.closed:
				return
			default:
			}
			s.hub.log.Warn(what+" failed; retrying", "node", s.NodeUUID)
			timer := time.NewTimer(syncRetry)
			select {
			case <-s.closed:
				timer.Stop()
				return
			case <-w.notify:
				timer.Stop()
			case <-timer.C:
			}
		}
	}
}
