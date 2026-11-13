// strata-health-fixture reproduces Strata 0.1.38's HTTP readiness during an
// internal child restart; it is not an inference implementation.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
)

func main() {
	port := flag.Int("port", 8000, "HTTP port")
	flag.Parse()
	var loaded atomic.Bool
	loaded.Store(true)
	var active, loads atomic.Int32
	ready := make(chan struct{})
	var once sync.Once
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		// The peer owns the response; a write failure means it disconnected.
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"service": "strata", "status": "ok", "loaded": loaded.Load(), "api_key": false})
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		data := []map[string]any{}
		if loaded.Load() {
			data = append(data, map[string]any{"id": "model"})
		}
		reply(w, map[string]any{"object": "list", "data": data})
	})
	mux.HandleFunc("/load", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Origin") != "" {
			w.WriteHeader(415)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		loads.Add(1)
		// The real loader does not stop when its HTTP client disconnects.
		<-ready
		loaded.Store(true)
		reply(w, map[string]any{"status": "loaded"})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string            `json:"model"`
			Messages []json.RawMessage `json:"messages"`
		}
		if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&body) != nil || body.Model != "model" || len(body.Messages) == 0 {
			w.WriteHeader(400)
			return
		}
		active.Add(1)
		defer active.Add(-1)
		// Simulate an old request whose Python HTTP handler remains blocked
		// after its engine child dies. The gateway must end this wait itself.
		<-r.Context().Done()
	})
	mux.HandleFunc("/__test__/state", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"active": active.Load(), "loads": loads.Load()})
	})
	mux.HandleFunc("/__test__/crash", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		loaded.Store(false)
		reply(w, map[string]any{"status": "child exited"})
	})
	mux.HandleFunc("/__test__/ready", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		once.Do(func() { close(ready) })
		reply(w, map[string]any{"status": "released"})
	})
	if err := http.ListenAndServe(fmt.Sprintf(":%d", *port), mux); err != nil {
		panic(err)
	}
}
