package api

import (
	"encoding/json"
	"hash/fnv"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// The overview is rebuilt at most every overviewRefreshFloor, and then only
// once a node, an instance or a deployment changed since it was built (the
// bus's generation of those topics); with no change it is rebuilt every
// overviewMaxAge, which keeps what the bus does not announce - the gateway's
// held requests, the hour of yields - from going stale. Between rebuilds a
// request is answered with the bytes of the last one. A fleet's nodes report
// status every few seconds, so without the floor every report would cost a
// pass over the fleet.
const (
	overviewRefreshFloor = 2 * time.Second
	overviewMaxAge       = 10 * time.Second
)

// overviewTopics are the bus topics whose changes invalidate the overview.
var overviewTopics = []string{"node", "instance", "deployment"}

// overviewSnapshot is one build of the overview, already encoded.
type overviewSnapshot struct {
	body []byte
	// summary is the build's fleet summary, which the dashboard shows too.
	summary dto.OverviewSummary
	etag    string
	at      time.Time
	// generation is the bus generation the snapshot was built at.
	generation uint64
}

// overviewCache holds the last build. rebuilds counts the builds, which the
// tests and the fleet perf case read.
type overviewCache struct {
	mu       sync.Mutex
	snapshot *overviewSnapshot
	rebuilds atomic.Int64
	// sharing is the fleet's sharing stats (scheduling_timeline_sharing.go).
	sharing sharingCache
	// activity is the scheduling activity's answers per range
	// (scheduling_activity.go).
	activity activityCache
	// timeline is the answers of custom ranges in the past
	// (scheduling_timeline_range.go).
	timeline timelineCache
}

// schedulingOverview answers GET /scheduling/overview: every node and each
// card's compact state in one response, plus the fleet summary (dto.SchedulingOverview).
// It is served from a cache (overviewCache); the response carries an ETag, so
// a poll that finds nothing changed costs a 304. It only reads.
func (a *apiHandlers) schedulingOverview(w http.ResponseWriter, r *http.Request) {
	snap, err := a.currentOverview()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("ETag", snap.etag)
	w.Header().Set("Cache-Control", "no-cache")
	if match := r.Header.Get("If-None-Match"); match != "" && match == snap.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(snap.body)
}

func (a *apiHandlers) currentOverview() (*overviewSnapshot, error) {
	c := &a.overview
	c.mu.Lock()
	defer c.mu.Unlock()
	generation := a.overviewGeneration()
	if snap := c.snapshot; snap != nil {
		age := time.Since(snap.at)
		if age < overviewMaxAge && (generation == snap.generation || age < overviewRefreshFloor) {
			return snap, nil
		}
	}
	// The generation is read before the build, so a change that lands during
	// it leaves the snapshot stale and the next request after the floor rebuilds.
	out, err := a.buildOverview(time.Now().UTC())
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	sum := fnv.New64a()
	_, _ = sum.Write(body)
	c.snapshot = &overviewSnapshot{body: body, summary: out.Summary, etag: `"` + strconv.FormatUint(sum.Sum64(), 36) + `"`, at: time.Now().UTC(), generation: generation}
	c.rebuilds.Add(1)
	return c.snapshot, nil
}

// overviewGeneration is the bus's count of changes to what the overview is
// drawn from; 0 without a bus.
func (a *apiHandlers) overviewGeneration() uint64 {
	if a.d.Bus == nil {
		return 0
	}
	var generation uint64
	for _, topic := range overviewTopics {
		generation += a.d.Bus.Generation(topic)
	}
	return generation
}
