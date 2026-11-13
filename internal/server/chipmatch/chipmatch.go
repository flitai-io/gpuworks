// Package chipmatch evaluates the flat GPU predicates used by inference specs.
// It deliberately has no knowledge of the catalog store: callers translate their
// spec rows into the small value types in this package.
package chipmatch

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// GPU is the normalized hardware fact reported by an agent. ArchID is the
// persisted architecture identity; an empty value remains unknown.
type GPU struct {
	Name   string
	Vendor string
	CC     float64
	ArchID string
}

// Predicate is the inline gpus object from a spec or unsupported rule. Vendor
// is required. CC is a NVIDIA-only expression: >=x, <x, x, or x..y. Name is a
// case-insensitive regular expression against the detector-provided card name.
// ArchID, when set, requires a non-empty exact architecture ID match ignoring case.
type Predicate struct {
	Vendor string `json:"vendor"`
	CC     string `json:"cc,omitempty"`
	Name   string `json:"name,omitempty"`
	ArchID string `json:"arch_id,omitempty"`
}

// Spec is the matching subset of a flat inference spec. Indexes returned by
// Select refer to the caller-provided slice so callers can retain their own
// catalog model without coupling it to this package.
type Spec struct {
	Name         string
	GPUs         Predicate
	Quantization string
}

// Unsupported is a model-level denial rule. An empty Quantization applies to
// every spec; otherwise it only denies specs with that quantization.
type Unsupported struct {
	GPUs         Predicate
	Quantization string
	Reason       string
}

type unsupportedJSON struct {
	GPUs         Predicate `json:"gpus"`
	Quantization string    `json:"quantization"`
	Reason       string    `json:"reason"`
}

// ReportedGPU normalizes the hardware facts persisted from an agent into the
// one GPU identity every catalog matcher consumes. API preview and controller
// launch validation both use this conversion, so vendor inference and compute
// capability parsing cannot drift between admission and execution. The name
// is the stored one, already normalized (accelerator.NormalizeGPUName), so
// the catalog's name predicates see one spelling per model.
func ReportedGPU(name, vendor, archID, computeCapability string) GPU {
	cc, _ := strconv.ParseFloat(strings.TrimSpace(computeCapability), 64)
	return GPU{
		Name:   name,
		Vendor: accelerator.InferVendor(vendor, name, archID, computeCapability),
		CC:     cc,
		ArchID: strings.TrimSpace(archID),
	}
}

// DecodePredicate reads the persisted flat-catalog gpus object.
func DecodePredicate(raw string) (Predicate, error) {
	var predicate Predicate
	if err := json.Unmarshal([]byte(raw), &predicate); err != nil {
		return predicate, err
	}
	return predicate, nil
}

// DecodeUnsupported reads the persisted model-level denial rules.
func DecodeUnsupported(raw string) ([]Unsupported, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var rows []unsupportedJSON
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return nil, err
	}
	result := make([]Unsupported, len(rows))
	for index, row := range rows {
		result[index] = Unsupported(row)
	}
	return result, nil
}

// ValidateSpec applies both the selected spec predicate and its model-level
// unsupported rules to one concrete GPU.
func ValidateSpec(gpu GPU, spec Spec, unsupported []Unsupported) error {
	selection := Select(gpu, []Spec{spec}, unsupported)
	if selection.Matched {
		return nil
	}
	reason := selection.Reason
	if reason == "" {
		reason = "no matching inference spec"
	}
	return fmt.Errorf("%s: %s", gpu.Name, reason)
}

// Verdict is the result of evaluating one predicate. Specificity is exposed
// for diagnostics; selection uses it together with the internal predicate
// width. An exact architecture ID is more specific than a name predicate,
// which is more specific than a bounded CC range, an open CC range, or a
// vendor-only predicate.
type Verdict struct {
	Matched     bool
	Reason      string
	Specificity int
	matchedBy   string
	width       float64
}

// Selection identifies the first best matching spec. UnsupportedIndex is set
// only when every matching spec was denied; then Matched is false and Reason is
// the catalog-provided unsupported reason. Ambiguous means equally specific
// matching specs existed and the earlier catalog entry was selected.
type Selection struct {
	Matched          bool
	SpecIndex        int
	UnsupportedIndex int
	Reason           string
	MatchedBy        string
	Ambiguous        bool
}

// Evaluate reports whether gpu matches predicate. Invalid predicates are a
// non-match so an invalid catalog entry can never accidentally become usable.
// An ArchID selector is exact (case-insensitive); it does not infer architecture
// from the display name.
func Evaluate(gpu GPU, predicate Predicate) Verdict {
	gpu.Vendor = normalize(gpu.Vendor)
	predicate.Vendor = normalize(predicate.Vendor)
	if predicate.Vendor == "" {
		return Verdict{Reason: "gpus.vendor is required"}
	}
	if gpu.Vendor == "" || gpu.Vendor != predicate.Vendor {
		return Verdict{Reason: "vendor mismatch"}
	}
	if archID := strings.TrimSpace(predicate.ArchID); archID != "" && !strings.EqualFold(strings.TrimSpace(gpu.ArchID), archID) {
		return Verdict{Reason: "gpus.arch_id does not match GPU architecture"}
	}

	name, err := compileName(predicate.Name)
	if err != nil {
		return Verdict{Reason: err.Error()}
	}
	if name != nil && !name.MatchString(gpu.Name) {
		return Verdict{Reason: "gpus.name does not match GPU name"}
	}

	cc, err := parseCC(predicate.CC)
	if err != nil {
		return Verdict{Reason: err.Error()}
	}
	specificity, width := 0, math.Inf(1)
	if gpu.Vendor == "nvidia" {
		if cc.kind != ccNone && !cc.matches(gpu.CC) {
			return Verdict{Reason: cc.mismatchReason(gpu.CC)}
		}
		specificity, width = cc.specificity()
	}
	if name != nil {
		specificity = 3
	}
	if strings.TrimSpace(predicate.ArchID) != "" {
		specificity += 4
	}
	return Verdict{
		Matched:     true,
		Reason:      "predicate matched",
		Specificity: specificity,
		matchedBy:   predicateDescription(predicate),
		width:       width,
	}
}

// Select applies model-level unsupported rules and then selects the narrowest
// matching spec. A matching unsupported rule is an absolute veto for the
// applicable quantization. Equal predicates are a catalog defect: directory
// order wins deterministically and MatchedBy explicitly says ambiguous.
func Select(gpu GPU, specs []Spec, unsupported []Unsupported) Selection {
	type candidate struct {
		index   int
		verdict Verdict
	}
	candidates := make([]candidate, 0, len(specs))
	denied := make([]int, len(specs))
	for i := range denied {
		denied[i] = -1
	}

	for specIndex, spec := range specs {
		for unsupportedIndex, rule := range unsupported {
			if !quantizationApplies(rule.Quantization, spec.Quantization) {
				continue
			}
			if Evaluate(gpu, rule.GPUs).Matched {
				denied[specIndex] = unsupportedIndex
				break
			}
		}
		verdict := Evaluate(gpu, spec.GPUs)
		if verdict.Matched && denied[specIndex] == -1 {
			candidates = append(candidates, candidate{index: specIndex, verdict: verdict})
		}
	}

	if len(candidates) == 0 {
		for specIndex, unsupportedIndex := range denied {
			if unsupportedIndex < 0 {
				continue
			}
			rule := unsupported[unsupportedIndex]
			return Selection{
				SpecIndex:        specIndex,
				UnsupportedIndex: unsupportedIndex,
				Reason:           rule.Reason,
				MatchedBy: fmt.Sprintf("unsupported[%d] %s", unsupportedIndex,
					predicateDescription(rule.GPUs)),
			}
		}
		return Selection{SpecIndex: -1, UnsupportedIndex: -1, Reason: "no matching spec"}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i].verdict, candidates[j].verdict
		if left.Specificity != right.Specificity {
			return left.Specificity > right.Specificity
		}
		if left.width != right.width {
			return left.width < right.width
		}
		return candidates[i].index < candidates[j].index
	})

	winner := candidates[0]
	matchedBy := fmt.Sprintf("spec:%s %s", specs[winner.index].Name, winner.verdict.matchedBy)
	ambiguous := len(candidates) > 1 && sameSpecificity(winner.verdict, candidates[1].verdict)
	if ambiguous {
		matchedBy += fmt.Sprintf("; ambiguous=true (catalog order selected spec:%s)", specs[winner.index].Name)
	}
	return Selection{Matched: true, SpecIndex: winner.index, UnsupportedIndex: -1, MatchedBy: matchedBy, Ambiguous: ambiguous}
}

func sameSpecificity(left, right Verdict) bool {
	return left.Specificity == right.Specificity && left.width == right.width
}

func quantizationApplies(rule, spec string) bool {
	return strings.TrimSpace(rule) == "" || strings.EqualFold(strings.TrimSpace(rule), strings.TrimSpace(spec))
}

// compiledNames caches gpus.name patterns. They come from the finite set of
// catalog specs, and matching runs per card on scheduling paths, so each
// pattern is compiled once per process. Invalid patterns are not cached.
var compiledNames sync.Map // pattern -> *regexp.Regexp

func compileName(pattern string) (*regexp.Regexp, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, nil
	}
	if re, ok := compiledNames.Load(pattern); ok {
		return re.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid gpus.name regex: %w", err)
	}
	compiledNames.Store(pattern, re)
	return re, nil
}

type ccKind uint8

const (
	ccNone ccKind = iota
	ccOpenLower
	ccOpenUpper
	ccExact
	ccRange
)

type ccPredicate struct {
	kind         ccKind
	lower, upper float64
}

var ccNumberExpression = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?$`)

func parseCC(expression string) (ccPredicate, error) {
	expression = strings.TrimSpace(expression)
	if expression == "" {
		return ccPredicate{}, nil
	}
	if strings.HasPrefix(expression, ">=") {
		value, err := parseCCNumber(strings.TrimSpace(strings.TrimPrefix(expression, ">=")))
		return ccPredicate{kind: ccOpenLower, lower: value}, err
	}
	if strings.HasPrefix(expression, "<") {
		value, err := parseCCNumber(strings.TrimSpace(strings.TrimPrefix(expression, "<")))
		return ccPredicate{kind: ccOpenUpper, upper: value}, err
	}
	if strings.Count(expression, "..") == 1 {
		parts := strings.Split(expression, "..")
		lower, lowerErr := parseCCNumber(strings.TrimSpace(parts[0]))
		upper, upperErr := parseCCNumber(strings.TrimSpace(parts[1]))
		if lowerErr != nil {
			return ccPredicate{}, lowerErr
		}
		if upperErr != nil {
			return ccPredicate{}, upperErr
		}
		if lower > upper {
			return ccPredicate{}, fmt.Errorf("invalid gpus.cc range %q: lower bound exceeds upper bound", expression)
		}
		return ccPredicate{kind: ccRange, lower: lower, upper: upper}, nil
	}
	value, err := parseCCNumber(expression)
	return ccPredicate{kind: ccExact, lower: value, upper: value}, err
}

func parseCCNumber(value string) (float64, error) {
	if !ccNumberExpression.MatchString(value) {
		return 0, fmt.Errorf("invalid gpus.cc %q", value)
	}
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 {
		return 0, fmt.Errorf("invalid gpus.cc %q", value)
	}
	return number, nil
}

func (predicate ccPredicate) matches(cc float64) bool {
	switch predicate.kind {
	case ccOpenLower:
		return cc >= predicate.lower
	case ccOpenUpper:
		return cc < predicate.upper
	case ccExact:
		return cc == predicate.lower
	case ccRange:
		return cc >= predicate.lower && cc <= predicate.upper
	default:
		return true
	}
}

func (predicate ccPredicate) mismatchReason(cc float64) string {
	switch predicate.kind {
	case ccOpenLower:
		return fmt.Sprintf("compute capability %.1f is below %.1f", cc, predicate.lower)
	case ccOpenUpper:
		return fmt.Sprintf("compute capability %.1f is not below %.1f", cc, predicate.upper)
	case ccExact:
		return fmt.Sprintf("compute capability %.1f is not %.1f", cc, predicate.lower)
	case ccRange:
		return fmt.Sprintf("compute capability %.1f is outside %.1f..%.1f", cc, predicate.lower, predicate.upper)
	default:
		return "compute capability mismatch"
	}
}

func (predicate ccPredicate) specificity() (int, float64) {
	switch predicate.kind {
	case ccExact:
		return 2, 0
	case ccRange:
		return 2, predicate.upper - predicate.lower
	case ccOpenLower, ccOpenUpper:
		return 1, math.Inf(1)
	default:
		return 0, math.Inf(1)
	}
}

func predicateDescription(predicate Predicate) string {
	parts := []string{"vendor=" + normalize(predicate.Vendor)}
	if cc := strings.TrimSpace(predicate.CC); cc != "" {
		parts = append(parts, "cc="+cc)
	}
	if name := strings.TrimSpace(predicate.Name); name != "" {
		parts = append(parts, fmt.Sprintf("name=%q", name))
	}
	if archID := strings.TrimSpace(predicate.ArchID); archID != "" {
		parts = append(parts, "arch_id="+archID)
	}
	return "gpus:{" + strings.Join(parts, " ") + "}"
}

func normalize(value string) string { return strings.ToLower(strings.TrimSpace(value)) }
