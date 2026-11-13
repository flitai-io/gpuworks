package kubeapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestForbiddenAndPodConditions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `{"kind":"Status","reason":"Forbidden","code":403,"message":"pods is forbidden"}`)
	}))
	defer srv.Close()
	c := New(srv.URL, func() (string, error) { return "tok", nil }, srv.Client())
	err := c.Get(context.Background(), AllPodsPath, &PodList{})
	if !IsForbidden(err) || IsNotFound(err) {
		t.Fatalf("forbidden list = %v", err)
	}
	var pod Pod
	// A pod no scheduler could place, as the API server shows it.
	raw := `{"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable","message":"0/1 nodes are available: 1 1/1 CardInsufficientMemory."}]}}`
	if err := json.Unmarshal([]byte(raw), &pod); err != nil {
		t.Fatal(err)
	}
	cond, ok := pod.Status.Condition("PodScheduled")
	if !ok || cond.Reason != "Unschedulable" || !strings.Contains(cond.Message, "CardInsufficientMemory") {
		t.Fatalf("condition = %+v, %v", cond, ok)
	}
	if _, ok := pod.Status.Condition("Ready"); ok {
		t.Fatal("a condition the pod does not have was found")
	}
}
