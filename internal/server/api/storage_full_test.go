package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	_ "github.com/glebarez/go-sqlite"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// realFullDiskError is the error SQLite answers a write with when there is no
// room: a page cap on a real database file stands in for the disk.
func realFullDiskError(t *testing.T) error {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "full.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("CREATE TABLE t (x BLOB)"); err != nil {
		t.Fatal(err)
	}
	var pages int
	if err := db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA max_page_count = %d", pages)); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("INSERT INTO t VALUES (randomblob(1048576))")
	if err == nil || !strings.Contains(err.Error(), "database or disk is full") {
		t.Fatalf("insert past the cap = %v, want SQLite's full-disk error", err)
	}
	return fmt.Errorf("create deployment: %w", err)
}

func TestWriteErrAnswersAFullServerDiskWithAProductError(t *testing.T) {
	full := realFullDiskError(t)
	rec := httptest.NewRecorder()
	writeErr(rec, http.StatusInternalServerError, full)

	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507", rec.Code)
	}
	if got := rec.Header().Get(usageErrorHeader); got != ErrorServerStorageFull {
		t.Fatalf("%s = %q", usageErrorHeader, got)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	if body["code"] != "SERVER_STORAGE_FULL" || len(body) != 2 {
		t.Fatalf("body = %s, want exactly error and code", rec.Body.String())
	}
	want := "the server's disk is full; free space on the server host and retry: create deployment: database or disk is full (13)"
	if body["error"] != want {
		t.Fatalf("error = %q\nwant    %q", body["error"], want)
	}
}

func TestWriteErrLeavesOtherErrorsAsTheyWere(t *testing.T) {
	product := messages.NewProductError("NODE_OFFLINE", "node is offline", map[string]any{"node": "n1"})
	cases := []struct {
		name   string
		status int
		err    error
		want   map[string]any
	}{
		{"plain 500", http.StatusInternalServerError, errors.New("boom"), map[string]any{"error": "boom", "code": "INTERNAL_ERROR"}},
		{"product error keeps its code", http.StatusConflict, product, map[string]any{"error": "node is offline", "code": "NODE_OFFLINE", "params": map[string]any{"node": "n1"}}},
		{"look-alike text is not a full disk", http.StatusInternalServerError, errors.New("database or disk is full (13)"), map[string]any{"error": "database or disk is full (13)", "code": "INTERNAL_ERROR"}},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		writeErr(rec, tc.status, tc.err)
		if rec.Code != tc.status {
			t.Errorf("%s: status = %d, want %d", tc.name, rec.Code, tc.status)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: body %q: %v", tc.name, rec.Body.String(), err)
		}
		if !reflect.DeepEqual(body, tc.want) {
			t.Errorf("%s: body = %v, want %v", tc.name, body, tc.want)
		}
	}
}

// dataDirRecorder is a response writer that knows the server's data
// directory, as the server's access log does.
type dataDirRecorder struct {
	*httptest.ResponseRecorder
	dir string
}

func (r dataDirRecorder) ServerDataDir() string { return r.dir }

func TestWriteErrNamesTheFullDataDirectoryAndItsRoom(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("statfs is reported on Linux and macOS")
	}
	dir := t.TempDir()
	rec := dataDirRecorder{httptest.NewRecorder(), dir}
	writeErr(rec, http.StatusServiceUnavailable, realFullDiskError(t))

	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507", rec.Code)
	}
	var body struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	total, _ := body.Params["total_bytes"].(float64)
	avail, hasAvail := body.Params["available_bytes"].(float64)
	if body.Code != "SERVER_STORAGE_FULL" || body.Params["path"] != dir || total <= 0 || !hasAvail || avail < 0 || avail > total || len(body.Params) != 3 {
		t.Fatalf("body = %s, want the data directory with its filesystem's total and available bytes", rec.Body.String())
	}
}
