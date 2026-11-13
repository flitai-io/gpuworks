package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestOutboundPolicyCheckRejectsAnyPermanentlyBlockedDNSAnswer(t *testing.T) {
	queries := outboundDNSFixture(t, map[string][]netip.Addr{
		"public.gwks.test":         {netip.MustParseAddr("8.8.8.8")},
		"private.gwks.test":        {netip.MustParseAddr("10.1.2.3")},
		"mixed-private.gwks.test":  {netip.MustParseAddr("10.1.2.3"), netip.MustParseAddr("169.254.169.254")},
		"mixed-reserved.gwks.test": {netip.MustParseAddr("169.254.169.254"), netip.MustParseAddr("10.1.2.3")},
	})
	router, admin, _, _ := outboundCheckRouter(t)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	for _, tc := range []struct{ host, status string }{
		{"public.gwks.test", "allowed"}, {"private.gwks.test", "needs_allow"},
		{"mixed-private.gwks.test", "blocked"}, {"mixed-reserved.gwks.test", "blocked"},
	} {
		t.Run(tc.host, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/server/outbound-policy/check", strings.NewReader(`{"hosts":["`+tc.host+`"]}`))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+admin)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			want := `{"items":[{"host":"` + tc.host + `","status":"` + tc.status + `"}],"total":1}`
			if response.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != want {
				t.Fatalf("check status=%d body=%s, want %s", response.StatusCode, body, want)
			}
		})
	}
	if queries.Load() == 0 {
		t.Fatal("DNS fixture did not receive the product's queries")
	}
}

func TestOutboundPolicyCheckBoundsAnUnresponsiveDNSBatch(t *testing.T) {
	queries := outboundDNSFixture(t, nil)
	router, admin, _, _ := outboundCheckRouter(t)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/server/outbound-policy/check", strings.NewReader(`{"hosts":["silent.gwks.test","silent-two.gwks.test"]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+admin)
	client := server.Client()
	client.Timeout = 20 * time.Second
	started := time.Now()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"items":[{"host":"silent.gwks.test","status":"unresolved"},{"host":"silent-two.gwks.test","status":"unresolved"}],"total":2}`
	if response.StatusCode != 200 || strings.TrimSpace(string(body)) != want || time.Since(started) > 15*time.Second || queries.Load() == 0 {
		t.Fatalf("DNS batch status=%d body=%s elapsed=%s questions=%d", response.StatusCode, body, time.Since(started), queries.Load())
	}
}

func TestOutboundPolicyCheckCancelsDNSWhenItsHTTPRequestIsCanceled(t *testing.T) {
	queries := outboundDNSFixture(t, nil)
	router, admin, _, _ := outboundCheckRouter(t)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/v1/server/outbound-policy/check", strings.NewReader(`{"hosts":["silent.gwks.test"]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+admin)
	done := make(chan error, 1)
	go func() {
		response, err := server.Client().Do(request)
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for queries.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("HTTP check did not reach the DNS fixture")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled HTTP request: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled HTTP check did not return")
	}
	started := time.Now()
	server.Close()
	if time.Since(started) > 5*time.Second {
		t.Fatal("canceled DNS check kept its HTTP handler alive")
	}
}
