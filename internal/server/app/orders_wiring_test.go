package app

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The API's sleep and wake orders take the controller's order lock, the one
// the automation takes: a sleep order waits while the controller's Orders is
// locked. An API left with Orders of its own would answer at once, and its
// orders and the automation's would no longer exclude each other.
func TestTheAPIsOrdersAreTheControllers(t *testing.T) {
	dir := t.TempDir()
	application, err := New(Config{Addr: "127.0.0.1:0", DataDir: dir, DBPath: dir + "/gpuworks.db", AdminUser: "admin", AdminPassword: "gpuworks-test-password-2026"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { application.proxy.Close() })
	handler := application.http.Handler

	login := httptest.NewRecorder()
	handler.ServeHTTP(login, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(`{"username":"admin","password":"gpuworks-test-password-2026"}`)))
	var session struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &session); login.Code != http.StatusOK || err != nil || session.Token == "" {
		t.Fatalf("login = %d %s", login.Code, login.Body.String())
	}

	unlock := application.ctrl.Orders().Lock()
	answered := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/999/sleep", nil)
		req.Header.Set("Authorization", "Bearer "+session.Token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		answered <- rec.Code
	}()
	select {
	case code := <-answered:
		unlock()
		t.Fatalf("the sleep order answered %d while the controller's order lock was held", code)
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case code := <-answered:
		if code != http.StatusNotFound {
			t.Fatalf("the sleep order for a missing deployment answered %d, want 404", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the sleep order did not answer after the lock was released")
	}
}
