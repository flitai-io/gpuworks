package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/priority"
)

func allOnline(string) bool { return true }

// counts reads an instance's in-flight counts under the balancer's lock.
func (b *balancer) counts(uuid string) inflightCounts {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.states[uuid]
	if !ok {
		return inflightCounts{}
	}
	return inflightCounts{total: s.inflight.total, byValue: append([]priorityCount(nil), s.inflight.byValue...)}
}

func TestInflightCountsByPriority(t *testing.T) {
	b := newBalancer(nil)
	replica := []candidate{{instanceUUID: "r", nodeUUID: "n", slots: 8}}
	normal, batch, high := admission{priority: 0}, admission{priority: -40}, admission{priority: 20}
	var leases []*lease
	for _, adm := range []admission{normal, batch, normal, batch, normal} {
		l, _ := b.pick(replica, nil, allOnline, adm)
		if l == nil || l.priority != adm.priority || l.c.slots != 8 {
			t.Fatalf("lease = %+v, want one for priority %d carrying the replica's slots", l, adm.priority)
		}
		leases = append(leases, l)
	}
	counts := b.counts("r")
	if counts.total != 5 || b.inflight("r") != 5 {
		t.Fatalf("total = %d (inflight %d), want 5", counts.total, b.inflight("r"))
	}
	// What a request of each class waits behind: the leases at its value
	// or above, since the engine runs lower values after it.
	for _, want := range []struct {
		adm    admission
		behind int
	}{{normal, 3}, {high, 0}, {batch, 5}} {
		if got := counts.atLeast(want.adm.priority); got != want.behind {
			t.Fatalf("a request of priority %d waits behind %d, want %d", want.adm.priority, got, want.behind)
		}
	}

	// A lease gives back its own value, whatever else the replica holds,
	// and once only.
	leases[1].release(outcomeOK)
	leases[1].release(outcomeOK)
	if counts := b.counts("r"); counts.total != 4 || counts.atLeast(normal.priority) != 3 || counts.atLeast(batch.priority) != 4 {
		t.Fatalf("after one batch release: %+v", counts)
	}
	for _, l := range leases {
		l.release(outcomeOK)
	}
	if counts := b.counts("r"); counts.total != 0 || len(counts.byValue) != 0 {
		t.Fatalf("after every release: %+v, want nothing left", counts)
	}
	b.gc(map[string]bool{})
	if _, kept := b.states["r"]; kept {
		t.Fatal("an idle, unrouted replica's state was kept")
	}
}

// A request's priority is counted, never scored: every policy picks the
// same replicas for requests of mixed classes as for neutral ones.
func TestPriorityDoesNotChangeTheChoice(t *testing.T) {
	candidates := []candidate{
		{instanceUUID: "a", nodeUUID: "n"}, {instanceUUID: "b", nodeUUID: "n"}, {instanceUUID: "c", nodeUUID: "n"},
	}
	classes := []admission{{priority: 40}, {priority: -40}, {}, {priority: 20}, {priority: -20}}
	for _, policy := range []string{deploy.LeastRequest, deploy.Failover, deploy.PrefixAffinity} {
		t.Run(policy, func(t *testing.T) {
			neutral, mixed := newBalancer(nil), newBalancer(nil)
			for _, b := range []*balancer{neutral, mixed} {
				calls := 0
				b.randN = func(n int) int { calls++; return calls % n }
			}
			var held []*lease
			for i := 0; i < 40; i++ {
				affinity := ""
				if policy == deploy.PrefixAffinity {
					affinity = []string{"k1", "k2", "k3"}[i%3]
				}
				want, _ := neutral.pickBy(policy, candidates, affinity, nil, allOnline, admission{})
				got, _ := mixed.pickBy(policy, candidates, affinity, nil, allOnline, classes[i%len(classes)])
				if want.c.instanceUUID != got.c.instanceUUID {
					t.Fatalf("pick %d: %s with classes, %s without", i, got.c.instanceUUID, want.c.instanceUUID)
				}
				// Every third pair is given back, so loads differ by replica.
				if i%3 == 0 {
					want.release(outcomeOK)
					got.release(outcomeOK)
				} else {
					held = append(held, want, got)
				}
			}
			for _, l := range held {
				l.release(outcomeOK)
			}
			for _, uuid := range []string{"a", "b", "c"} {
				if counts := mixed.counts(uuid); counts.total != 0 || len(counts.byValue) != 0 {
					t.Fatalf("%s kept %+v", uuid, counts)
				}
			}
		})
	}
}

// A replica switched after a connection failure gives the request's lease
// back under the value it was taken with, and the next replica is leased for
// the same class.
func TestReplicaSwitchKeepsTheRequestPriority(t *testing.T) {
	b := newBalancer(nil)
	candidates := []candidate{{instanceUUID: "primary", nodeUUID: "n", modelName: "m"}, {instanceUUID: "backup", nodeUUID: "n", modelName: "m"}}
	high := admission{priority: 20}
	first, _ := b.pickFailover(candidates, nil, allOnline, high)
	attempt := &routeAttempt{candidates: candidates, current: first, tried: map[string]bool{"primary": true}, attempts: 1, modelName: "m", policy: deploy.Failover, started: time.Now(), admission: high}
	calls := 0
	transport := &retryTransport{
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			if calls++; calls == 1 {
				return nil, &tunnelOpenError{err: errors.New("dial failed")}
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
		}),
		balancer: b,
		hub:      readinessTestHub{online: map[string]bool{"n": true}},
	}
	req := httptest.NewRequest(http.MethodPost, "http://proxy/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(`{"model":"m"}`)), nil }
	resp, err := transport.RoundTrip(req.WithContext(context.WithValue(req.Context(), attemptKey, attempt)))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if counts := b.counts("primary"); counts.total != 0 || len(counts.byValue) != 0 {
		t.Fatalf("the failed replica kept %+v", counts)
	}
	if attempt.current == nil || attempt.current.c.instanceUUID != "backup" || attempt.current.priority != high.priority {
		t.Fatalf("retried lease = %+v, want backup at priority %d", attempt.current, high.priority)
	}
	if counts := b.counts("backup"); counts.total != 1 || counts.atLeast(high.priority) != 1 || counts.atLeast(high.priority+1) != 0 {
		t.Fatalf("backup counts = %+v", counts)
	}
	attempt.current.release(outcomeOK)
	if counts := b.counts("backup"); counts.total != 0 || len(counts.byValue) != 0 {
		t.Fatalf("backup kept %+v", counts)
	}
}

func TestAdmissionIsTheDecidedClass(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if adm := admissionFrom(req.Context()); adm != (admission{}) {
		t.Fatalf("undecided = %+v, want neutral", adm)
	}
	decided := context.WithValue(req.Context(), priorityDecisionKey{}, priority.Decision{Class: priority.Class{Name: "batch", Value: -40}})
	if adm := admissionFrom(decided); adm.priority != -40 {
		t.Fatalf("decided batch = %+v", adm)
	}
}
