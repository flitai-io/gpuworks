package priority

import "testing"

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
