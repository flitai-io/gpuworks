package loadgen

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The client's header goes out with every request (the key still wins), and
// the class the gateway answers is kept for each request, streamed, whole or
// refused, and in its detail record.
func TestClientSendsItsHeaderAndKeepsTheAnsweredClass(t *testing.T) {
	var asked, auth []string
	shape := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.Header.Get(PriorityClassHeader))
		auth = append(auth, r.Header.Get("Authorization"))
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
			return
		}
		w.Header().Set(PriorityClassHeader, "high")
		switch shape {
		case "refused":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"asleep"}}`))
		case "stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: [DONE]\n\n"))
		default:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"a"},"finish_reason":"stop"}]}`))
		}
	}))
	defer srv.Close()
	header := http.Header{PriorityClassHeader: []string{"high"}, "Authorization": []string{"Bearer spoofed"}}
	c := Client{BaseURL: srv.URL, APIKey: "key", Header: header, ObservePriorityClass: true}
	if _, err := c.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, shape = range []string{"whole", "stream", "refused"} {
		res, _ := c.Do(context.Background(), Request{Model: "m", Stream: shape == "stream"})
		if res.Metrics.PriorityClass != "high" {
			t.Fatalf("%s: answered class = %q", shape, res.Metrics.PriorityClass)
		}
		if rec := NewDetailRecord(res); rec.PriorityClass != "high" {
			t.Fatalf("%s: detail class = %q", shape, rec.PriorityClass)
		}
	}
	for i := range asked {
		if asked[i] != "high" || auth[i] != "Bearer key" {
			t.Fatalf("request %d asked %q with %q", i, asked[i], auth[i])
		}
	}
	if len(asked) != 4 {
		t.Fatalf("requests = %d", len(asked))
	}
	// No header: nothing asked; and an endpoint that is not our gateway
	// has its header of that name ignored.
	shape = "whole"
	res, err := (Client{BaseURL: srv.URL}).Do(context.Background(), Request{Model: "m"})
	if err != nil || asked[len(asked)-1] != "" || res.Metrics.PriorityClass != "" {
		t.Fatalf("without a header asked %q, kept %q, err=%v", asked[len(asked)-1], res.Metrics.PriorityClass, err)
	}
}
