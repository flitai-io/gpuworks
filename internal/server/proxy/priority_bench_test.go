package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/priority"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// BenchmarkGatewayOwnedFields measures the body work the gateway does on
// every dispatched JSON request for the fields only it may set
// (ownGatewayFields): the one scan that finds which of them the body names,
// and the rewrite that writes the priority. The cases:
//
//   - off/normal: a deployment without priority scheduling; the scan alone,
//     the work every request did before request priority;
//   - on/normal: priority scheduling at the neutral class; the scan alone;
//   - on/high: a class with a value, inserted into a body without the field;
//   - on/high/client-field: the client's own field replaced.
//
// Run:
//
//	go test ./internal/server/proxy/ -run '^$' -bench BenchmarkGatewayOwnedFields -benchmem
func BenchmarkGatewayOwnedFields(b *testing.B) {
	rig := newPriorityRig(b)
	// One request through the handler loads the route table and the classes.
	if rec, _, _ := rig.send(b, rig.key(b, store.APIKeyPriority{}), "v", "", ""); rec.Code != http.StatusOK {
		b.Fatalf("warm-up = %d %s", rec.Code, rec.Body)
	}
	table := rig.p.priority.get()
	for _, size := range []int{2 << 10, 32 << 10, 256 << 10} {
		for _, tc := range []struct {
			name, deployment, class string
			clientField             bool
		}{
			{"off/normal", "off", priority.Normal, false},
			{"on/normal", "v", priority.Normal, false},
			{"on/high", "v", priority.High, false},
			{"on/high/client-field", "v", priority.High, true},
		} {
			class, ok := table.ByName(tc.class)
			if !ok {
				b.Fatalf("no class %q", tc.class)
			}
			body := benchChatBody(tc.deployment, size, tc.clientField)
			tgt := target{instanceUUID: tc.deployment + "-instance"}
			id := rig.deployments[tc.deployment]
			b.Run(fmt.Sprintf("%dKiB/%s", size>>10, tc.name), func(b *testing.B) {
				b.SetBytes(int64(len(body)))
				b.ReportAllocs()
				for b.Loop() {
					r, release := benchReplayRequest(b, body, priority.Decision{Class: class})
					w := httptest.NewRecorder()
					_, cleanup, ok := rig.p.ownGatewayFields(w, r, tgt, id, func(err error) { b.Fatal(err) })
					if !ok {
						b.Fatalf("refused: %d %s", w.Code, w.Body)
					}
					cleanup()
					release()
				}
			})
		}
	}
}

// benchChatBody is a chat request of about size bytes of prompt text.
func benchChatBody(model string, size int, clientField bool) []byte {
	field := ""
	if clientField {
		field = `"priority":-1000,`
	}
	prompt := strings.Repeat("The quick brown fox jumps over the lazy dog. ", size/45+1)[:size]
	return []byte(`{"model":"` + model + `",` + field + `"stream":true,"max_tokens":256,"messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"` + prompt + `"}]}`)
}

// benchReplayRequest is a request as the gateway holds it at dispatch: its
// body replayable, its class decided. release gives the body's share of the
// replay memory budget back, as the end of a request does; without it the
// budget runs out and later bodies spool to disk.
func benchReplayRequest(b *testing.B, body []byte, decision priority.Decision) (*http.Request, func()) {
	b.Helper()
	rb, err := newReplayBody(body, DefaultMaxBodyBytes)
	if err != nil {
		b.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Content-Type", "application/json")
	installReplayBody(r, rb)
	return r.WithContext(context.WithValue(r.Context(), priorityDecisionKey{}, decision)), func() { _ = rb.Close() }
}
