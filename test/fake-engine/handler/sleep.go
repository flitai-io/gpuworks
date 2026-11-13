package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// sleepState mirrors what vLLM's sleep mode does to an engine, so a controller
// driven against the fixture is held to the same protocol as against the real
// thing:
//
//   - POST /sleep?level=1|2&mode=abort|wait|keep puts the engine to sleep. Level
//     1 keeps the weights (in host RAM); level 2 discards them.
//   - POST /wake_up brings an L1 sleeper back in one step. An L2 sleeper woken
//     with the plain order comes back with weight buffers that hold nothing,
//     and serves garbage - exactly as vLLM does - which is how a test catches
//     a controller that forgot the reload.
//   - POST /wake_up?tags=weights, POST /collective_rpc {"method":"reload_weights"},
//     POST /wake_up?tags=kv_cache is the L2 wake sequence.
//   - Inference requests that arrive while the engine is asleep block until it
//     is awake (or the client gives up); vLLM queues them, it does not refuse.
type sleepState struct {
	mu                  sync.Mutex
	level               int // requested depth; sleepingTags is the observed executor state
	sleepingTags        map[string]bool
	weightsValid        bool // false after an L2 sleep until reload_weights
	kvAwake             bool // false after any sleep until the kv_cache buffers are restored
	awake               chan struct{}
	wakeFailures        int
	partialWakeFailures int
	sleepDelay          time.Duration
	wakeDelay           time.Duration
}

func newSleepState() *sleepState {
	return &sleepState{weightsValid: true, kvAwake: true, awake: closedChan(), sleepingTags: map[string]bool{}}
}

func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func (s *sleepState) sleeping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sleepingTags) > 0
}

// waitAwake blocks an inference request until the engine can serve or the
// request is abandoned.
func (s *sleepState) waitAwake(ctx context.Context) bool {
	for {
		s.mu.Lock()
		ch := s.awake
		ready := len(s.sleepingTags) == 0 && s.kvAwake
		s.mu.Unlock()
		if ready {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ch:
		}
	}
}

func (s *sleepState) sleep(level int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.level = level
	s.kvAwake = false
	s.sleepingTags = map[string]bool{"kv_cache": true}
	if level >= 2 {
		s.weightsValid = false
		s.sleepingTags["weights"] = true
	}
	if isClosed(s.awake) {
		s.awake = make(chan struct{})
	}
}

// wake restores the named buffers; no tags restores every buffer. Restoring
// the weight buffers of an L2 sleeper does not restore their contents.
func (s *sleepState) wake(tags []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	weights := len(tags) == 0
	kv := len(tags) == 0
	for _, tag := range tags {
		switch tag {
		case "weights":
			weights = true
		case "kv_cache":
			kv = true
		}
	}
	if weights {
		delete(s.sleepingTags, "weights")
	}
	if kv {
		delete(s.sleepingTags, "kv_cache")
		s.kvAwake = true
	}
	if len(s.sleepingTags) == 0 && s.kvAwake && !isClosed(s.awake) {
		close(s.awake)
	}
}

// reloadWeights is what collective_rpc reload_weights does: it fills weight
// buffers that exist. It fails when they do not, which is the case while the
// engine is still asleep at L2.
func (s *sleepState) reloadWeights() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sleepingTags["weights"] {
		return false
	}
	s.weightsValid = true
	return true
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func registerSleep(mux *http.ServeMux, state *sleepState) {
	mux.HandleFunc("/sleep", func(w http.ResponseWriter, r *http.Request) {
		// Keep GET as a compatibility probe for the Docker smoke fixture; the
		// production vLLM contract uses POST and all level/mode validation still
		// follows that contract.
		if r.Method != http.MethodPost && r.Method != http.MethodGet {
			requireMethod(w, r, http.MethodPost)
			return
		}
		level := 1
		switch raw := r.URL.Query().Get("level"); raw {
		case "", "1":
		case "2":
			level = 2
		default:
			// vLLM does int(level) and lets the executor refuse the number.
			http.Error(w, "invalid sleep level: "+raw, http.StatusBadRequest)
			return
		}
		switch mode := r.URL.Query().Get("mode"); mode {
		case "", "abort", "wait", "keep":
		default:
			// vLLM's pause_scheduler raises on an unknown mode.
			http.Error(w, "invalid pause mode: "+mode, http.StatusBadRequest)
			return
		}
		state.mu.Lock()
		delay := state.sleepDelay
		state.mu.Unlock()
		if delay > 0 {
			// Deliberately ignore r.Context(): vLLM's wait-mode scheduler may
			// finish the pause after the caller has given up waiting.
			time.Sleep(delay)
		}
		state.sleep(level)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/debug/sleep-delay", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		ms, err := strconv.Atoi(r.URL.Query().Get("ms"))
		if err != nil || ms < 0 {
			http.Error(w, "ms must be a non-negative integer", http.StatusBadRequest)
			return
		}
		state.mu.Lock()
		state.sleepDelay = time.Duration(ms) * time.Millisecond
		state.mu.Unlock()
		writeJSON(w, map[string]int{"sleep_delay_ms": ms})
	})
	mux.HandleFunc("/wake_up", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodGet {
			requireMethod(w, r, http.MethodPost)
			return
		}
		state.mu.Lock()
		delay := state.wakeDelay
		fail := state.wakeFailures > 0
		if fail {
			state.wakeFailures--
		}
		state.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		if fail {
			http.Error(w, "injected wake failure", http.StatusInternalServerError)
			return
		}
		tags := r.URL.Query()["tags"]
		for _, tag := range tags {
			if tag != "weights" && tag != "kv_cache" {
				http.Error(w, "invalid wake tag: "+tag, http.StatusBadRequest)
				return
			}
		}
		partial := false
		if len(tags) == 1 && tags[0] == "weights" {
			state.mu.Lock()
			partial = state.partialWakeFailures > 0
			if partial {
				state.partialWakeFailures--
			}
			state.mu.Unlock()
		}
		state.wake(tags)
		if partial {
			http.Error(w, "injected partial wake failure", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/debug/partial-wake-failures", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		count, err := strconv.Atoi(r.URL.Query().Get("count"))
		if err != nil || count < 0 {
			http.Error(w, "count must be a non-negative integer", http.StatusBadRequest)
			return
		}
		state.mu.Lock()
		state.partialWakeFailures = count
		state.mu.Unlock()
		writeJSON(w, map[string]int{"partial_wake_failures": count})
	})
	mux.HandleFunc("/debug/wake-failures", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		count, err := strconv.Atoi(r.URL.Query().Get("count"))
		if err != nil || count < 0 {
			http.Error(w, "count must be a non-negative integer", http.StatusBadRequest)
			return
		}
		state.mu.Lock()
		state.wakeFailures = count
		state.mu.Unlock()
		writeJSON(w, map[string]int{"wake_failures": count})
	})
	mux.HandleFunc("/debug/wake-delay", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		ms, err := strconv.Atoi(r.URL.Query().Get("ms"))
		if err != nil || ms < 0 {
			http.Error(w, "ms must be a non-negative integer", http.StatusBadRequest)
			return
		}
		state.mu.Lock()
		state.wakeDelay = time.Duration(ms) * time.Millisecond
		state.mu.Unlock()
		writeJSON(w, map[string]int{"wake_delay_ms": ms})
	})
	mux.HandleFunc("/is_sleeping", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]bool{"is_sleeping": state.sleeping()})
	})
	mux.HandleFunc("/collective_rpc", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var body struct {
			Method *string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "JSON decode error: "+err.Error(), http.StatusBadRequest)
			return
		}
		if body.Method == nil {
			http.Error(w, "Missing 'method' in request body", http.StatusBadRequest)
			return
		}
		if *body.Method != "reload_weights" {
			// The real endpoint would dispatch any worker method; the fixture
			// admits only the one the platform relies on, so a typo is caught
			// rather than silently accepted.
			http.Error(w, "unsupported collective_rpc method: "+*body.Method, http.StatusBadRequest)
			return
		}
		if !state.reloadWeights() {
			http.Error(w, "reload_weights: weight buffers are not allocated (engine asleep)", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"results": []any{nil}})
	})
	// vLLM requires prefix-cache invalidation after an L2 wake. The fixture
	// accepts the endpoint so tests exercise the complete production sequence.
	mux.HandleFunc("/reset_prefix_cache", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

// gateInference wraps the inference routes so that a request arriving while
// the engine is asleep waits, and one served after a bad L2 wake gets garbage.
func gateInference(next http.Handler, state *sleepState) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") && r.URL.Path != "/v1/models" {
			if !state.waitAwake(r.Context()) {
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
