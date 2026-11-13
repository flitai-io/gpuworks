package batch

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ModelWait is why a running batch's requests for one model are being
// retried: what the gateway answered last, since when the model has had
// requests waiting, and how many wait now. It lives in memory only, for the
// batch's executor: after a restart it fills again from the next answers.
type ModelWait struct {
	Model string
	// Status is the HTTP status of the last answer, 0 for none (a transport
	// failure); Message the gateway's words.
	Status  int
	Message string
	// Since is when the oldest of the waiting requests was first retried.
	Since time.Time
	// Requests is how many requests wait for a retry now.
	Requests int
}

// waitBoard is the ModelWait of each model a batch has requests waiting for.
type waitBoard struct {
	mu     sync.Mutex
	models map[string]*ModelWait
}

// retrying records that a request for model got an answer it is retried for.
// first says the request was not waiting yet.
func (w *waitBoard) retrying(model string, first bool, reply Reply, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.models == nil {
		w.models = map[string]*ModelWait{}
	}
	m := w.models[model]
	if m == nil {
		m = &ModelWait{Model: model, Since: now}
		w.models[model] = m
	}
	if first {
		m.Requests++
	}
	m.Status, m.Message = reply.Status, replyMessage(reply)
}

// settled records that a waiting request for model has its answer, or was
// given up; the model leaves the board with its last waiting request.
func (w *waitBoard) settled(model string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	m := w.models[model]
	if m == nil {
		return
	}
	if m.Requests--; m.Requests <= 0 {
		delete(w.models, model)
	}
}

// list is the board by model name.
func (w *waitBoard) list() []ModelWait {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]ModelWait, 0, len(w.models))
	for _, m := range w.models {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// replyMessage is what an answer that is retried says: the error message of
// the gateway's or the engine's body, or the transport failure.
func replyMessage(r Reply) string {
	if r.Status == 0 {
		if r.Err != nil {
			return r.Err.Error()
		}
		return ""
	}
	if r.Status >= 200 && r.Status < 300 {
		return "the response was cut off by a preemption (finish_reason abort)"
	}
	_, message := upstreamError(r)
	if message == http.StatusText(r.Status) {
		return message
	}
	return clip(message, 300)
}

// Waits is why the requests of a running batch are being retried, by model;
// none for a batch that is not running or has no request waiting.
func (s *Service) Waits(batchID string) []ModelWait {
	s.mu.Lock()
	h := s.runs[batchID]
	s.mu.Unlock()
	if h == nil {
		return nil
	}
	return h.waits.list()
}

// requestModel is the model a request body names.
func requestModel(body []byte) string {
	var doc struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return ""
	}
	return strings.TrimSpace(doc.Model)
}

// stoppedModelTTL is how long a lookup of whether a model's deployment is
// stopped is reused: a batch's requests for it are refused at once, and each
// must not cost a read.
const stoppedModelTTL = 5 * time.Second

type stoppedLookup struct {
	stopped bool
	at      time.Time
}

// modelStopped says model is served only by stopped deployments: the gateway
// refuses its requests with 404 as if no deployment served it, but someone
// may start it, so a batch waits for that rather than failing them.
func (s *Service) modelStopped(model string) bool {
	if s.cfg.Models == nil || model == "" {
		return false
	}
	now := s.cfg.Clock.Now()
	// Hold the lock across the read. Releasing it first lets every concurrent
	// refusal miss an empty cache and pay for its own lookup.
	s.stoppedMu.Lock()
	defer s.stoppedMu.Unlock()
	if l, ok := s.stopped[model]; ok && now.Sub(l.at) < stoppedModelTTL {
		return l.stopped
	}
	entry, err := s.cfg.Models.Lookup(model)
	stopped := err == nil && entry.State == ModelStopped
	if s.stopped == nil {
		s.stopped = map[string]stoppedLookup{}
	}
	for name, l := range s.stopped {
		if now.Sub(l.at) >= stoppedModelTTL {
			delete(s.stopped, name)
		}
	}
	s.stopped[model] = stoppedLookup{stopped: stopped, at: now}
	return stopped
}

// LookupModel is what a served name is to a batch now (Models.Lookup);
// unknown without a Models.
func (s *Service) LookupModel(name string) (ModelEntry, error) {
	if s.cfg.Models == nil {
		return ModelEntry{Name: name}, nil
	}
	return s.cfg.Models.Lookup(name)
}
