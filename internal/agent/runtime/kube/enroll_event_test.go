package kube

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi/kubefake"
)

const testAgentPod = "gpuworks-agent-abcde"

type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// refusalFixture is a fake cluster holding the agent's own pod and a
// reporter for it driven by a controllable clock.
func refusalFixture(t *testing.T) (*kubefake.Server, *RefusalReporter, *testClock, string) {
	t.Helper()
	k := kubefake.New(t, testNS, testNode)
	pod := kubeapi.Pod{Metadata: kubeapi.ObjectMeta{Name: testAgentPod}, Spec: kubeapi.PodSpec{Containers: []kubeapi.Container{{Name: "agent", Image: "gpuworks/gpuworks-agent:v1"}}}}
	if err := k.Client().Create(context.Background(), kubeapi.PodsPath(testNS), pod, nil); err != nil {
		t.Fatal(err)
	}
	clock := &testClock{t: time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)}
	r := &RefusalReporter{
		API: k.Client(), Namespace: testNS, PodName: testAgentPod, NodeName: testNode,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: clock.now,
	}
	return k, r, clock, k.Pod(testNS, testAgentPod).Metadata.UID
}

func TestRefusalEventIsAWarningOnTheAgentsOwnPod(t *testing.T) {
	k, r, _, uid := refusalFixture(t)
	if uid == "" {
		t.Fatal("fake pod has no uid")
	}
	r.Report(context.Background(), &EnrollRefusal{Reason: "node_taken", Entry: "gpu-pool-b"})

	evs := k.Events(testNS)
	if len(evs) != 1 {
		t.Fatalf("events = %+v, want exactly one", evs)
	}
	ev := evs[0]
	want := kubeapi.ObjectReference{APIVersion: "v1", Kind: "Pod", Namespace: testNS, Name: testAgentPod, UID: uid}
	if ev.Involved != want {
		t.Errorf("involvedObject = %+v, want %+v", ev.Involved, want)
	}
	if ev.Reason != "GpuworksEnrollRefused" || ev.Type != "Warning" || ev.Count != 1 {
		t.Errorf("reason/type/count = %q/%q/%d", ev.Reason, ev.Type, ev.Count)
	}
	if ev.Source.Component != "gpuworks-agent" || ev.ReportingComponent != "gpuworks-agent" || ev.Source.Host != testNode {
		t.Errorf("source = %+v reporting = %q", ev.Source, ev.ReportingComponent)
	}
	if ev.FirstTimestamp != "2026-11-13T08:00:00Z" || ev.LastTimestamp != ev.FirstTimestamp {
		t.Errorf("timestamps = %q %q", ev.FirstTimestamp, ev.LastTimestamp)
	}
	if !strings.HasPrefix(ev.Metadata.Name, testAgentPod+".") {
		t.Errorf("event name = %q", ev.Metadata.Name)
	}
	if !strings.Contains(ev.Message, `"gpu-pool-b"`) || !strings.Contains(ev.Message, "node selector") {
		t.Errorf("node_taken message = %q, want the holding entry and what to do", ev.Message)
	}
}

func TestRefusalEventMessagesSayWhyAndWhatToDo(t *testing.T) {
	for reason, want := range map[string]string{
		"unknown_token": "unknown or has been revoked",
		"":              "unknown or has been revoked", // an older server's bare refusal
		"other_cluster": "another Kubernetes cluster",
		"node_taken":    "another entry of this cluster", // no entry named
		"something_new": "reason something_new",
	} {
		msg := RefusalEventMessage(&EnrollRefusal{Reason: reason})
		if !strings.Contains(msg, want) {
			t.Errorf("message for reason %q = %q, want it to contain %q", reason, msg, want)
		}
	}
}

func TestRefusalEventIsNotRepeatedWithinTheIntervalAndRefreshedAfterIt(t *testing.T) {
	k, r, clock, _ := refusalFixture(t)
	taken := &EnrollRefusal{Reason: "node_taken", Entry: "other"}
	ctx := context.Background()

	r.Report(ctx, taken)
	for i := 0; i < 5; i++ { // the retry loop asks again every minute or so
		clock.advance(time.Minute)
		r.Report(ctx, taken)
	}
	if n := len(k.Events(testNS)); n != 1 {
		t.Fatalf("events after 5 minutes of the same refusal = %d, want 1", n)
	}
	clock.advance(5 * time.Minute) // 10 minutes since the first
	r.Report(ctx, taken)
	evs := k.Events(testNS)
	if len(evs) != 2 {
		t.Fatalf("events after the interval = %d, want 2", len(evs))
	}
	if evs[0].Metadata.Name == evs[1].Metadata.Name {
		t.Errorf("both events are named %q", evs[0].Metadata.Name)
	}
	if evs[1].FirstTimestamp != "2026-11-13T08:10:00Z" {
		t.Errorf("refreshed event timestamp = %q", evs[1].FirstTimestamp)
	}
}

func TestRefusalEventForAChangedRefusalIsWrittenAtOnce(t *testing.T) {
	k, r, clock, _ := refusalFixture(t)
	ctx := context.Background()
	// Seconds apart, well inside the interval: only the change matters.
	for _, refusal := range []*EnrollRefusal{
		{Reason: "node_taken", Entry: "a"},
		{Reason: "node_taken", Entry: "a"},
		{Reason: "node_taken", Entry: "b"}, // another entry holds it now
		{Reason: "unknown_token"},
		{Reason: "unknown_token"},
	} {
		r.Report(ctx, refusal)
		clock.advance(time.Second)
	}
	evs := k.Events(testNS)
	if len(evs) != 3 {
		t.Fatalf("events = %d, want 3 (a, b, unknown_token)", len(evs))
	}
	if !strings.Contains(evs[0].Message, `"a"`) || !strings.Contains(evs[1].Message, `"b"`) || !strings.Contains(evs[2].Message, "revoked") {
		t.Errorf("messages = %q / %q / %q", evs[0].Message, evs[1].Message, evs[2].Message)
	}
}

func TestRefusalEventIsStillWrittenWhenThePodCannotBeRead(t *testing.T) {
	k, r, _, _ := refusalFixture(t)
	r.PodName = "no-such-pod"
	r.Report(context.Background(), &EnrollRefusal{Reason: "unknown_token"})
	evs := k.Events(testNS)
	if len(evs) != 1 || evs[0].Involved.Name != "no-such-pod" || evs[0].Involved.UID != "" {
		t.Fatalf("events = %+v, want one event on the named pod without a uid", evs)
	}
}

func TestRefusalEventWriteFailuresAreOnlyThrottledNeverFatal(t *testing.T) {
	k, r, clock, _ := refusalFixture(t)
	ctx := context.Background()
	k.DenyEvents = true // a manifest from before the Role had events create
	r.Report(ctx, &EnrollRefusal{Reason: "node_taken", Entry: "x"})
	before := len(k.Requests)
	clock.advance(time.Minute)
	r.Report(ctx, &EnrollRefusal{Reason: "node_taken", Entry: "x"})
	if len(k.Requests) != before {
		t.Errorf("a forbidden write was retried within the interval: %v", k.Requests[before:])
	}
	if n := len(k.Events(testNS)); n != 0 {
		t.Fatalf("events = %d under a 403", n)
	}
	// After the Role is fixed the next interval writes it.
	k.DenyEvents = false
	clock.advance(10 * time.Minute)
	r.Report(ctx, &EnrollRefusal{Reason: "node_taken", Entry: "x"})
	if n := len(k.Events(testNS)); n != 1 {
		t.Fatalf("events after the Role allowed it = %d, want 1", n)
	}
}

func TestRefusalEventNeedsAPodToHangOn(t *testing.T) {
	k, r, _, _ := refusalFixture(t)
	r.PodName = ""
	r.Report(context.Background(), &EnrollRefusal{Reason: "unknown_token"})
	if n := len(k.Events(testNS)); n != 0 {
		t.Fatalf("events without a pod name = %d", n)
	}
}

// TestEnrollRefusalCarriesTheHoldingEntry reads the server's real 403 body
// (api.kubeEnroll's node_taken answer) and feeds it to the event.
func TestEnrollRefusalCarriesTheHoldingEntry(t *testing.T) {
	k, r, _, _ := refusalFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"node is held by another entry","code":"KUBE_ENROLL_REFUSED","params":{"reason":"node_taken","entry":"prod-pool"}}`))
	}))
	defer srv.Close()
	_, err := Enroll(context.Background(), k.Client(), srv.Client(), srv.URL, "tok", testNode, t.TempDir(), true)
	var refusal *EnrollRefusal
	if !errors.As(err, &refusal) || refusal.Reason != "node_taken" || refusal.Entry != "prod-pool" {
		t.Fatalf("Enroll = %v, want an EnrollRefusal for node_taken naming prod-pool", err)
	}
	r.Report(context.Background(), refusal)
	if evs := k.Events(testNS); len(evs) != 1 || !strings.Contains(evs[0].Message, `"prod-pool"`) {
		t.Fatalf("events = %+v", evs)
	}
}
