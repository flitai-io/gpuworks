package api

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An open console's event stream must not re-read its login for every change
// it forwards: a fleet's change rate times the open tabs would otherwise be
// the session table's read rate. The login is re-checked on a change to a
// login (the management-session topic) and on the stream's own clock, which
// is what catches a revocation another process writes.
func TestEventsStreamDoesNotReadTheLoginPerForwardedEvent(t *testing.T) {
	st, err := store.Open("file:events_auth_reads_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, _, _, err := st.EnsureAdmin("admin", "gpuworks-test-password-2026"); err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner("session-test-secret", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	router := Router(Deps{Store: st, Bus: st.Bus, AuthSigner: signer})
	cookie, _ := sessionLogin(t, router, "gpuworks-test-password-2026")
	server := httptest.NewServer(router)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/events", nil)
	req.AddCookie(cookie)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("SSE status=%d", response.StatusCode)
	}
	frames := readSSEFrames(t, bufio.NewReader(response.Body))
	if first := collectSSE(frames, 200*time.Millisecond); len(first) == 0 {
		t.Fatal("no initial frame")
	}

	var loginReads atomic.Int64
	name := "test:events_login_reads"
	if err := st.DB().Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "management_sessions" {
			loginReads.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.DB().Callback().Query().Remove(name) })

	const events = 200
	start := time.Now()
	for i := 0; i < events; i++ {
		st.Bus.PublishChange("deployment", fmt.Sprint(i), "state")
	}
	got := collectSSE(frames, 300*time.Millisecond)
	elapsed := time.Since(start)
	if len(got) == 0 {
		t.Fatal("no deployment event forwarded")
	}
	// The stream's own clock ticks once a second; allow for each tick in the
	// window, and one for a tick already due.
	if limit := int64(elapsed/time.Second) + 1; loginReads.Load() > limit {
		t.Fatalf("login read %d times while forwarding %d events in %v; want at most %d (the stream's clock, not one per event)",
			loginReads.Load(), events, elapsed.Round(time.Millisecond), limit)
	}
}
