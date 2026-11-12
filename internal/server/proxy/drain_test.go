package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func newFront(t *testing.T, p *Proxy) *httptest.Server {
	t.Helper()
	front := httptest.NewServer(p.Handler())
	t.Cleanup(front.Close)
	return front
}

// postChatNoCleanup is postChat for a goroutine: it reports its error rather
// than failing the test, and closes the response itself.
func postChatNoCleanup(ctx context.Context, url, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/chat/completions", strings.NewReader(`{"model":"omni"}`))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// A two-replica deployment one of whose replicas drains: a new request goes
// to the other replica, never to the draining one, from the store's rows and
// from the route snapshot alike; with both draining it is held until a drain
// ends. InstancesInFlight counts each replica's own requests.
func TestADrainingReplicaLeavesItsSiblingServing(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	p, _, key, depID, closeUpstream := newAccountingProxyFor(t, upstream, &store.Deployment{Engine: "vllm", Sleepable: true, Replicas: 2})
	t.Cleanup(closeUpstream)
	st := p.store
	first, err := st.InstanceByUUID("omni-instance")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "omni-2", DeploymentID: depID, NodeID: first.NodeID, State: proto.InstRunning, APIPort: 30001, Ports: "[30001]"}); err != nil {
		t.Fatal(err)
	}
	front := newFront(t, p)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		once.Do(func() { close(release) })
	})
	both := []string{"omni-instance", "omni-2"}

	// send starts a request and waits until the gateway has leased it to a
	// replica; the upstream holds it there until release.
	send := func(want string) {
		t.Helper()
		before := p.InstancesInFlight([]string{want})
		go func() { _ = postChatNoCleanup(ctx, front.URL, key) }()
		deadline := time.Now().Add(10 * time.Second)
		for p.InstancesInFlight([]string{want}) == before {
			if time.Now().After(deadline) {
				t.Fatalf("no request reached %s: in flight %d/%d", want, p.InstancesInFlight([]string{"omni-instance"}), p.InstancesInFlight([]string{"omni-2"}))
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	p.SetDraining(depID, []string{"omni-instance"}, true)
	// From the rows (no snapshot yet), then from the snapshot.
	send("omni-2")
	p.routes.rebuild()
	send("omni-2")
	send("omni-2")
	if got := p.InstancesInFlight([]string{"omni-instance"}); got != 0 {
		t.Fatalf("the draining replica took %d new requests", got)
	}
	if got := p.InstancesInFlight(both); got != 3 {
		t.Fatalf("in flight on both = %d, want 3", got)
	}

	// Both draining: a new request is held, not routed.
	p.SetWakeDemand(func(uint) {})
	p.SetDraining(depID, []string{"omni-2"}, true)
	go func() { _ = postChatNoCleanup(ctx, front.URL, key) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if n, _ := p.HeldRequests(depID); n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a request for a deployment draining whole was not held")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := p.InstancesInFlight(both); got != 3 {
		t.Fatalf("a held request was routed: in flight %d", got)
	}
	// The first replica's drain ends: the held request goes to it.
	p.SetDraining(depID, []string{"omni-instance"}, false)
	deadline = time.Now().Add(10 * time.Second)
	for p.InstancesInFlight([]string{"omni-instance"}) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the held request did not reach the replica whose drain ended: in flight %d", p.InstancesInFlight([]string{"omni-instance"}))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if p.hold.drainingOf(depID)["omni-2"] != true || p.hold.drainingOf(depID)["omni-instance"] {
		t.Fatalf("draining = %v, want omni-2 only", p.hold.drainingOf(depID))
	}
	p.SetDraining(depID, []string{"omni-2"}, false)
	if p.hold.drainingOf(depID) != nil {
		t.Fatal("a deployment with no replica draining still reads draining")
	}
	once.Do(func() { close(release) })
	deadline = time.Now().Add(10 * time.Second)
	for p.InstancesInFlight(both) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("requests still in flight: %d", p.InstancesInFlight(both))
		}
		time.Sleep(5 * time.Millisecond)
	}
}
