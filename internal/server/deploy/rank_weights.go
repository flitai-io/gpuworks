// rank_weights.go owns rank-scoped weights (ADR 0076): a multi-node spec whose
// weight source names the rank in its subfolder, so each rank of a group reads
// only its own part of one repository.

package deploy

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	placementeval "gpuworks.ai/gpuworks/internal/server/placement"
)

// subfolderReference is any ${...} in a weight subfolder. It is wider than the
// launch fields' variable pattern on purpose: a subfolder is a literal path
// apart from the rank, so every other reference, however it is spelled, is
// refused rather than left in the path.
var subfolderReference = regexp.MustCompile(`\$\{([^}]*)\}`)

// rankReference reports whether a reference names the rank under either
// spelling the launch fields accept: the documented ${GPUWORKS_NODE_RANK} or
// its older alias ${NODE_RANK} (placement.Canonical is the one alias rule).
func rankReference(name string) bool {
	return placementeval.Canonical(name) == placementeval.VarNodeRank
}

// RankScopedSubfolder reports whether a weight source's subfolder names the
// rank, as in "rank${GPUWORKS_NODE_RANK}".
func RankScopedSubfolder(subfolder string) bool {
	for _, match := range subfolderReference.FindAllStringSubmatch(subfolder, -1) {
		if rankReference(match[1]) {
			return true
		}
	}
	return false
}

// UnknownSubfolderVariable returns the first ${...} in subfolder that is not
// the rank variable, "" when there is none. The rank is the only fact a
// subfolder may depend on: it is known before anything is downloaded.
func UnknownSubfolderVariable(subfolder string) string {
	for _, match := range subfolderReference.FindAllStringSubmatch(subfolder, -1) {
		if !rankReference(match[1]) {
			return match[0]
		}
	}
	return ""
}

// ExpandRankSubfolder is the subfolder rank reads. A subfolder that does not
// name the rank is returned unchanged.
func ExpandRankSubfolder(subfolder string, rank int) string {
	return subfolderReference.ReplaceAllStringFunc(subfolder, func(match string) string {
		if rankReference(subfolderReference.FindStringSubmatch(match)[1]) {
			return strconv.Itoa(rank)
		}
		return match
	})
}

// RankSubfolderMatches reports whether actual is the subfolder some rank of the
// rank-scoped template reads ("rank${GPUWORKS_NODE_RANK}" matches "rank3",
// not "rank3/x" or the whole repository).
func RankSubfolderMatches(template, actual string) bool {
	if !RankScopedSubfolder(template) {
		return template == actual
	}
	_, ok := RankOfSubfolder(template, actual)
	return ok
}

// RankOfSubfolder is the rank whose expansion of template is actual.
func RankOfSubfolder(template, actual string) (int, bool) {
	var pattern strings.Builder
	last := 0
	for _, bounds := range subfolderReference.FindAllStringSubmatchIndex(template, -1) {
		pattern.WriteString(regexp.QuoteMeta(template[last:bounds[0]]))
		if rankReference(template[bounds[2]:bounds[3]]) {
			pattern.WriteString(`([0-9]+)`)
		} else {
			pattern.WriteString(regexp.QuoteMeta(template[bounds[0]:bounds[1]]))
		}
		last = bounds[1]
	}
	pattern.WriteString(regexp.QuoteMeta(template[last:]))
	match := regexp.MustCompile(`^` + pattern.String() + `$`).FindStringSubmatch(actual)
	if len(match) < 2 {
		return 0, false
	}
	rank, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, false
	}
	// Every reference names the same rank: the expansion is the proof.
	return rank, ExpandRankSubfolder(template, rank) == actual
}

// RankWeight binds one rank of a deployment with rank-scoped weights to the
// model artifact it mounts: the template's subfolder expanded for the rank,
// resolved once at create like a component's StoreModelID and never again from
// the mutable catalog.
type RankWeight struct {
	Rank         int    `json:"rank"`
	Subfolder    string `json:"subfolder"`
	StoreModelID uint   `json:"store_model_id"`
}

// RankWeightModelID is the model rank mounts in place of the deployment's own,
// and false when the selection's weights are not rank-scoped or bind no such
// rank.
func (s CatalogSelection) RankWeightModelID(rank int) (uint, bool) {
	for _, weight := range s.RankWeights {
		if weight.Rank == rank && weight.StoreModelID != 0 {
			return weight.StoreModelID, true
		}
	}
	return 0, false
}

// RankScoped reports whether the selection's own weights are rank-scoped.
func (s CatalogSelection) RankScoped() bool { return len(s.RankWeights) > 0 }

// ReadsByRank reports whether what a node reads depends on its rank: the
// selection's own weights or some component's are rank-scoped.
func (s CatalogSelection) ReadsByRank() bool { return len(s.BoundRanks()) > 0 }

// BoundRanks are the ranks a rank-scoped binding names, the selection's own
// weights' and every component's, in ascending order.
func (s CatalogSelection) BoundRanks() []int {
	seen := map[int]bool{}
	var ranks []int
	add := func(weights []RankWeight) {
		for _, weight := range weights {
			if !seen[weight.Rank] {
				seen[weight.Rank] = true
				ranks = append(ranks, weight.Rank)
			}
		}
	}
	add(s.RankWeights)
	for _, component := range s.Components {
		add(component.RankWeights)
	}
	slices.Sort(ranks)
	return ranks
}
