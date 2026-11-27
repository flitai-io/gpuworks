package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// Bounds on replaying a request a preemption cut off.
const (
	// replayBufferMax is the largest reply the gateway holds back to see
	// whether it was cut off; a larger one is passed through as it comes.
	replayBufferMax = 8 << 20
	// replayMax is how many times one request is replayed.
	replayMax = 3
)

// replaysPreemption says a request for dep is replayed when a preemption cuts
// it off: the deployment is one the arbiter puts to sleep for others while it
// is busy (a timeslice or anytime policy), and the request is not a stream — a stream's
// first tokens have reached the client before it could be cut off.
func replaysPreemption(dep *store.Deployment, r *http.Request) bool {
	if dep == nil || !dep.Policy().Preemptible() || r.GetBody == nil {
		return false
	}
	body, err := r.GetBody()
	if err != nil {
		return false
	}
	defer body.Close()
	facts, err := scanBodyFacts(body)
	return err == nil && !facts.stream
}

// serveResolved serves a request whose deployment and target are chosen,
// replaying it if a preemption cuts it off (replaysPreemption).
func (p *Proxy) serveResolved(w http.ResponseWriter, r *http.Request, modelName, affinity string, dep *store.Deployment, tgt target) {
	if replaysPreemption(dep, r) {
		p.serveReplayingPreemption(w, r, modelName, affinity, dep, tgt)
		return
	}
	p.serveSelectedTarget(w, r, modelName, tgt, dep.ID)
}

// serveReplayingPreemption serves a non-streaming request for a preemptible
// deployment, holding the reply back: one the engine ended with
// finish_reason "abort" (the deployment was put to sleep for another, docs/
// explanation/service-levels.md) is dropped, and the request waits for the
// deployment to serve again (holdForWake) and is sent once more. The client
// sees one reply, the one that ran to its end.
func (p *Proxy) serveReplayingPreemption(w http.ResponseWriter, r *http.Request, modelName, affinity string, dep *store.Deployment, tgt target) {
	for replay := 0; ; replay++ {
		held := newHeldReply(w)
		p.serveSelectedTarget(held, r, modelName, tgt, dep.ID)
		if held.passedThrough || replay == replayMax || held.status != http.StatusOK || !replyAborted(held.body.Bytes()) {
			held.release()
			return
		}
		body, err := r.GetBody()
		if err != nil {
			held.release()
			return
		}
		r.Body = body
		var rejection *refusal
		var asleep bool
		// The request was taken once: its replay is never refused as busy.
		dep, tgt, rejection, asleep = p.selectForRequest(withBusyDeclaration(r.Context(), false), modelName, affinity)
		if rejection != nil && asleep {
			dep, tgt, rejection, asleep = p.holdForWake(r.Context(), w, modelName, affinity, dep)
		}
		if rejection != nil {
			p.acct.RecordImmediate(deploymentIDOrZero(dep), rejection.status(), asleep)
			writeRefusal(w, r, rejection)
			return
		}
	}
}

// replyAborted says a chat or completion reply was cut off by the engine:
// some choice ended with finish_reason "abort".
func replyAborted(body []byte) bool {
	var reply struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &reply) != nil {
		return false
	}
	for _, choice := range reply.Choices {
		if choice.FinishReason == "abort" {
			return true
		}
	}
	return false
}

// heldReply keeps a reply back from the client until the gateway knows it
// ran to its end, up to replayBufferMax; past that it passes the reply
// through as it comes, and it is no longer replayed.
type heldReply struct {
	w             http.ResponseWriter
	header        http.Header
	status        int
	body          bytes.Buffer
	passedThrough bool
}

func newHeldReply(w http.ResponseWriter) *heldReply {
	return &heldReply{w: w, header: http.Header{}}
}

func (h *heldReply) Header() http.Header {
	if h.passedThrough {
		return h.w.Header()
	}
	return h.header
}

func (h *heldReply) WriteHeader(status int) {
	if h.passedThrough || h.status != 0 {
		return
	}
	h.status = status
}

func (h *heldReply) Write(data []byte) (int, error) {
	if h.status == 0 {
		h.status = http.StatusOK
	}
	if h.passedThrough {
		return h.w.Write(data)
	}
	if h.body.Len()+len(data) > replayBufferMax {
		h.passThrough()
		return h.w.Write(data)
	}
	return h.body.Write(data)
}

// Flush is a no-op while the reply is held: it is sent whole.
func (h *heldReply) Flush() {
	if h.passedThrough {
		if f, ok := h.w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// passThrough sends what is held and lets the rest go straight through.
func (h *heldReply) passThrough() {
	h.passedThrough = true
	h.copyOut()
}

// release sends the held reply to the client.
func (h *heldReply) release() {
	if h.passedThrough {
		return
	}
	h.copyOut()
}

func (h *heldReply) copyOut() {
	dst := h.w.Header()
	for k, v := range h.header {
		dst[k] = v
	}
	status := h.status
	if status == 0 {
		status = http.StatusOK
	}
	h.w.WriteHeader(status)
	if h.body.Len() > 0 {
		_, _ = h.w.Write(h.body.Bytes())
		h.body.Reset()
	}
}
