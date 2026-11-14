package api

import (
	"encoding/json"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// nodeStatusWindow bounds how often one stream forwards routine node status
// reports. Every agent reports once a minute, so a thousand nodes would
// otherwise make each open page refetch its lists many times a second; the
// first report in a window goes out at once and the rest collapse into one
// event at the window's end. Other node changes are never held back.
var nodeStatusWindow = 5 * time.Second

// events streams change notifications to the UI over Server-Sent Events.
// The UI re-fetches affected resources on each event (events carry no payload),
// which keeps the stream tiny and the client logic idempotent.
func (a *apiHandlers) events(w http.ResponseWriter, r *http.Request) {
	if !a.managementStreamAllowed(r, false) {
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sub := a.d.Bus.Subscribe(128) // all topics
	defer sub.Close()

	// Prime the client so it does an initial fetch on connect.
	writeSSE(w, "node", map[string]string{"reason": "connected"})
	fl.Flush()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	authTicker := time.NewTicker(time.Second)
	defer authTicker.Stop()
	var lastStatus time.Time
	var statusFlush <-chan time.Time

	for {
		select {
		case <-r.Context().Done():
			return
		case <-authTicker.C:
			// The clock catches what no event here announces: a login
			// that expires, and one revoked by another process (reset-password).
			if !a.managementStreamAllowed(r, false) {
				return
			}
		case ev := <-sub.C():
			// Only a change to a login can revoke this one: re-reading it
			// for every other change made the session table's read rate the
			// fleet's change rate times the open consoles.
			if ev.Topic == "management-session" {
				if !a.managementStreamAllowed(r, false) {
					return
				}
				continue
			}
			if ev.Topic == "node" && ev.Reason == store.NodeChangeStatus {
				if statusFlush != nil {
					continue
				}
				if wait := nodeStatusWindow - time.Since(lastStatus); wait > 0 {
					statusFlush = time.After(wait)
					continue
				}
				lastStatus = time.Now().UTC()
			}
			writeSSE(w, ev.Topic, map[string]string{"key": ev.Key, "reason": ev.Reason})
			fl.Flush()
		case <-statusFlush:
			// The held reports name several nodes; the event carries none.
			statusFlush = nil
			lastStatus = time.Now().UTC()
			writeSSE(w, "node", map[string]string{"key": "", "reason": store.NodeChangeStatus})
			fl.Flush()
		case <-ticker.C:
			// Keepalive comment frame.
			_, _ = w.Write([]byte(": ping\n\n"))
			fl.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, event string, data any) {
	b, _ := json.Marshal(data)
	_, _ = w.Write([]byte("event: " + event + "\ndata: "))
	_, _ = w.Write(b)
	_, _ = w.Write([]byte("\n\n"))
}
