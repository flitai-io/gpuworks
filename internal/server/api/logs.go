package api

import (
	"bufio"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// instanceLogs streams an instance's container logs to the browser over SSE,
// sourced from the agent's logtail yamux stream via the hub.
func (a *apiHandlers) instanceLogs(w http.ResponseWriter, r *http.Request) {
	uuid := chi.URLParam(r, "uuid")
	inst, err := a.d.Store.InstanceByUUID(uuid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	node, err := a.d.Store.NodeByID(inst.NodeID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if a.d.NodeStreams == nil {
		writeErr(w, http.StatusServiceUnavailable, errNoLogtail)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errStreamingUnsupported)
		return
	}

	conn, err := a.d.NodeStreams.OpenLogtail(r.Context(), node.UUID, uuid)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	defer conn.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Close the tunnel when the browser disconnects.
	go func() {
		<-r.Context().Done()
		_ = conn.Close()
	}()

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if err := writeSSELogLine(w, sc.Text()); err != nil {
			return
		}
		fl.Flush()
	}
}

// writeSSELogLine writes a log line as a plain-text SSE event. Log lines used
// to be JSON-marshaled here, which made the browser display quotes and escaped
// control characters (for example, "\\u001b") instead of the original line.
// Prefix every physical line with data: so this remains valid SSE even if a
// custom runtime returns an embedded newline.
func writeSSELogLine(w io.Writer, line string) error {
	// A carriage return is a line terminator in the SSE grammar. Drop it rather
	// than allowing progress-bar output to split one event unexpectedly.
	line = strings.ReplaceAll(line, "\r", "")
	for _, part := range strings.Split(line, "\n") {
		if _, err := io.WriteString(w, "data: "+part+"\n"); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "\n")
	return err
}

type staticErr string

func (e staticErr) Error() string { return string(e) }

const errNoLogtail = staticErr("log streaming not available")
