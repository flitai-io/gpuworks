package hub

import (
	"sync"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// sessionReportBatchMax bounds the progress reports one store transaction
// writes, so a node with many instances never holds the store's writer long.
const sessionReportBatchMax = 256

// reportJob is one ordered report: a progress-only instance report
// (store.InstanceProgressOnly), which the worker batches, or any other report
// as the function that applies it.
type reportJob struct {
	instance string // the instance the report is about, if any
	progress *proto.InstanceStatus
	run      func()
}

// reportQueue is a session's ordered worker queue. Reports apply in receipt
// order, with one exception that keeps a storm's backlog bounded: a
// progress-only instance report replaces a still pending one for the same
// instance in the same state and phase, in place, provided the pending one is
// not the first report of that state and phase. The first one is the
// transition: the store records it as a timeline event carrying its message
// (an agent sends a preparation note that way), so it is never replaced. A
// later one, with the session's earlier report already naming that state and
// phase, would only have written the same columns with older values and
// published the same progress notification, so applying only the newer one,
// at the older one's position, is what the store would have ended with; nothing
// else the session reported in between depends on it. A report of any other
// shape for that instance ends the run, so transitions are never merged away.
type reportQueue struct {
	mu         sync.Mutex
	cond       *sync.Cond
	jobs       []*reportJob
	byInstance map[string]*reportJob // the instance's pending replaceable progress, while it is its newest report
	last       map[string]string     // the state and phase of each instance's newest report this session
	closed     bool
	done       chan struct{}
}

func newReportQueue() *reportQueue {
	q := &reportQueue{byInstance: map[string]*reportJob{}, last: map[string]string{}, done: make(chan struct{})}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func statePhase(st *proto.InstanceStatus) string { return st.State + "\x00" + st.Phase }

// push queues job, coalescing as described on reportQueue. It waits while
// sessionReportDepth reports are pending, which only the session's own read
// loop ever does, and returns false once the queue is closed or abandon
// reports true. With progress coalesced, the depth bounds a node's distinct
// instances reporting at once plus its other reports, not a storm's length.
func (q *reportQueue) push(job *reportJob, abandon func() bool) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if job.progress != nil {
		if pending := q.byInstance[job.instance]; pending != nil &&
			pending.progress.State == job.progress.State && pending.progress.Phase == job.progress.Phase {
			pending.progress = job.progress
			return true
		}
	}
	for len(q.jobs) >= sessionReportDepth && !q.closed && !abandon() {
		q.cond.Wait()
	}
	if q.closed || abandon() {
		return false
	}
	q.jobs = append(q.jobs, job)
	if job.instance != "" {
		delete(q.byInstance, job.instance)
		if job.progress != nil {
			if q.last[job.instance] == statePhase(job.progress) {
				q.byInstance[job.instance] = job
			}
			q.last[job.instance] = statePhase(job.progress)
		} else {
			delete(q.last, job.instance) // its state and phase are the full path's to judge
		}
	}
	q.cond.Broadcast()
	return true
}

// take waits for pending reports and returns all of them, or nil once the
// queue is closed and empty.
func (q *reportQueue) take() []*reportJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.jobs) == 0 && !q.closed {
		q.cond.Wait()
	}
	jobs := q.jobs
	q.jobs = nil
	for _, job := range jobs {
		if job.progress != nil {
			delete(q.byInstance, job.instance)
		}
	}
	q.cond.Broadcast()
	return jobs
}

func (q *reportQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

// wake releases a push waiting on a full queue so it can see abandon.
func (q *reportQueue) wake() {
	q.mu.Lock()
	q.cond.Broadcast()
	q.mu.Unlock()
}

// runReports is the session's ordered worker: each run of consecutive
// progress reports is one store transaction, everything else applies alone.
func (s *Session) runReports(q *reportQueue) {
	defer close(q.done)
	for {
		jobs := q.take()
		if jobs == nil {
			return
		}
		var batch []proto.InstanceStatus
		for _, job := range jobs {
			if job.progress != nil {
				batch = append(batch, *job.progress)
				if len(batch) == sessionReportBatchMax {
					s.applyInstanceProgress(batch)
					batch = batch[:0]
				}
				continue
			}
			s.applyInstanceProgress(batch)
			batch = batch[:0]
			job.run()
		}
		s.applyInstanceProgress(batch)
	}
}

// applyInstanceProgress writes a batch of progress reports in one
// transaction. Those the store does not take as progress (the row moved on, or
// belongs to another node), and the whole batch when the transaction fails or
// the session is no longer current, go through the full per-report path, in
// order (which refuses the latter too).
func (s *Session) applyInstanceProgress(batch []proto.InstanceStatus) {
	if len(batch) == 0 {
		return
	}
	rest := batch
	if _, err := s.applyCurrent(func(nodeID uint) error {
		unapplied, err := s.hub.store.ApplyInstanceProgress(nodeID, batch)
		if err == nil {
			rest = unapplied
		}
		return err
	}); err != nil {
		s.hub.log.Debug("apply status.instance progress batch", "node", s.NodeUUID, "reports", len(batch), "err", err)
	}
	for _, st := range rest {
		s.applyInstanceReport(st)
	}
}

// submitInstanceStatus queues an instance report on the ordered worker.
func (s *Session) submitInstanceStatus(st proto.InstanceStatus) {
	job := &reportJob{instance: st.UUID, run: func() { s.applyInstanceReport(st) }}
	if store.InstanceProgressOnly(st) {
		job = &reportJob{instance: st.UUID, progress: &st}
	}
	s.pushReport(job)
}
