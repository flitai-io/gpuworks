package chipmatch

import "testing"

func TestDomesticInheritancePredicatesMatchOnlyApprovedFamilies(t *testing.T) {
	tests := []struct {
		name      string
		predicate Predicate
		matches   []string
		rejects   []string
	}{
		{
			name:      "Hygon K100 PASS inheritance covers the BW family and excludes K500",
			predicate: Predicate{Vendor: "hygon", Name: "(^|[^A-Za-z0-9])(?:K100(?:[_ -]?AI)?|BW(?:[_ -]?[0-9]+)?)([^A-Za-z0-9]|$)"},
			matches:   []string{"K100", "K100_AI", "K100AI", "BW", "BW100", "BW150", "BW1000", "BW1100", "BW_1000", "BW3000"},
			rejects:   []string{"K500", "Z100", "BWX", "BW1000X"},
		},
		{
			name:      "Ascend 910B PASS inheritance stays within approved SKUs",
			predicate: Predicate{Vendor: "ascend", Name: "(^|[^A-Za-z0-9])(?:910[BC][A-Za-z0-9]*|950DT|950PR)([^A-Za-z0-9]|$)"},
			matches:   []string{"910B", "910B1", "910B2C", "910B4", "910C", "910C3", "950DT", "950PR"},
			rejects:   []string{"310P", "910A", "Atlas 300I"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, name := range tt.matches {
				result := Select(GPU{Vendor: tt.predicate.Vendor, Name: name}, []Spec{{Name: "candidate", GPUs: tt.predicate}}, nil)
				if !result.Matched {
					t.Errorf("%q did not match predicate %q", name, tt.predicate.Name)
				}
			}
			for _, name := range tt.rejects {
				result := Select(GPU{Vendor: tt.predicate.Vendor, Name: name}, []Spec{{Name: "candidate", GPUs: tt.predicate}}, nil)
				if result.Matched {
					t.Errorf("%q unexpectedly matched predicate %q", name, tt.predicate.Name)
				}
			}
		})
	}
}
