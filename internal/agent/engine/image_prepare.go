package engine

import (
	"context"
	"errors"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/imagepull"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// imageDigest is the "sha256:<hex>" a reference pins, or "" for a tag-only
// reference. Both "repo@sha256:..." and "repo:tag@sha256:..." pin it.
func imageDigest(ref string) string {
	at := strings.LastIndexByte(ref, '@')
	if at < 0 {
		return ""
	}
	digest := strings.ToLower(strings.TrimSpace(ref[at+1:]))
	if !strings.HasPrefix(digest, "sha256:") || len(digest) == len("sha256:") {
		return ""
	}
	return digest
}

// localImageRefs are the references under which a present image satisfies spec.
// spec.Image always does. For a digest-pinned spec every pull candidate does
// too: the candidates are what the server (and the node's own network rules)
// chose as sources of this image, and a runtime records a digest pull under the
// registry name it came from and cannot tag a digest reference, so an image
// fetched from a source other than the canonical name never appears under the
// canonical name.
func localImageRefs(spec proto.InstanceDesiredState, candidates []string) []string {
	refs := []string{spec.Image}
	if imageDigest(spec.Image) == "" {
		return refs
	}
	for _, ref := range candidates {
		if ref != spec.Image {
			refs = append(refs, ref)
		}
	}
	return refs
}

// firstLocalImage returns the first of refs the runtime holds. A failed check
// proves nothing about absence, so the first error is returned alongside
// found=false for the caller to log and treat as absent.
func firstLocalImage(ctx context.Context, driver runtime.Driver, refs []string) (ref string, found bool, err error) {
	var firstErr error
	for _, ref := range refs {
		present, err := driver.ImageExists(ctx, ref)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if present {
			return ref, true, nil
		}
	}
	return "", false, firstErr
}

// prepareImage makes the instance's image available locally and returns the
// reference the container must be launched from, reporting progress and the
// final failure itself. The candidates are authoritative: the server states
// where an image may come from (a template declares its own sources, and the
// user picks one), so this does not second-guess a candidate's content. The
// launch reference is spec.Image except when a digest-pinned spec.Image was
// satisfied by a candidate (already present, or pulled): that candidate is
// launched as is, since a runtime cannot tag a digest name and the canonical
// name therefore cannot exist locally. A tag-referenced spec.Image is tagged
// from whichever candidate was pulled.
func (e *Engine) prepareImage(ctx context.Context, driver runtime.Driver, spec proto.InstanceDesiredState, name string) (string, bool) {
	candidates, serverPlanned := e.pullCandidates(spec)
	localRefs := localImageRefs(spec, candidateRefs(candidates))
	pinned := imageDigest(spec.Image)

	// Keep the local image check visible: it can be slow on large udocker
	// stores, and its zero-byte result must not look like a stalled download.
	e.reportStateProgress(spec, proto.InstPulling, name, "", "checking whether the image is already available", proto.PhaseImageVerify, 0, 0, 0, 0)
	local, found, imageCheckErr := firstLocalImage(ctx, driver, localRefs)
	if imageCheckErr != nil {
		e.log.Warn("runtime image check failed", "instance", spec.UUID, "image", spec.Image, "runtime", driver.Name(), "phase", "provision", "err", imageCheckErr)
	}
	if found {
		e.reportStateProgress(spec, proto.InstPulling, name, "", "image is already available locally"+launchNote(spec, local), proto.PhaseImageVerify, 1, 0, 0, 0)
		return local, true
	}
	if errors.Is(imageCheckErr, runtime.ErrRuntimeUnavailable) {
		// No source can help: the runtime itself is not usable on this node.
		e.failRuntimeUnavailable(spec, imageCheckErr)
		return "", false
	}
	if imageCheckErr == nil && e.importFromOtherRuntime(ctx, driver, spec, name) {
		return spec.Image, true
	}
	message := "image is not available locally; download required"
	if imageCheckErr != nil {
		message = "image availability check failed; download required"
	}
	e.reportStateProgress(spec, proto.InstPulling, name, "", message, proto.PhaseImageVerify, 1, 0, 0, 0)

	launch := ""
	res, walkErr := e.puller.Walk(ctx, driver, candidates, e.pullOptions(spec, serverPlanned), imagepull.Hooks{
		Ordered: func(summary string) {
			e.reportStateProgress(spec, proto.InstPulling, name, "", "image sources by probe: "+summary, proto.PhaseImageVerify, 1, 0, 0, 0)
		},
		Attempt: func(ref string) {
			e.reportStateProgress(spec, proto.InstPulling, name, "", "pulling "+ref, proto.PhaseImagePull, 0, 0, 0, 0)
		},
		Line: func(ref, line string, s imagepull.Snapshot, changed bool) {
			if !changed {
				return
			}
			e.reportStateProgress(spec, proto.InstPulling, name, "", pullMessage(ref, s), s.Phase, s.Fraction, s.Done, s.Total, s.Rate)
		},
		Heartbeat: func(ref string, s imagepull.Snapshot) {
			phase := s.Phase
			if phase == "" {
				phase = proto.PhaseImagePull
			}
			e.reportStateProgress(spec, proto.InstPulling, name, "", pullMessage(ref, s), phase, s.Fraction, s.Done, s.Total, s.Rate)
		},
		Failed: func(ref string, err error) {
			e.log.Warn("image preparation failed", "instance", spec.UUID, "image", spec.Image, "runtime", driver.Name(), "phase", "pull", "ref", ref, "err", err)
			if errors.Is(err, runtime.ErrRuntimeUnavailable) {
				return
			}
			// Each attempted source has to stay visible on its own. The
			// next candidate reports the same state and phase, so without
			// a phase of its own the failed attempt would be folded into
			// the attempt that follows it and disappear.
			e.reportStateProgress(spec, proto.InstPulling, name, "", "image source failed: "+ref+": "+err.Error(), proto.PhaseImagePullRetry, 0, 0, 0, 0)
		},
		Pulled: func(ctx context.Context, ref string) error {
			if ref == spec.Image || pinned != "" {
				// Either the canonical name itself, or a candidate of a
				// digest-pinned image: the pulled name is the launch reference.
				launch = ref
				return nil
			}
			tagger, ok := driver.(interface {
				TagImage(context.Context, string, string) error
			})
			if !ok {
				return errors.New("runtime does not support canonical image tagging")
			}
			if err := tagger.TagImage(ctx, ref, spec.Image); err != nil {
				return errors.New("canonical image tag failed: " + err.Error())
			}
			launch = spec.Image
			return nil
		},
		// A pull can fail after it delivered what the instance needs.
		Recovered: func(ctx context.Context) (string, bool) {
			ref, ok, _ := firstLocalImage(ctx, driver, localRefs)
			return ref, ok
		},
	})
	if walkErr != nil {
		if errors.Is(walkErr, runtime.ErrRuntimeUnavailable) {
			e.failRuntimeUnavailable(spec, walkErr)
		} else {
			e.reportState(spec, proto.InstFailed, "", "", walkErr.Error())
		}
		return "", false
	}
	if res.Recovered {
		launch = res.Ref
	}

	// Pull completion is a separate user-visible stage. Some runtimes do not
	// expose a stronger verification primitive, so ImageExists is best effort.
	e.reportStateProgress(spec, proto.InstPulling, name, "", "verifying downloaded image", proto.PhaseImageVerify, 0, 0, 0, 0)
	verified, verifyErr := driver.ImageExists(ctx, launch)
	note := launchNote(spec, launch)
	if verifyErr != nil || !verified {
		message := "download complete; runtime did not provide image verification"
		if verifyErr != nil {
			message = "download complete; image verification unavailable: " + verifyErr.Error()
		}
		e.reportStateProgress(spec, proto.InstPulling, name, "", message+note, proto.PhaseImageVerify, 1, 0, 0, 0)
	} else {
		e.reportStateProgress(spec, proto.InstPulling, name, "", "downloaded image verified"+note, proto.PhaseImageVerify, 1, 0, 0, 0)
	}
	return launch, true
}

// failRuntimeUnavailable reports an instance that cannot start because its
// runtime could not be set up on the node, naming the runtime and the cause.
func (e *Engine) failRuntimeUnavailable(spec proto.InstanceDesiredState, err error) {
	e.reportState(spec, proto.InstFailed, "", "", err.Error())
}

// launchNote names the reference a container is launched from when that is not
// the instance's own image, so the progress stream shows which source the image
// actually came from; the container's image id is observed separately once it
// runs.
func launchNote(spec proto.InstanceDesiredState, launch string) string {
	if launch == "" || launch == spec.Image {
		return ""
	}
	return "; launching from " + launch
}

// pullMessage is what a replica says while its image downloads: the source it
// pulls from, which is what the console shows as where the image comes from.
// The byte counters carry the progress; a runtime line follows the source only
// when there are none. Extraction speaks for itself.
func pullMessage(ref string, s imagepull.Snapshot) string {
	switch {
	case s.Phase == proto.PhaseImageExtract && s.Message != "":
		return s.Message
	case s.Total > 0 || s.Message == "":
		return "pulling " + ref
	default:
		return "pulling " + ref + ": " + s.Message
	}
}
