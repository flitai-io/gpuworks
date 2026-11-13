package handler

import (
	_ "embed"
	"net/http"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// The captured engine schema preserves real required fields, refs and defaults.
//
//go:embed testdata/openapi/vllm-omni-cuda12.9-vllm0.25.1.json
var openAPIDocument []byte

func registerDiscovery(mux *http.ServeMux, opts Options) {
	mux.HandleFunc("/v1/audio/voices", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		writeJSON(w, map[string]any{"voices": append([]string{}, opts.Voices...), "uploaded_voices": []string{}})
	})
	mux.HandleFunc("/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(discoveryDocument)
	})
	mux.HandleFunc("/v1/realtime", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// This handler owns the socket; a disconnected peer has no close result to consume.
		defer func() { _ = conn.CloseNow() }()
		if err := wsjson.Write(r.Context(), conn, map[string]any{"type": "session.created", "session": map[string]any{"id": "fake-session", "model": opts.Model}}); err != nil {
			return
		}
		for {
			var event map[string]any
			if err := wsjson.Read(r.Context(), conn, &event); err != nil {
				return
			}
			if event["type"] != "session.update" {
				if err := wsjson.Write(r.Context(), conn, map[string]any{"type": "error", "error": map[string]string{"type": "invalid_request_error", "message": "unsupported event"}}); err != nil {
					return
				}
				continue
			}
			if err := wsjson.Write(r.Context(), conn, map[string]any{"type": "session.updated", "session": event["session"]}); err != nil {
				return
			}
		}
	})
}
