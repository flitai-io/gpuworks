package accelerator

import "slices"

// Precision is a number format a card's peak rate is published for.
type Precision string

// The precisions a card spec states. FP32 is the rate of the ordinary
// cores; the others are the matrix (Tensor Core or equivalent) rates, with
// FP32 accumulation where the source gives both.
const (
	PrecisionFP32 Precision = "fp32"
	PrecisionTF32 Precision = "tf32"
	PrecisionBF16 Precision = "bf16"
	PrecisionFP16 Precision = "fp16"
	PrecisionFP8  Precision = "fp8"
	PrecisionINT8 Precision = "int8"
)

// Precisions lists every precision a card spec accounts for, in the order
// they are shown.
var Precisions = []Precision{PrecisionBF16, PrecisionFP16, PrecisionFP8, PrecisionINT8, PrecisionTF32, PrecisionFP32}

// DefaultPrecision is the precision a compute total is shown in until the
// viewer picks another: the format models are served in most often.
const DefaultPrecision = PrecisionBF16

// Integer reports whether rates in this precision count operations (TOPS)
// rather than floating-point operations (TFLOPS).
func (p Precision) Integer() bool { return p == PrecisionINT8 }

// Peak is one published peak rate, written as the source prints it so a
// reviewer can hold the entry against the page.
type Peak struct {
	Precision Precision
	// Stated is the figure in TFLOPS, or TOPS for an integer precision.
	Stated float64
	// Sparse: the source states the figure with 2:4 structured sparsity,
	// which by its own definition doubles the dense rate.
	Sparse bool
}

// Dense is the rate without sparsity.
func (p Peak) Dense() float64 {
	if p.Sparse {
		return p.Stated / 2
	}
	return p.Stated
}

// InterconnectKind names a link a card talks to other cards or the host
// over.
type InterconnectKind string

// The interconnects a card spec can state.
const (
	InterconnectNVLink InterconnectKind = "nvlink"
	InterconnectPCIe   InterconnectKind = "pcie"
)

// Interconnect is one link's published bandwidth per card.
type Interconnect struct {
	Kind       InterconnectKind
	Generation int
	GBps       float64
	// Bidirectional: GBps is the sum of both directions, as the source
	// states it; otherwise it is one direction.
	Bidirectional bool
}

// SpecFact is a kind of figure a source vouches for.
type SpecFact string

// The facts of a card spec, each of which some source must cover.
const (
	FactCompute               SpecFact = "compute"
	FactMemoryBandwidth       SpecFact = "memory_bandwidth"
	FactInterconnect          SpecFact = "interconnect"
	FactInterconnectDirection SpecFact = "interconnect_direction"
)

// SpecSource is a vendor document a card spec's figures come from.
type SpecSource struct {
	URL     string
	Title   string
	Version string
	// Retrieved is the date the figures were read off it, YYYY-MM-DD.
	Retrieved string
	Covers    []SpecFact
}

// covering is the source vouching for the given facts of one card spec: a
// document shared by several specs supports different figures of each.
func (s SpecSource) covering(facts ...SpecFact) SpecSource {
	s.Covers = facts
	return s
}

// CardSpec is one card model's published peak figures. Every figure comes
// from the vendor's own documents (Sources); the name it is found by was
// read off real hardware (NameSeen), because a name written from a
// datasheet need not be what the vendor tool prints. A model without both
// is not added, and no model stands in for another.
type CardSpec struct {
	// Name is the model as the platform stores it: the vendor tool's name
	// with the brand word dropped (NormalizeGPUName).
	Name string
	// Peaks and Unsupported account for every precision in Precisions
	// between them, so a missing rate never reads as unknown.
	Peaks       []Peak
	Unsupported []Precision
	// MemoryBandwidthGBps is the card's memory bandwidth in GB/s.
	MemoryBandwidthGBps float64
	Interconnects       []Interconnect
	Sources             []SpecSource
	// NameSeen says which tool printed Name, on what hardware and when.
	NameSeen string
}

// Peak is the dense rate in one precision. ok is false when the card does
// not compute in it.
func (s CardSpec) Peak(p Precision) (dense float64, ok bool) {
	for _, peak := range s.Peaks {
		if peak.Precision == p {
			return peak.Dense(), true
		}
	}
	return 0, false
}

// CardSpecOf finds the published figures of a vendor's card by the model
// name the platform stores. ok is false for a model the vendor's definition
// does not list: the name must match exactly, so a variant never takes a
// sibling's figures.
func CardSpecOf(vendor, name string) (CardSpec, bool) {
	spec, ok := vendorSpecOf(vendor)
	if !ok {
		return CardSpec{}, false
	}
	return spec.cardSpec(name)
}

func (v vendorSpec) cardSpec(name string) (CardSpec, bool) {
	name = NormalizeGPUName(name)
	for _, card := range v.cardSpecs {
		if card.Name == name {
			return card.clone(), true
		}
	}
	return CardSpec{}, false
}

func (s CardSpec) clone() CardSpec {
	s.Peaks = slices.Clone(s.Peaks)
	s.Unsupported = slices.Clone(s.Unsupported)
	s.Interconnects = slices.Clone(s.Interconnects)
	sources := make([]SpecSource, len(s.Sources))
	for i, source := range s.Sources {
		source.Covers = slices.Clone(source.Covers)
		sources[i] = source
	}
	s.Sources = sources
	return s
}
