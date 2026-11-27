package kubeapply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/kubemanifest"
)

// rolloutAPI answers the reads AgentRollout makes as the API server does: a
// 404 Status for a DaemonSet that is not there, pod lists only by the
// label selector, events only by a field selector it understands (kind,
// name and reason, all three), anything else refused.
type rolloutAPI struct {
	mu       sync.Mutex
	ds       string            // the DaemonSet's JSON, "" for none
	pods     string            // the pod list's JSON
	events   map[string]string // "kind/name/reason" -> event list JSON
	requests []string
	forbid   string // a path refused with 403
}

func (f *rolloutAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	if r.Method != http.MethodGet {
		status(w, 405, "MethodNotAllowed", "")
		return
	}
	if f.forbid != "" && r.URL.Path == f.forbid {
		status(w, 403, "Forbidden", `pods is forbidden: User "ram" cannot list resource "pods" in API group "" in the namespace "gpuworks-system"`)
		return
	}
	switch r.URL.Path {
	case "/apis/apps/v1/namespaces/gpuworks-system/daemonsets/gpuworks-agent":
		if f.ds == "" {
			status(w, 404, "NotFound", `daemonsets.apps "gpuworks-agent" not found`)
			return
		}
		fmt.Fprint(w, f.ds)
	case "/api/v1/namespaces/gpuworks-system/pods":
		if r.URL.Query().Get("labelSelector") != "app.kubernetes.io/name=gpuworks-agent" {
			status(w, 400, "BadRequest", "unexpected label selector "+r.URL.Query().Get("labelSelector"))
			return
		}
		fmt.Fprint(w, f.pods)
	case "/api/v1/namespaces/gpuworks-system/events":
		sel := map[string]string{}
		for _, part := range strings.Split(r.URL.Query().Get("fieldSelector"), ",") {
			k, v, _ := strings.Cut(part, "=")
			sel[k] = v
		}
		if len(sel) != 3 || sel["involvedObject.kind"] == "" || sel["involvedObject.name"] == "" || sel["reason"] == "" {
			status(w, 400, "BadRequest", "unexpected field selector "+r.URL.Query().Get("fieldSelector"))
			return
		}
		list, ok := f.events[sel["involvedObject.kind"]+"/"+sel["involvedObject.name"]+"/"+sel["reason"]]
		if !ok {
			list = `{"kind":"EventList","items":[]}`
		}
		fmt.Fprint(w, list)
	default:
		status(w, 404, "NotFound", "the server could not find the requested resource")
	}
}

func rolloutClient(t *testing.T, f *rolloutAPI) *Client {
	t.Helper()
	c, err := NewClient(serve(t, f, "    token: tok"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// fixturePods is a pod list captured from a kind cluster
// (kubemanifest/testdata/check), renamed to the agents' namespace.
func fixturePods(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "kubemanifest", "testdata", "check", "pods-"+name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func dsJSON(desired int, observed bool) string {
	gen := 0
	if observed {
		gen = 1
	}
	return fmt.Sprintf(`{"kind":"DaemonSet","metadata":{"name":"gpuworks-agent","generation":1},"status":{"observedGeneration":%d,"desiredNumberScheduled":%d}}`, gen, desired)
}

func eventList(msgs ...string) string {
	items := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		items = append(items, map[string]any{"kind": "Event", "message": m})
	}
	b, _ := json.Marshal(map[string]any{"kind": "EventList", "items": items})
	return string(b)
}

func judge(t *testing.T, f *rolloutAPI) ([]kubemanifest.CheckRow, bool) {
	t.Helper()
	state, err := rolloutClient(t, f).AgentRollout(context.Background(), "gpuworks-system", kubemanifest.AgentName)
	if err != nil {
		t.Fatal(err)
	}
	return kubemanifest.JudgeAgents(state)
}

func TestAgentRolloutReadsWhatTheCheckJudgesBy(t *testing.T) {
	for fixture, want := range map[string]kubemanifest.CheckCode{
		"ready":    kubemanifest.CheckOK,
		"crash":    kubemanifest.CheckContainerFailed,
		"unsched":  kubemanifest.CheckNotScheduled,
		"starting": kubemanifest.CheckStillStarting,
	} {
		rows, _ := judge(t, &rolloutAPI{ds: dsJSON(2, true), pods: fixturePods(t, fixture)})
		if len(rows) != 2 {
			t.Fatalf("%s: rows %+v", fixture, rows)
		}
		nodes := map[string]bool{}
		for _, r := range rows {
			nodes[r.Node] = true
			if r.Code != want {
				t.Errorf("%s: %s is %s, want %s (%s)", fixture, r.Node, r.Code, want, r.Detail)
			}
		}
		// Unscheduled pods too are named by the node the DaemonSet meant them for.
		if !nodes["gwks-precheck-worker"] || !nodes["gwks-precheck-control-plane"] {
			t.Errorf("%s: nodes %v", fixture, nodes)
		}
	}
}

func TestAgentRolloutTakesThePullErrorFromTheEvents(t *testing.T) {
	f := &rolloutAPI{ds: dsJSON(2, true), pods: fixturePods(t, "pull"), events: map[string]string{
		"Pod/pull-2wljm/Failed": eventList(`Failed to pull image "10.255.255.1:5000/gpuworks/gpuworks-agent:r1": dial tcp 10.255.255.1:5000: connect: no route to host`, "Error: ErrImagePull", "Error: ImagePullBackOff"),
	}}
	rows, done := judge(t, f)
	if !done {
		t.Error("a failed pull is an answer")
	}
	got := map[string]string{}
	for _, r := range rows {
		if r.Code != kubemanifest.CheckImagePullFailed {
			t.Errorf("%s: %s", r.Node, r.Code)
		}
		got[r.Pod] = r.Detail
	}
	if !strings.HasSuffix(got["pull-2wljm"], "connect: no route to host") {
		t.Errorf("detail from the event: %q", got["pull-2wljm"])
	}
	// Without an event the pod's own message stands.
	if !strings.HasPrefix(got["pull-4b6jv"], "ErrImagePull: failed to pull and unpack image") {
		t.Errorf("detail without an event: %q", got["pull-4b6jv"])
	}
}

func TestAgentRolloutSaysWhyPodsWereNotCreated(t *testing.T) {
	f := &rolloutAPI{ds: dsJSON(3, true), pods: fixturePods(t, "ready"), events: map[string]string{
		"DaemonSet/gpuworks-agent/FailedCreate": eventList(`Error creating: pods "gpuworks-agent-x" is forbidden: exceeded quota: compute`),
	}}
	rows, done := judge(t, f)
	last := rows[len(rows)-1]
	if !done || last.Code != kubemanifest.CheckPodCreateFailed || !strings.Contains(last.Detail, "1 of 3 pods were not created: Error creating") {
		t.Errorf("rows %+v done %v", rows, done)
	}
	// Only then are the DaemonSet's events read.
	f = &rolloutAPI{ds: dsJSON(2, true), pods: fixturePods(t, "ready")}
	judge(t, f)
	for _, r := range f.requests {
		if strings.Contains(r, "/events") {
			t.Errorf("read events with every pod there: %s", r)
		}
	}
}

func TestAgentRolloutWithoutTheDaemonSetOrItsCount(t *testing.T) {
	rows, done := judge(t, &rolloutAPI{})
	if !done || rows[0].Code != kubemanifest.CheckDaemonSetMissing {
		t.Errorf("no DaemonSet: %+v %v", rows, done)
	}
	rows, done = judge(t, &rolloutAPI{ds: dsJSON(0, false), pods: `{"items":[]}`})
	if done || rows[0].Code != kubemanifest.CheckStillStarting {
		t.Errorf("not counted yet: %+v %v", rows, done)
	}
	rows, done = judge(t, &rolloutAPI{ds: dsJSON(0, true), pods: `{"items":[]}`})
	if !done || rows[0].Code != kubemanifest.CheckNoNodeSelected {
		t.Errorf("no node: %+v %v", rows, done)
	}
}

// A pod going away (a rolling update, a deleted node) is no answer for its node.
func TestAgentRolloutLeavesOutPodsGoingAway(t *testing.T) {
	var list map[string]any
	if err := json.Unmarshal([]byte(fixturePods(t, "ready")), &list); err != nil {
		t.Fatal(err)
	}
	items := list["items"].([]any)
	items[0].(map[string]any)["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-26T04:00:00Z"
	b, _ := json.Marshal(list)
	rows, done := judge(t, &rolloutAPI{ds: dsJSON(2, true), pods: string(b)})
	if done || len(rows) != 2 || rows[0].Code != kubemanifest.CheckOK || rows[1].Code != kubemanifest.CheckPodCreateFailed {
		t.Errorf("rows %+v done %v", rows, done)
	}
}

func TestAgentRolloutPassesARefusalOn(t *testing.T) {
	f := &rolloutAPI{ds: dsJSON(2, true), forbid: "/api/v1/namespaces/gpuworks-system/pods"}
	_, err := rolloutClient(t, f).AgentRollout(context.Background(), "gpuworks-system", kubemanifest.AgentName)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != 403 || !strings.Contains(err.Error(), "cannot list resource") {
		t.Fatalf("err %v", err)
	}
}
