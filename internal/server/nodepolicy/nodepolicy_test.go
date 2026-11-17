package nodepolicy

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Every knob is a Policy field, a Setting and a column: the three cannot
// drift apart, which is what lets a new knob be one field, one Setting and
// one migration column with nothing else to change.
func TestEveryPolicyFieldIsASettingAndAColumn(t *testing.T) {
	typ := reflect.TypeOf(Policy{})
	var fields, columns []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		column := ""
		for _, part := range strings.Split(field.Tag.Get("gorm"), ";") {
			if name, ok := strings.CutPrefix(part, "column:"); ok {
				column = name
			}
		}
		if column == "" {
			t.Fatalf("Policy.%s has no gorm column", field.Name)
		}
		if !strings.Contains(field.Tag.Get("gorm"), "not null") || !strings.Contains(field.Tag.Get("gorm"), "default:") {
			t.Fatalf("Policy.%s column must be NOT NULL with a default, so existing nodes read as the default", field.Name)
		}
		fields = append(fields, field.Name)
		columns = append(columns, column)
	}
	var keys []string
	for _, setting := range Settings() {
		if setting.Get == nil || setting.Set == nil || setting.Applies == nil || setting.Kind == "" {
			t.Fatalf("setting %q is incomplete", setting.Key)
		}
		keys = append(keys, string(setting.Key))
	}
	sort.Strings(keys)
	sort.Strings(columns)
	if !reflect.DeepEqual(keys, columns) {
		t.Fatalf("setting keys %v, columns %v: each Policy field is one setting keyed by its column", keys, columns)
	}
	if len(fields) != len(keys) {
		t.Fatalf("%d fields, %d settings", len(fields), len(keys))
	}
	var values []string
	for key := range Values(Policy{}) {
		values = append(values, string(key))
	}
	sort.Strings(values)
	if !reflect.DeepEqual(values, keys) {
		t.Fatalf("Values keys %v, want the settings %v", values, keys)
	}
	var available []string
	for _, item := range Applicability(Facts{}) {
		available = append(available, string(item.Key))
	}
	sort.Strings(available)
	if !reflect.DeepEqual(available, keys) {
		t.Fatalf("Applicability keys %v, want the settings %v", available, keys)
	}
}

// Defaults are what a node that never set a knob reads, and what a zero
// Policy holds.
func TestDefaultsAreTheZeroPolicy(t *testing.T) {
	for _, setting := range Settings() {
		if got := setting.Get(Policy{}); got != setting.Default {
			t.Fatalf("%s: zero policy reads %v, default %v", setting.Key, got, setting.Default)
		}
	}
	if Values(Policy{})[KeyShareWithForeign] != false {
		t.Fatal("share_with_foreign must default to off")
	}
}

// The one place the consent rule lives: the node allows sharing, or the
// operator chose the cards.
func TestSharesForeignTruthTable(t *testing.T) {
	for _, tc := range []struct {
		allow, chosen, want bool
	}{
		{false, false, false},
		{false, true, true},
		{true, false, true},
		{true, true, true},
	} {
		got := Policy{ShareWithForeign: tc.allow}.SharesForeign(Consent{CardsChosen: tc.chosen})
		if got != tc.want {
			t.Fatalf("allow=%v chosen=%v: %v, want %v", tc.allow, tc.chosen, got, tc.want)
		}
	}
}

func TestApplyWritesKnownKeysAndRefusesTheRest(t *testing.T) {
	got, err := Apply(Policy{}, map[Key]json.RawMessage{KeyShareWithForeign: json.RawMessage("true")})
	if err != nil || !got.ShareWithForeign {
		t.Fatalf("Apply(true) = %+v, %v", got, err)
	}
	back, err := Apply(got, map[Key]json.RawMessage{KeyShareWithForeign: json.RawMessage("false")})
	if err != nil || back.ShareWithForeign {
		t.Fatalf("Apply(false) = %+v, %v", back, err)
	}
	if same, err := Apply(got, nil); err != nil || same != got {
		t.Fatalf("an empty patch changed the policy: %+v, %v", same, err)
	}
	for _, tc := range []struct {
		name  string
		patch map[Key]json.RawMessage
		key   Key
	}{
		{"unknown key", map[Key]json.RawMessage{"reticulate": json.RawMessage("true")}, "reticulate"},
		{"string for bool", map[Key]json.RawMessage{KeyShareWithForeign: json.RawMessage(`"yes"`)}, KeyShareWithForeign},
		{"null for bool", map[Key]json.RawMessage{KeyShareWithForeign: json.RawMessage(`null`)}, KeyShareWithForeign},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Apply(Policy{ShareWithForeign: true}, tc.patch)
			var invalid *InvalidError
			if !errors.As(err, &invalid) || invalid.Key != tc.key {
				t.Fatalf("Apply = %v, want an InvalidError for %q", err, tc.key)
			}
		})
	}
}

func TestApplicabilityNamesWhyTheSwitchHasNoEffect(t *testing.T) {
	for _, tc := range []struct {
		name   string
		facts  Facts
		ok     bool
		reason string
	}{
		{"ready and tracked", Facts{PartitionBackendReady: true, TracksForeign: true}, true, ""},
		{"no backend", Facts{TracksForeign: true}, false, ReasonNoPartitionBackend},
		{"old agent", Facts{PartitionBackendReady: true}, false, ReasonAgentTooOld},
		{"neither: the backend first", Facts{}, false, ReasonNoPartitionBackend},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, item := range Applicability(tc.facts) {
				if item.Key != KeyShareWithForeign {
					continue
				}
				if item.Applicable != tc.ok || item.Reason != tc.reason {
					t.Fatalf("availability = %+v, want applicable %v reason %q", item, tc.ok, tc.reason)
				}
				return
			}
			t.Fatal("share_with_foreign has no availability")
		})
	}
}
