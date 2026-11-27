package priority

import (
	"slices"
	"strings"
	"testing"
)

func testTable() Table {
	classes := []Class{
		{ID: 1, Name: Critical, Value: 40}, {ID: 2, Name: High, Value: 20}, {ID: 3, Name: Normal, Value: 0},
		{ID: 4, Name: Low, Value: -20}, {ID: 5, Name: Batch, Value: -40}, {ID: 6, Name: "gold", Value: 30},
	}
	return NewTable(classes, 3)
}

func TestResolve(t *testing.T) {
	table := testTable()
	cases := []struct {
		name      string
		source    Source
		binding   Binding
		requested string
		want      string
		lowered   bool
		unknown   bool
	}{
		{name: "anonymous runs at the default", source: SourceAnonymous, requested: Critical, want: Normal},
		{name: "a batch that names no class runs at batch", source: SourceBatch, binding: Binding{ClassID: 1}, want: Batch},
		{name: "a batch runs at the class it was created with", source: SourceBatch, binding: Binding{ClassID: 4}, requested: Critical, want: Critical},
		{name: "a batch whose class is gone runs at batch", source: SourceBatch, requested: "platinum", want: Batch},
		{name: "the console runs at the default unless it names a class", source: SourceConsole, binding: Binding{ClassID: 1}, want: Normal},
		{name: "the console runs at any class it names", source: SourceConsole, requested: " Critical ", want: Critical},
		{name: "the console naming no class is refused", source: SourceConsole, requested: "platinum", unknown: true},
		{name: "unbound key, nothing asked", want: Normal},
		{name: "bound key, nothing asked", binding: Binding{ClassID: 2}, want: High},
		{name: "within its own class by default", binding: Binding{ClassID: 2}, requested: Low, want: Low},
		{name: "above its own class without a ceiling is lowered", binding: Binding{ClassID: 2}, requested: Critical, want: High, lowered: true},
		{name: "up to the ceiling", binding: Binding{ClassID: 3, CeilingID: 6}, requested: "gold", want: "gold"},
		{name: "above the ceiling is lowered to it", binding: Binding{ClassID: 3, CeilingID: 6}, requested: Critical, want: "gold", lowered: true},
		{name: "a ceiling below the own class does not lower the default", binding: Binding{ClassID: 2, CeilingID: 4}, requested: High, want: High},
		{name: "fixed ignores the ask", binding: Binding{ClassID: 3, CeilingID: 1, Policy: PolicyFixed}, requested: Critical, want: Normal},
		{name: "fixed ignores a lower ask too", binding: Binding{ClassID: 3, Policy: PolicyFixed}, requested: Low, want: Normal},
		{name: "down only lowers", binding: Binding{ClassID: 2, CeilingID: 1, Policy: PolicyDownOnly}, requested: Batch, want: Batch},
		{name: "down only never raises, whatever the ceiling", binding: Binding{ClassID: 2, CeilingID: 1, Policy: PolicyDownOnly}, requested: Critical, want: High, lowered: true},
		{name: "an unknown class is refused", binding: Binding{ClassID: 2}, requested: "platinum", unknown: true},
		{name: "the client's case does not matter", binding: Binding{ClassID: 2}, requested: " LOW ", want: Low},
		{name: "a stale class id falls back to the default", binding: Binding{ClassID: 99}, want: Normal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Resolve(table, tc.source, tc.binding, tc.requested)
			if ok == tc.unknown {
				t.Fatalf("ok = %v", ok)
			}
			if tc.unknown {
				return
			}
			if got.Class.Name != tc.want || got.Lowered != tc.lowered {
				t.Fatalf("decision = %+v, want %s lowered=%v", got, tc.want, tc.lowered)
			}
		})
	}
}

func TestTableDefaults(t *testing.T) {
	if got := NewTable([]Class{{ID: 3, Name: Normal}, {ID: 7, Name: "x", Value: 5}}, 7).Default.Name; got != "x" {
		t.Fatalf("default = %s", got)
	}
	if got := NewTable([]Class{{ID: 3, Name: Normal}}, 99).Default.Name; got != Normal {
		t.Fatalf("missing default falls back to normal, got %s", got)
	}
	if got := NewTable(nil, 0).Default; got.Name != Normal || got.Value != 0 {
		t.Fatalf("empty table default = %+v", got)
	}
}

func TestValidation(t *testing.T) {
	for name, want := range map[string]bool{"gold": true, "tier-2": true, "Gold": false, "2x": false, "": false, "a b": false, "abcdefghijklmnopqrstuvwxyz0123456": false} {
		if ValidName(name) != want {
			t.Fatalf("ValidName(%q) = %v", name, !want)
		}
	}
	if !ValidValue(-100) || !ValidValue(100) || ValidValue(101) || ValidValue(-101) {
		t.Fatal("value bounds")
	}
	for text, want := range map[string]Policy{"": PolicyWithinCeiling, "fixed": PolicyFixed, "down_only": PolicyDownOnly, "within_ceiling": PolicyWithinCeiling} {
		if got, ok := ParsePolicy(text); !ok || got != want {
			t.Fatalf("ParsePolicy(%q) = %v %v", text, got, ok)
		}
	}
	if _, ok := ParsePolicy("strict"); ok {
		t.Fatal("unknown policy accepted")
	}
	if !IsBuiltin(Batch) || IsBuiltin("gold") {
		t.Fatal("IsBuiltin")
	}
}

func TestKeyReach(t *testing.T) {
	table := testTable()
	cases := []struct {
		name         string
		binding      Binding
		own, ceiling string
		reachable    []string
	}{
		{name: "within its ceiling", binding: Binding{ClassID: 3, CeilingID: 1}, own: Normal, ceiling: Critical, reachable: []string{Critical, "gold", High, Normal, Low, Batch}},
		{name: "a custom-valued ceiling", binding: Binding{ClassID: 3, CeilingID: 6}, own: Normal, ceiling: "gold", reachable: []string{"gold", High, Normal, Low, Batch}},
		{name: "no ceiling is its own class", binding: Binding{ClassID: 2}, own: High, ceiling: High, reachable: []string{High, Normal, Low, Batch}},
		{name: "a ceiling below its own class is its own class", binding: Binding{ClassID: 2, CeilingID: 4}, own: High, ceiling: High, reachable: []string{High, Normal, Low, Batch}},
		{name: "unbound runs at the default", binding: Binding{}, own: Normal, ceiling: Normal, reachable: []string{Normal, Low, Batch}},
		{name: "fixed reaches only its own class", binding: Binding{ClassID: 3, CeilingID: 1, Policy: PolicyFixed}, own: Normal, ceiling: Normal, reachable: []string{Normal}},
		{name: "down only reaches what is not above it", binding: Binding{ClassID: 6, CeilingID: 1, Policy: PolicyDownOnly}, own: "gold", ceiling: "gold", reachable: []string{"gold", High, Normal, Low, Batch}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := KeyReach(table, tc.binding)
			var names []string
			for _, c := range got.Reachable {
				names = append(names, c.Name)
			}
			if got.Own.Name != tc.own || got.Ceiling.Name != tc.ceiling || strings.Join(names, ",") != strings.Join(tc.reachable, ",") {
				t.Fatalf("reach = own %s ceiling %s reachable %v, want %s %s %v", got.Own.Name, got.Ceiling.Name, names, tc.own, tc.ceiling, tc.reachable)
			}
			// The rule a client applies to the reach (a reachable class runs
			// as named, any other at the ceiling) is the one Resolve applies.
			for name, class := range table.byName {
				want := got.Ceiling
				if slices.Contains(got.Reachable, class) {
					want = class
				}
				if decision, _ := Resolve(table, SourceKey, tc.binding, name); decision.Class != want {
					t.Fatalf("asking %s runs at %s, the reach says %s", name, decision.Class.Name, want.Name)
				}
			}
		})
	}
	if got := KeyReach(NewTable(nil, 0), Binding{}); len(got.Reachable) != 1 || got.Reachable[0] != got.Own || got.Own.Name != Normal {
		t.Fatalf("an empty table's reach = %+v, want its neutral default alone", got)
	}
}
