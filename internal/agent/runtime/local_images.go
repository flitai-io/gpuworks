package runtime

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

// LocalImageStore lists tagged images and resolves a selection on the node.
// Identity is the immutable ID printed by the node's CLI. The verified
// inspection supplies the identity used for node presence.
type LocalImageStore interface {
	ListLocalImages(context.Context, string) (proto.RuntimeImageList, error)
	ResolveLocalImage(context.Context, proto.RuntimeImageSelection, bool) (ImageInspection, error)
}

var (
	ErrLocalImageChanged     = errors.New("selected runtime image changed; refresh the list")
	ErrLocalImageConflict    = errors.New("the deployment namespace already contains a different image under this tag")
	ErrLocalImageUnsupported = errors.New("this runtime does not support local image discovery")
	localNamespace           = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

// OCI SHA-2 digests use lowercase hex with the algorithm-defined length.
var imageDigestPattern = regexp.MustCompile(`^(sha256:[a-f0-9]{64}|sha384:[a-f0-9]{96}|sha512:[a-f0-9]{128})$`)

func ValidImageDigest(id string) bool { return imageDigestPattern.MatchString(id) }

func ValidImageNamespace(ns string) bool { return ns == "" || localNamespace.MatchString(ns) }

func ValidLocalImageRef(ref string) bool {
	tail := ref[strings.LastIndex(ref, "/")+1:]
	name, tag, ok := strings.Cut(tail, ":")
	return ok && name != "" && tag != "" && !strings.HasPrefix(ref, "-") && !strings.ContainsAny(ref, "@<> \t\n\r") && !strings.Contains(tag, ":")
}

// Both CLIs implement these image-list template fields. Keep stderr separate
// from stdout: containerd may warn about unrelated image records.
const LocalImageListFormat = "{{.Repository}}\t{{.Tag}}\t{{.ID}}\t{{.Size}}"

func ParseLocalImages(output string) []proto.RuntimeImage {
	items := []proto.RuntimeImage{}
	seen := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		p := strings.Split(strings.TrimSuffix(line, "\r"), "\t")
		if len(p) != 4 {
			continue
		}
		ref := p[0] + ":" + p[1]
		if seen[ref] || !ValidLocalImageRef(ref) || p[0] == "" || !ValidImageDigest(p[2]) {
			continue
		}
		seen[ref] = true
		items = append(items, proto.RuntimeImage{Ref: ref, Identity: p[2], Size: p[3]})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Ref < items[j].Ref })
	return items
}
