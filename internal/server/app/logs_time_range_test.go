package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/logstore"
	"gpuworks.ai/gpuworks/internal/server/obsingest"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Exercise the console's wire requests through the router, production
// callbacks and real SQLite. A canned search response cannot catch a store
// silently changing the time window after the API has parsed it.
func TestLogsTimeRangesWithRealStore(t *testing.T) {
	dir := t.TempDir()
	ls, err := logstore.OpenSQLite(filepath.Join(dir, "logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ls.Close() })
	now := time.Now().UTC().Truncate(time.Hour)
	var entries []logstore.Entry
	for _, item := range []struct {
		age    time.Duration
		line   string
		source string
	}{
		{40 * 24 * time.Hour, "Traceback expired", logstore.SourceInstance},
		{10 * 24 * time.Hour, "Traceback old", logstore.SourceInstance},
		{3 * 24 * time.Hour, "Traceback week", logstore.SourceInstance},
		{time.Hour, "Traceback recent", logstore.SourceInstance},
		{time.Hour, "healthy", logstore.SourceInstance},
		{time.Hour, "Traceback agent", logstore.SourceAgent},
	} {
		entries = append(entries, logstore.Entry{Instance: "inst-a", Node: "node-a", TimeMS: now.Add(-item.age).UnixMilli(), Source: item.source, Level: logstore.LevelError, Line: item.line})
	}
	if err := ls.Append(entries); err != nil {
		t.Fatal(err)
	}
	if err := ls.Retain(context.Background(), now.Add(-30*24*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner(filepath.Join(dir, "auth-secret"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "management.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_, _, _, err = st.EnsureAdmin("admin", "gpuworks-test-password-2026")
	if err != nil {
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
	router := api.Router(api.Deps{Store: st, AuthSigner: signer, LogsSearch: o.searchLogs, LogsHistogram: o.histogramLogs})
	get := func(t *testing.T, path string, params url.Values, out any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path+"?"+params.Encode(), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", path, rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
	ms := func(age time.Duration) string { return strconv.FormatInt(now.Add(-age).UnixMilli(), 10) }
	for _, tc := range []struct {
		name, query string
		bounds      url.Values
		want        []string
	}{
		{"all", "Traceback", url.Values{}, []string{"Traceback recent", "Traceback week", "Traceback old"}},
		{"seven-days", "Traceback", url.Values{"from": {ms(7 * 24 * time.Hour)}}, []string{"Traceback recent", "Traceback week"}},
		{"default-window", "Traceback", url.Values{"from": {ms(24 * time.Hour)}}, []string{"Traceback recent"}},
		{"upper-bound-only", "Traceback", url.Values{"to": {ms(2 * 24 * time.Hour)}}, []string{"Traceback week", "Traceback old"}},
		{"historical-window", "Traceback", url.Values{"from": {ms(4 * 24 * time.Hour)}, "to": {ms(2 * 24 * time.Hour)}}, []string{"Traceback week"}},
		{"all-sources", "Traceback", url.Values{"source": {""}}, []string{"Traceback agent", "Traceback recent", "Traceback week", "Traceback old"}},
		{"all-without-keyword", "", url.Values{}, []string{"healthy", "Traceback recent", "Traceback week", "Traceback old"}},
		{"no-match", "absent", url.Values{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := url.Values{"q": {tc.query}, "source": {"instance"}, "limit": {"1"}, "with_total": {"1"}}
			for key, values := range tc.bounds {
				params[key] = values
			}
			var page api.LogsPage
			get(t, "/api/v1/logs/search", params, &page)
			if page.Total != int64(len(tc.want)) || page.TotalCapped || page.Truncated != (len(tc.want) > 1) {
				t.Fatalf("unexpected first page: %+v; want total %d", page, len(tc.want))
			}
			var buckets dto.List[api.LogsBucket]
			params.Del("with_total")
			params.Del("limit")
			params.Set("bucket", "10800000")
			get(t, "/api/v1/logs/histogram", params, &buckets)
			if buckets.Items == nil || buckets.Total != len(buckets.Items) {
				t.Fatalf("histogram envelope = %+v", buckets)
			}
			var histogramTotal int64
			for _, bucket := range buckets.Items {
				for _, n := range bucket.Counts {
					histogramTotal += n
				}
			}
			if histogramTotal != page.Total {
				t.Fatalf("histogram total=%d, search total=%d", histogramTotal, page.Total)
			}
			params.Set("limit", "1")
			params.Del("bucket")
			var got []string
			for len(page.Items) > 0 {
				if len(page.Items) != 1 || len(got) >= len(tc.want) {
					t.Fatalf("unexpected page: %+v", page)
				}
				entry := page.Items[0]
				got = append(got, entry.Line)
				params.Set("cursor_ts", strconv.FormatInt(entry.TimeMS, 10))
				params.Set("cursor_id", strconv.FormatInt(entry.ID, 10))
				get(t, "/api/v1/logs/search", params, &page)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("paginated lines=%v, want %v", got, tc.want)
			}
		})
	}
}
