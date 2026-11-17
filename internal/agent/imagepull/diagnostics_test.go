package imagepull

import (
	"context"
	"strings"
	"testing"
)

func TestRegistryProbeEvidenceDoesNotReplaceTheActualPull(t *testing.T) {
	p, _ := newPuller(t, newClock(), nil, Config{})
	d := newDriver("udocker", map[string]pullStep{
		"a.example/x:1": failCLI("unauthorized: image access denied"),
		"b.example/x:1": ok,
	})
	var summary string
	var failures []error
	result, err := p.Walk(context.Background(), d, ranked("a.example/x:1", "b.example/x:1"), Options{}, Hooks{
		Ordered: func(s string) { summary = s },
		Failed:  func(_ string, err error) { failures = append(failures, err) },
	})
	if err != nil || result.Ref != "b.example/x:1" || strings.Join(d.order(), ",") != "a.example/x:1,b.example/x:1" {
		t.Fatalf("probe replaced actual pull/fallback: result=%+v err=%v pulls=%v", result, err, d.order())
	}
	if len(failures) != 1 || !strings.Contains(failures[0].Error(), "image access denied") {
		t.Fatalf("missing actual pull failure: %v", failures)
	}
	for _, want := range []string{"HTTP 401", "GET /v2/", "observed_at=", "from=node", "image authorization, tag, manifest and layers not checked"} {
		if !strings.Contains(summary, want) {
			t.Errorf("ranking evidence %q lacks %q", summary, want)
		}
	}
	if p.prober.Cooled("a.example") {
		t.Fatal("image authorization failure incorrectly cooled a registry that answered")
	}
}
