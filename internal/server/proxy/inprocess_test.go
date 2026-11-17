package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// A request sent in process is a client's request: routed by its model,
// answered, counted and recorded for the caller that sent it.
func TestDoSendsARequestAsTheCallersAndAccountsIt(t *testing.T) {
	var got atomic.Value
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.Store(r.URL.Path + " " + string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	})
	p, acct, _, depID, closeUpstream := newAccountingProxy(t, upstream)
	defer closeUpstream()
	var records []uxrecord.Request
	p.SetRequestSink(func(r uxrecord.Request) { records = append(records, r) })

	// A batch's requests carry its key's ID, which must name a real key: its
	// scope is read at send time.
	if _, err := p.store.CreateAPIKey("batch-owner", 9); err != nil {
		t.Fatal(err)
	}
	var key store.APIKey
	if err := p.store.DB().Where("name = ?", "batch-owner").First(&key).Error; err != nil {
		t.Fatal(err)
	}
	reply := p.Do(context.Background(), uxrecord.Caller{APIKeyID: key.ID, UserID: 9, Source: uxrecord.SourceBatch}, chatPath, []byte(`{"model":"omni","messages":[]}`))
	if reply.Status != http.StatusOK || reply.Err != nil || reply.TooLarge || !strings.Contains(string(reply.Body), `"finish_reason":"stop"`) || reply.RequestID == "" {
		t.Fatalf("reply = %+v", reply)
	}
	if v, _ := got.Load().(string); v != chatPath+` {"model":"omni","messages":[]}` {
		t.Fatalf("the replica received %q", v)
	}
	if _, ok := acct.LastRequestAt(depID); !ok {
		t.Error("the request did not count as the deployment's last request")
	}
	if _, samples := acct.snapshotAndClearDirty(); len(samples) == 0 {
		t.Error("the request was not accounted")
	}
	if len(records) != 1 || records[0].APIKeyID != key.ID || records[0].UserID != 9 || records[0].Source != uxrecord.SourceBatch {
		t.Fatalf("records = %+v, want the batch source kept", records)
	}
	if live := p.LiveRequests(depID, 10); len(live.Requests) != 0 {
		t.Errorf("still in flight: %+v", live)
	}
}

func TestDoReportsTheGatewaysRefusalsAndTheContext(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), "bad"):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"bad prompt"}}`)
		case strings.Contains(string(body), "slow"):
			<-r.Context().Done()
		default:
			_, _ = io.WriteString(w, "ok")
		}
	})
	p, _, _, _, closeUpstream := newAccountingProxy(t, upstream)
	defer closeUpstream()

	unknown := p.Do(context.Background(), uxrecord.Caller{}, chatPath, []byte(`{"model":"nope"}`))
	if unknown.Status != http.StatusNotFound || unknown.RequestID == "" ||
		string(unknown.Body) != `{"error":{"message":"no deployment serves model nope","type":"not_found_error","code":"MODEL_NOT_FOUND","param":"model","params":{"model":"nope"}}}`+"\n" {
		t.Fatalf("unknown model = %+v %s", unknown, unknown.Body)
	}
	noModel := p.Do(context.Background(), uxrecord.Caller{}, chatPath, []byte(`{}`))
	if noModel.Status != http.StatusBadRequest {
		t.Fatalf("no model = %+v", noModel)
	}
	bad := p.Do(context.Background(), uxrecord.Caller{}, chatPath, []byte(`{"model":"omni","x":"bad"}`))
	if bad.Status != http.StatusBadRequest || !strings.Contains(string(bad.Body), "bad prompt") {
		t.Fatalf("upstream 400 = %+v", bad)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	slow := p.Do(ctx, uxrecord.Caller{}, chatPath, []byte(`{"model":"omni","x":"slow"}`))
	if slow.Err == nil {
		t.Fatalf("a request cut by its context reports no error: %+v", slow)
	}
}

func TestDoCutsAResponseOverTheLimit(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 4096))
	})
	p, _, _, _, closeUpstream := newAccountingProxy(t, upstream)
	defer closeUpstream()
	old := inProcessReplyLimit
	inProcessReplyLimit = 1024
	defer func() { inProcessReplyLimit = old }()
	reply := p.Do(context.Background(), uxrecord.Caller{}, chatPath, []byte(`{"model":"omni"}`))
	if !reply.TooLarge || len(reply.Body) > 1024 {
		t.Fatalf("reply = status %d tooLarge=%v body=%d", reply.Status, reply.TooLarge, len(reply.Body))
	}
}

// A request for a deployment the platform put to sleep is held while it wakes,
// as a client's is.
func TestDoWaitsForASleepingDeploymentToWake(t *testing.T) {
	p, st, _, depID, _ := newSleepingProxy(t, false)
	p.SetWakeDemand(func(id uint) {
		go func() {
			time.Sleep(150 * time.Millisecond)
			_ = updateInstanceRow(st, "omni-instance", map[string]any{"sleep_level": 0})
		}()
	})
	start := time.Now()
	reply := p.Do(context.Background(), uxrecord.Caller{}, chatPath, []byte(`{"model":"omni"}`))
	if reply.Status != http.StatusOK || string(reply.Body) != `{"ok":true}` || time.Since(start) < 150*time.Millisecond {
		t.Fatalf("reply = %+v after %v", reply, time.Since(start))
	}
	_ = depID
}

func TestDoRefusesAnOperatorsSleepAtOnce(t *testing.T) {
	p, _, _, _, _ := newSleepingProxy(t, true) // an operator's sleep is not held: refused at once
	reply := p.Do(context.Background(), uxrecord.Caller{}, chatPath, []byte(`{"model":"omni"}`))
	if reply.Status != http.StatusServiceUnavailable {
		t.Fatalf("reply = %+v", reply)
	}
}

func TestIdentifyNamesTheKeyOrRefuses(t *testing.T) {
	p, _, key, _, closeUpstream := newAccountingProxy(t, http.NotFoundHandler())
	defer closeUpstream()
	req := func(header string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/files", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		return r
	}
	if c, ok := p.Identify(req("Bearer " + key)); !ok || c.APIKeyID == 0 || c.Source != uxrecord.SourceAPI || c.ReceivedMS == 0 {
		t.Fatalf("valid key = %+v %v", c, ok)
	}
	for _, header := range []string{"", "Bearer wrong", "Basic abc"} {
		if _, ok := p.Identify(req(header)); ok {
			t.Errorf("%q was identified", header)
		}
	}
}
