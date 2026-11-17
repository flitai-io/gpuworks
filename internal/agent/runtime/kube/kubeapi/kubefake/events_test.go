package kubefake

import (
	"context"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

func validEvent() kubeapi.Event {
	return kubeapi.Event{
		Metadata: kubeapi.ObjectMeta{Name: "agent.1a2b", Namespace: "gpuworks-system"},
		Involved: kubeapi.ObjectReference{APIVersion: "v1", Kind: "Pod", Namespace: "gpuworks-system", Name: "agent"},
		Reason:   "Why", Type: "Warning", Message: "m",
	}
}

// TestEventCreateIsValidatedLikeAnAPIServer: a fake that accepted anything
// would hide an event the real server rejects.
func TestEventCreateIsValidatedLikeAnAPIServer(t *testing.T) {
	k := New(t, "gpuworks-system", "n1")
	c := k.Client()
	ctx := context.Background()

	got, err := c.CreateEvent(ctx, validEvent())
	if err != nil || got.Metadata.UID == "" || got.Kind != "Event" {
		t.Fatalf("CreateEvent = %+v, %v", got, err)
	}
	if evs := k.Events("gpuworks-system"); len(evs) != 1 || evs[0].Reason != "Why" {
		t.Fatalf("Events = %+v", evs)
	}
	if _, err := c.CreateEvent(ctx, validEvent()); !kubeapi.IsAlreadyExists(err) {
		t.Errorf("duplicate name = %v, want AlreadyExists", err)
	}
	for name, mutate := range map[string]func(*kubeapi.Event){
		"involved object in another namespace": func(e *kubeapi.Event) { e.Involved.Namespace = "other" },
		"no involved kind":                     func(e *kubeapi.Event) { e.Involved.Kind = "" },
		"bad type":                             func(e *kubeapi.Event) { e.Type = "Error" },
		"bad name":                             func(e *kubeapi.Event) { e.Metadata.Name = "Not A Name" },
	} {
		ev := validEvent()
		ev.Metadata.Name = "e-" + name[:3]
		mutate(&ev)
		var apiErr *kubeapi.APIError
		if _, err := c.CreateEvent(ctx, ev); !errors.As(err, &apiErr) || apiErr.Code != 422 {
			t.Errorf("%s: err = %v, want 422 Invalid", name, err)
		}
	}
	if n := len(k.Events("gpuworks-system")); n != 1 {
		t.Errorf("rejected events were stored: %d", n)
	}

	k.DenyEvents = true
	ev := validEvent()
	ev.Metadata.Name = "denied"
	if _, err := c.CreateEvent(ctx, ev); !kubeapi.IsForbidden(err) {
		t.Errorf("denied create = %v, want Forbidden", err)
	}
}
