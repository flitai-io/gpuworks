package api

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestManagementSessionRevocationClosesSSE(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared-bus", true: "independent-process-store"}[external], func(t *testing.T) {
			st, _, router := newSessionRouter(t)
			cookie, _ := sessionLogin(t, router, "gpuworks-test-password-2026")
			server := httptest.NewServer(router)
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
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
			reader := bufio.NewReader(response.Body)
			line, err := reader.ReadString('\n')
			if err != nil || line != "event: node\n" {
				t.Fatalf("initial SSE event=%q err=%v", line, err)
			}
			writer := st
			if external {
				var databases []struct{ Name, File string }
				if err := st.DB().Raw("PRAGMA database_list").Scan(&databases).Error; err != nil {
					t.Fatal(err)
				}
				for _, db := range databases {
					if db.Name == "main" {
						writer, err = store.Open(db.File, bus.New())
						break
					}
				}
				if err != nil || writer == st {
					t.Fatalf("open independent store: %v", err)
				}
				defer writer.Close()
			}
			if err := writer.SetUserPassword("admin", "gpuworks-new-test-password-2026"); err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, reader); err != nil {
				t.Fatalf("revoked SSE did not close before deadline: %v", err)
			}
			if rec := sessionRequest(router, http.MethodGet, "/api/v1/events", cookie, ""); rec.Code != http.StatusUnauthorized {
				t.Fatalf("revoked SSE reconnected: %d", rec.Code)
			}
		})
	}
}

// A revoked platform token must cut its stream before the next change is
// forwarded, not on the periodic re-check: the re-check is pushed out of
// reach so only the revocation itself can close the stream.
func TestPlatformTokenRevocationClosesSSEBeforeNextEvent(t *testing.T) {
	old := streamAuthRecheck
	streamAuthRecheck = time.Hour
	defer func() { streamAuthRecheck = old }()

	st, _, router := newSessionRouter(t)
	var admin struct{ ID uint }
	if err := st.DB().Raw("SELECT id FROM users WHERE username = ?", "admin").Scan(&admin).Error; err != nil || admin.ID == 0 {
		t.Fatalf("admin id=%d err=%v", admin.ID, err)
	}
	full, row, err := st.CreatePlatformToken(admin.ID, "stream", store.PlatformTokenRead, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+full)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("SSE status=%d", response.StatusCode)
	}
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "event: node\n" {
		t.Fatalf("initial SSE event=%q err=%v", line, err)
	}
	if line, err := reader.ReadString('\n'); err != nil || !strings.Contains(line, `"connected"`) {
		t.Fatalf("initial SSE data=%q err=%v", line, err)
	}

	if _, err := st.DeletePlatformToken(admin.ID, row.ID); err != nil {
		t.Fatal(err)
	}
	st.Bus.PublishChange("node", "after-revoke", "registered")
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("revoked SSE did not close before deadline: %v", err)
	}
	if strings.Contains(string(rest), "after-revoke") {
		t.Fatalf("revoked token's stream forwarded a later event: %q", rest)
	}
	if rec := sessionRequest(router, http.MethodGet, "/api/v1/events", nil, full); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token reconnected: %d", rec.Code)
	}
}
