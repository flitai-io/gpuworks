package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type memFile struct {
	bytes.Buffer
	closed bool
}

func (f *memFile) Close() error { f.closed = true; return nil }

// A debug session records a streamed request's whole exchange: the request
// as prepared by the gateway (with the usage the gateway asked for), the engine's
// bytes, the client's (without the usage event), and how it ended.
func TestDebugSessionRecordsTheRawExchange(t *testing.T) {
	upstream := &vllmUpstream{t: t}
	p, _, key, depID, closeUpstream := newAccountingProxy(t, upstream)
	defer closeUpstream()
	p.routes.rebuild()
	rec := &fileRecorder{dep: depID, files: map[string]*memFile{}}
	p.SetRequestRecorder(rec)

	client := `{"model":"omni","messages":[{"role":"user","content":"What is 17 plus 25?"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(client))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "session=secret")
	out := httptest.NewRecorder()
	p.Handler().ServeHTTP(out, req)
	if out.Code != http.StatusOK {
		t.Fatalf("status %d", out.Code)
	}
	id := out.Header().Get("X-Request-ID")
	f := rec.files[id]
	if f == nil || !f.closed {
		t.Fatalf("no closed capture for %s: %v", id, rec.files)
	}
	var lines []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(f.Bytes()))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		lines = append(lines, m)
	}
	first, last := lines[0], lines[len(lines)-1]
	if first["t"] != "request" || !strings.HasPrefix(first["body"].(string), `{"stream_options":{"include_usage":true},`) ||
		strings.Contains(f.String(), "secret") || strings.Contains(f.String(), key) {
		t.Fatalf("request line = %v", first)
	}
	if lines[1]["t"] != "response" || lines[1]["status"] != float64(200) {
		t.Fatalf("response line = %v", lines[1])
	}
	var up, down strings.Builder
	for _, l := range lines {
		switch l["t"] {
		case "upstream":
			up.WriteString(l["data"].(string))
		case "client":
			down.WriteString(l["data"].(string))
		}
	}
	if !strings.Contains(up.String(), `"choices":[],"usage"`) || strings.Contains(down.String(), `"choices":[],"usage"`) || down.String() != out.Body.String() {
		t.Fatal("upstream must hold the usage event the client never saw, and client bytes must be what the client got")
	}
	if last["t"] != "end" || last["outcome"] != "ok" || last["status"] != float64(200) {
		t.Fatalf("end line = %v", last)
	}
	if last["observation_scope"] != "gateway" || last["finish_reason"] != "stop" {
		t.Fatalf("terminal gateway evidence = %v", last)
	}
	if last["read_error"] != nil || last["write_error"] != nil {
		t.Fatalf("successful response reported a copy failure: %v", last)
	}
	if n := strings.Count(f.String(), `"t":"end"`); n != 1 {
		t.Fatalf("%d end lines", n)
	}

	// Another deployment's requests are not recorded.
	rec.dep = depID + 1
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(client))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	out = httptest.NewRecorder()
	p.Handler().ServeHTTP(out, req)
	if rec.files[out.Header().Get("X-Request-ID")] != nil {
		t.Fatal("recorded a deployment the session does not cover")
	}
}

type fileRecorder struct {
	dep   uint
	files map[string]*memFile
}

func (r *fileRecorder) Begin(deploymentID uint, requestID string) io.WriteCloser {
	if deploymentID != r.dep {
		return nil
	}
	f := &memFile{}
	r.files[requestID] = f
	return f
}
