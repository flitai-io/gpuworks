package api

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestRunPodUpstreamErrorDoesNotExposeAPIKey(t *testing.T) {
	const apiKey = "synthetic-runpod-api-key-never-expose"
	var failList atomic.Bool
	var keyCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+apiKey {
			t.Errorf("missing expected fixture authentication")
		}
		switch r.URL.Path {
		case "/pods":
			if !failList.Load() {
				fmt.Fprint(w, `[]`)
				return
			}
			w.WriteHeader(http.StatusForbidden)
		case "/account/ssh-keys":
			keyCalls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
		default:
			t.Errorf("unexpected external request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
		fmt.Fprintf(w, `{"error":"vendor diagnostic echoes Authorization: Bearer %s"}`, apiKey)
	}))
	defer upstream.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "provider.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, err := auth.NewSecretbox("provider-error-fixture")
	if err != nil {
		t.Fatal(err)
	}
	service := provider.NewService(st, box)
	if _, err := st.SetPlatformSSHKey("synthetic-platform-private-key", "ssh-ed25519 AAAA fixture", "SHA256:fixture"); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	account, err := service.Add(t.Context(), "runpod", "fixture", provider.Credentials{Endpoint: upstream.URL, APIKey: apiKey})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("best-effort-key-warning", func(t *testing.T) {
		if keyCalls.Load() != 1 {
			t.Fatalf("SSH key calls = %d", keyCalls.Load())
		}
		output := logs.String()
		if !strings.Contains(output, "authorize platform ssh key") || !strings.Contains(output, "status 400") {
			t.Fatalf("warning lost operation/status: %s", output)
		}
		if strings.Contains(output, apiKey) || strings.Contains(output, "vendor diagnostic") {
			t.Fatalf("untrusted upstream response reached warning: %s", output)
		}
	})
	failList.Store(true)
	t.Run("sync-424-wire", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/"+strconv.FormatUint(uint64(account.ID), 10)+"/sync", nil)
		route := chi.NewRouteContext()
		route.URLParams.Add("id", strconv.FormatUint(uint64(account.ID), 10))
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
		response := httptest.NewRecorder()
		handler := apiHandlers{d: Deps{Store: st, Providers: service}}
		handler.syncProvider(response, req)
		if response.Code != http.StatusFailedDependency {
			t.Fatalf("sync HTTP status = %d", response.Code)
		}
		if !strings.Contains(response.Body.String(), "list pods: status 403") {
			t.Fatalf("sync lost operation/status: %s", response.Body.String())
		}
		if strings.Contains(response.Body.String(), apiKey) || strings.Contains(response.Body.String(), "vendor diagnostic") {
			t.Fatalf("untrusted upstream response reached API: %s", response.Body.String())
		}
	})
}
