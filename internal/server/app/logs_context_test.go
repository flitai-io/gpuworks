package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/logstore"
	"gpuworks.ai/gpuworks/internal/server/obsingest"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// TestLogsContextMissingAnchorWire pins the /logs/context wire contract for a
// nonexistent anchor (GWKS-147) through the production router, the production
// callback and a real SQLite store: the response stays 200 and match_index is
// -1, so a caller that bounds-checks it never reads a neighbouring row as the
// match. Measured before the fix: items had 2 rows and match_index was 2.
func TestLogsContextMissingAnchorWire(t *testing.T) {
	dir := t.TempDir()
	ls, err := logstore.OpenSQLite(filepath.Join(dir, "logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ls.Close() })
	ts := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	if err := ls.Append([]logstore.Entry{
		{Instance: "inst-a", Node: "node-a", TimeMS: ts.UnixMilli() - 2000, Level: logstore.LevelInfo, Source: logstore.SourceInstance, Line: "first line"},
		{Instance: "inst-a", Node: "node-a", TimeMS: ts.UnixMilli() - 1000, Level: logstore.LevelError, Source: logstore.SourceInstance, Line: "matched line"},
		{Instance: "inst-a", Node: "node-a", TimeMS: ts.UnixMilli(), Level: logstore.LevelInfo, Source: logstore.SourceInstance, Line: "last line"},
	}); err != nil {
		t.Fatal(err)
	}
	res, err := ls.Search(logstore.Filter{Query: `"matched line"`, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	anchor := res.Entries[0]

	signer, err := auth.NewTokenSigner(filepath.Join(dir, "auth-secret"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "management.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, _, _, err := st.EnsureAdmin("admin", "gpuworks-test-password-2026"); err != nil {
		t.Fatal(err)
	}
	user, err := st.AuthenticateUser("admin", "gpuworks-test-password-2026")
	if err != nil {
		t.Fatal(err)
	}
	_, session, err := st.CreateManagementSession(user, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Sign(auth.Claims{UserID: user.ID, Username: user.Username, Role: user.Role, SessionID: session.TokenHash})
	if err != nil {
		t.Fatal(err)
	}
	o := &observability{obsingest.New(obsingest.Stores{Logs: ls})}
	// LogsContext is wired to the production callback, not a stub: a stub
	// cannot catch the store handing the app layer an out-of-range index.
	router := api.Router(api.Deps{Store: st, AuthSigner: signer, LogsContext: o.contextLogs})
	get := func(t *testing.T, params url.Values) api.LogsWindow {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/logs/context?"+params.Encode(), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var window api.LogsWindow
		if err := json.Unmarshal(rec.Body.Bytes(), &window); err != nil {
			t.Fatal(err)
		}
		return window
	}
	params := func(id int64) url.Values {
		return url.Values{
			"instance": {"inst-a"}, "ts": {strconv.FormatInt(anchor.TimeMS, 10)},
			"id": {fmt.Sprint(id)}, "before": {"2"}, "after": {"2"},
		}
	}

	// Present anchor: 200, match_index inside the slice and pointing at it.
	window := get(t, params(anchor.ID))
	if window.MatchIndex < 0 || window.MatchIndex >= len(window.Items) {
		t.Fatalf("present anchor: match_index=%d, items=%d", window.MatchIndex, len(window.Items))
	}
	if window.Items[window.MatchIndex].ID != anchor.ID {
		t.Fatalf("present anchor: items[%d] = %+v, want id %d", window.MatchIndex, window.Items[window.MatchIndex], anchor.ID)
	}

	// Missing anchor: 200 with the neighbourhood, match_index -1. A 404 here
	// would make the console's context panel drop the surrounding lines that
	// are still real, so the smallest safe change keeps the 200 and fixes the
	// index.
	window = get(t, params(2000000000))
	if window.MatchIndex != -1 {
		t.Fatalf("missing anchor: match_index=%d, want -1", window.MatchIndex)
	}
	if len(window.Items) == 0 {
		t.Fatal("missing anchor: expected the surrounding rows to still be returned")
	}
}
