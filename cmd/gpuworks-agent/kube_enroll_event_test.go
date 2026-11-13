package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentapp "gpuworks.ai/gpuworks/internal/agent/app"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi/kubefake"
)

// refusingServer refuses enrollment as node_taken for the first refusals
// calls, then enrolls.
func refusingServer(t *testing.T, refusals int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= refusals {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"node held","code":"KUBE_ENROLL_REFUSED","params":{"reason":"node_taken","entry":"pool-a"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"node_uuid":"n-1","node_token":"tok-1","idle_return_seconds":-1}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func refusalCluster(t *testing.T) *kubefake.Server {
	t.Helper()
	t.Setenv("GPUWORKS_KUBE_ENROLL_TOKEN", "tok")
	t.Setenv("GPUWORKS_KUBE_NODE_NAME", "gpu-1")
	t.Setenv("GPUWORKS_KUBE_NAMESPACE", "gpuworks-system")
	t.Setenv("GPUWORKS_KUBE_POD_NAME", "gpuworks-agent-x1")
	k := kubefake.New(t, "gpuworks-system", "gpu-1")
	pod := kubeapi.Pod{Metadata: kubeapi.ObjectMeta{Name: "gpuworks-agent-x1"}, Spec: kubeapi.PodSpec{Containers: []kubeapi.Container{{Name: "agent", Image: "gpuworks/gpuworks-agent:v1"}}}}
	if err := k.Client().Create(context.Background(), kubeapi.PodsPath("gpuworks-system"), pod, nil); err != nil {
		t.Fatal(err)
	}
	return k
}

func fastRetries(t *testing.T, interval time.Duration) {
	t.Helper()
	oldDelay, oldInterval := kubeEnrollRetryDelay, kubeEnrollRefusalEventInterval
	kubeEnrollRetryDelay, kubeEnrollRefusalEventInterval = 5*time.Millisecond, interval
	t.Cleanup(func() { kubeEnrollRetryDelay, kubeEnrollRefusalEventInterval = oldDelay, oldInterval })
}

// TestEnrollRefusalIsAnEventOnTheAgentsPodOncePerRefusal: the retry loop
// asks several times while refused, and the pod's events show the refusal
// once, not once per retry; the node enrolls when the refusal lifts.
func TestEnrollRefusalIsAnEventOnTheAgentsPodOncePerRefusal(t *testing.T) {
	k := refusalCluster(t)
	fastRetries(t, time.Hour)
	srv, calls := refusingServer(t, 4)

	id, err := enrollKubeNodeWith(context.Background(), discardLog(), agentapp.Config{ServerURL: srv.URL, StateDir: t.TempDir()}, k.Client())
	if err != nil || id.NodeUUID != "n-1" {
		t.Fatalf("enrollKubeNodeWith = %+v, %v", id, err)
	}
	if calls.Load() != 5 {
		t.Fatalf("server saw %d enrollments, want 4 refusals and 1 success", calls.Load())
	}
	evs := k.Events("gpuworks-system")
	if len(evs) != 1 {
		t.Fatalf("events after 4 refused retries = %d, want 1: %+v", len(evs), evs)
	}
	ev := evs[0]
	pod := k.Pod("gpuworks-system", "gpuworks-agent-x1")
	if ev.Reason != kube.EnrollRefusedReason || ev.Type != "Warning" || ev.Involved.Kind != "Pod" ||
		ev.Involved.Name != "gpuworks-agent-x1" || ev.Involved.Namespace != "gpuworks-system" || ev.Involved.UID != pod.Metadata.UID {
		t.Errorf("event = %+v, want a Warning GpuworksEnrollRefused on the agent pod %s", ev, pod.Metadata.UID)
	}
	if !strings.Contains(ev.Message, `"pool-a"`) {
		t.Errorf("message = %q, want the entry holding the node", ev.Message)
	}
}

// TestEnrollRefusalEventIsRefreshedWhileTheRefusalPersists: a refusal that
// outlasts the interval is written again.
func TestEnrollRefusalEventIsRefreshedWhileTheRefusalPersists(t *testing.T) {
	k := refusalCluster(t)
	fastRetries(t, 20*time.Millisecond)
	srv, _ := refusingServer(t, 1<<30)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	if _, err := enrollKubeNodeWith(ctx, discardLog(), agentapp.Config{ServerURL: srv.URL, StateDir: t.TempDir()}, k.Client()); err == nil {
		t.Fatal("enrolled while refused")
	}
	if n := len(k.Events("gpuworks-system")); n < 2 {
		t.Fatalf("events after a refusal outlasting the interval = %d, want a refresh", n)
	}
}

// TestEnrollRefusalEventForbiddenDoesNotBreakTheRetryLoop: a Role from
// before events create (403) leaves enrollment retrying as before.
func TestEnrollRefusalEventForbiddenDoesNotBreakTheRetryLoop(t *testing.T) {
	k := refusalCluster(t)
	k.DenyEvents = true
	fastRetries(t, time.Hour)
	srv, calls := refusingServer(t, 3)

	id, err := enrollKubeNodeWith(context.Background(), discardLog(), agentapp.Config{ServerURL: srv.URL, StateDir: t.TempDir()}, k.Client())
	if err != nil || id.NodeUUID != "n-1" || calls.Load() != 4 {
		t.Fatalf("enrollKubeNodeWith = %+v, %v after %d server calls, want enrolled on the 4th", id, err, calls.Load())
	}
	if n := len(k.Events("gpuworks-system")); n != 0 {
		t.Fatalf("events = %d though creating them is forbidden", n)
	}
}
