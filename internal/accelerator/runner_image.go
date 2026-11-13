package accelerator

import (
	"regexp"
	"strings"
)

// runnerRepository is the repository name every built-in runner image is
// published under, on every registry that mirrors it.
const runnerRepository = "gpuworks-runner"

// runnerAscendModel reads the device model segment of a CANN runner tag. The
// tag grammar is <backend><framework version>[-<model>]-<engine><version>[-rN]
// (cmd/gpuworks-image-manifest owns it); only the Ascend models are read here.
// A tag with no model segment is a generic image and has no family.
var runnerAscendModel = regexp.MustCompile(`^cann[0-9]+(?:\.[0-9]+)*-(910b|a2|a3|910c|310p|950)-`)

// RunnerImageChipFamily is the chip family a built-in runner image is built
// for, in the same vocabulary as Arch.Family ("910b", "910c", "310p"), or ""
// when the reference says none: a generic image (no model segment), another
// vendor's image, or a reference outside the gpuworks-runner repository.
//
// The family is derived from the tag on purpose. The catalog row keeps the
// model only in its notes, and a second stored copy of what the reference
// already says could drift from it.
//
// The Ascend 950 images name their family "950", but ascendFamily does not
// know a 950 card's chip identity yet, so no node reports that family and a
// 950 image is never excluded for a node (nor preferred for one) until it
// does.
func RunnerImageChipFamily(imageRef string) string {
	ref := strings.TrimSpace(imageRef)
	if at := strings.IndexByte(ref, '@'); at >= 0 {
		ref = ref[:at]
	}
	slash := strings.LastIndexByte(ref, '/')
	repoTag := ref[slash+1:]
	repo, tag, found := strings.Cut(repoTag, ":")
	if !found || !strings.EqualFold(repo, runnerRepository) {
		return ""
	}
	match := runnerAscendModel.FindStringSubmatch(strings.ToLower(tag))
	if match == nil {
		return ""
	}
	switch match[1] {
	case "910b", "a2":
		return "910b"
	case "a3", "910c":
		return "910c"
	case "310p":
		return "310p"
	case "950":
		return "950"
	default:
		return ""
	}
}
