package planning

import (
	"encoding/json"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// ParsePlacement reads the two shapes a placement appears in - the decoded
// request object and the JSON text of a stored row - into one value. The cases
// below are the behaviors the four readers it replaced had between them, so the
// unification cannot quietly change how a hint is read.
func TestParsePlacementReadsBothShapes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  Placement
	}{
		{"absent", nil, Placement{}},
		{"empty text", "", Placement{}},
		{"explicit null text", "null", Placement{}},
		{"stored text", `{"tp":2,"gpus":[0,1]}`, Placement{TP: 2, GPUIndexes: []int{0, 1}}},
		{"stored text with whitespace", " {\"tp\":2} ", Placement{TP: 2}},
		{
			"decoded request object",
			map[string]any{"tp": json.Number("2"), "gpu_indexes": []any{float64(1), float64(3)}},
			Placement{TP: 2, GPUIndexes: []int{1, 3}},
		},
		{
			// The deploy form sends "gpus"; a confirmed plan echoes
			// "gpu_indexes". Both spellings are read, and a row that carries
			// both means the form's spelling: it is the one the user confirmed.
			"both spellings",
			map[string]any{"gpus": []any{float64(0)}, "gpu_indexes": []any{float64(2)}},
			Placement{GPUIndexes: []int{0}},
		},
		{
			// Keys an older form sent and nothing reads are ignored.
			"dead top-level keys",
			map[string]any{"mode": " manual ", "node_uuid": " node-a ", "tp": float64(1)},
			Placement{TP: 1},
		},
		{
			// A hint the reader cannot use leaves the field unset rather than
			// failing the deployment: these are best-effort hints.
			"unusable fields are unset",
			map[string]any{"tp": 2.5, "gpus": []any{float64(-1), 1.5, "x", float64(2)}},
			Placement{GPUIndexes: []int{2}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePlacement(tc.value)
			if err != nil {
				t.Fatalf("ParsePlacement(%#v) error = %v", tc.value, err)
			}
			// Whose choice the indexes are is OperatorPinned's to test.
			if !reflect.DeepEqual(got.WithoutOperatorChoice(), tc.want) {
				t.Fatalf("ParsePlacement(%#v) = %+v, want %+v", tc.value, got, tc.want)
			}
		})
	}
}

// A selector still inside a placement - whatever its shape, readable or not -
// is refused rather than ignored: where a deployment may run moved to its node
// requirements, and ignoring the selector would widen it. A document that is
// not a placement at all is refused too.
func TestParsePlacementRefusesSelectorsAndUnreadableDocuments(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"broken JSON", "{"},
		{"JSON array", `[1,2]`},
		{"a readable selector", `{"node_labels":{"pool":"inference"}}`},
		{"an empty selector", `{"node_labels":{}}`},
		{"JSON null label object", `{"node_labels":null}`},
		{"labels as a list", `{"node_labels":["pool=inference"]}`},
		{"a selector in a request object", map[string]any{"node_labels": map[string]any{"pool": "inference"}}},
		{"unsupported value type", 42},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParsePlacement(tc.value); err == nil {
				t.Fatalf("ParsePlacement(%#v) accepted it", tc.value)
			}
		})
	}
}

// testNode builds a node carrying the given user labels.
func testNode(uuid string, labels map[string]string) *store.Node {
	encoded, err := json.Marshal(labels)
	if err != nil {
		panic(err)
	}
	return &store.Node{UUID: uuid, Labels: string(encoded)}
}
