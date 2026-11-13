package kube

import (
	"context"
	"testing"
)

func TestNodesSelectedMatchesLabelsLikeTheManifest(t *testing.T) {
	r := newRig(t, 0)
	ctx := context.Background()
	r.srv.AddNode("pool-a-1", map[string]string{"gpu": "yes", "zone": "z1", "pool": "np-a"})
	r.srv.AddNode("pool-b-1", map[string]string{"gpu": "yes", "zone": "z1", "pool": "np-b"})
	r.srv.AddNode("plain", map[string]string{"zone": "z2"})
	r.srv.AddNode("empty-value", map[string]string{"flag": ""})
	names := []string{"pool-a-1", "pool-b-1", "plain", "empty-value", "gone-node"}
	for _, tc := range []struct {
		name     string
		selector map[string]string
		label    string
		ids      []string
		want     map[string]bool
	}{
		{"selector pairs must all match", map[string]string{"gpu": "yes", "zone": "z1"}, "", nil,
			map[string]bool{"pool-a-1": true, "pool-b-1": true}},
		{"a value must be equal, not just present", map[string]string{"zone": "z9"}, "", nil, map[string]bool{}},
		{"an empty value is a value", map[string]string{"flag": ""}, "", nil, map[string]bool{"empty-value": true}},
		{"pool label in the pool ids", nil, "pool", []string{"np-a"}, map[string]bool{"pool-a-1": true}},
		{"several pools", nil, "pool", []string{"np-a", "np-b"}, map[string]bool{"pool-a-1": true, "pool-b-1": true}},
		{"selector and pools both apply", map[string]string{"zone": "z2"}, "pool", []string{"np-a"}, map[string]bool{}},
		{"pool label with no ids selects nothing", nil, "pool", nil, map[string]bool{}},
		{"no selection selects every node that exists", nil, "", nil,
			map[string]bool{"pool-a-1": true, "pool-b-1": true, "plain": true, "empty-value": true}},
	} {
		got, err := r.d.NodesSelected(ctx, names, tc.selector, tc.label, tc.ids)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != len(names) {
			t.Fatalf("%s: answered for %v, asked %v", tc.name, got, names)
		}
		for _, n := range names {
			if got[n] != tc.want[n] {
				t.Errorf("%s: %s selected = %v, want %v", tc.name, n, got[n], tc.want[n])
			}
		}
	}
	if got, err := r.d.NodesSelected(ctx, nil, nil, "", nil); err != nil || len(got) != 0 {
		t.Fatalf("no names = %v, %v", got, err)
	}
	r.srv.FailNext["GET /api/v1/nodes/"] = 500
	if _, err := r.d.NodesSelected(ctx, []string{"plain"}, nil, "", nil); err == nil {
		t.Fatal("an API failure read as an answer")
	}
}
