package inferenceapi

import (
	"encoding/json"
	"strings"
	"testing"
)

// An image edit may declare how many reference images it accepts; nothing else
// may, and a contract stored before the field existed still means one.
func TestMaxReferenceImagesDeclaration(t *testing.T) {
	edit := func(max int) Contract {
		return Contract{Operations: []Operation{{Type: OperationImageEdit, MaxReferenceImages: max}}}
	}
	for _, tc := range []struct {
		name     string
		contract Contract
		limit    int
		err      string
	}{
		{"unset", edit(0), 1, ""},
		{"nine", edit(9), 9, ""},
		{"many", edit(64), 64, ""},
		{"negative", edit(-1), 0, "positive count"},
		{"generation", Contract{Operations: []Operation{{Type: OperationImageGenerate, MaxReferenceImages: 2}}}, 0, "only valid for openai.image.edit"},
		{"video", Contract{Operations: []Operation{{Type: OperationVideoGenerate, MaxReferenceImages: 2}}}, 0, "only valid for openai.image.edit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			normalized, err := NormalizeContract(tc.contract)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := normalized.Operations[0].ReferenceImageLimit(); got != tc.limit {
				t.Fatalf("limit = %d, want %d", got, tc.limit)
			}
		})
	}
	if got := (Operation{Type: OperationImageGenerate}).ReferenceImageLimit(); got != 0 {
		t.Fatalf("image generation limit = %d, want 0", got)
	}
	var stored Contract
	if err := json.Unmarshal([]byte(`{"operations":[{"type":"openai.image.edit","endpoint":"/v1/images/edits","mode":"sync","request_encoding":"multipart","inputs":[]}]}`), &stored); err != nil {
		t.Fatal(err)
	}
	if got := stored.Operations[0].ReferenceImageLimit(); got != 1 {
		t.Fatalf("a contract stored without the field allows %d reference images, want 1", got)
	}
	encoded, _ := json.Marshal(Operation{Type: OperationImageEdit, MaxReferenceImages: 9})
	if !strings.Contains(string(encoded), `"max_reference_images":9`) {
		t.Fatalf("wire = %s", encoded)
	}
	encoded, _ = json.Marshal(Operation{Type: OperationImageEdit})
	if strings.Contains(string(encoded), "max_reference_images") {
		t.Fatalf("an unset count is written: %s", encoded)
	}
}
