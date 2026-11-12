package deployment

import "gpuworks.ai/gpuworks/internal/server/imagecatalog"

// templateOwnsImage reports whether a deployment that runs image runs the
// selected template's own image. It is the one test for "the template's image"
// on the deploy path: an image the template names is trusted as an override,
// and only that image gets the sources the template declares for it.
func templateOwnsImage(selection templateSelection, image string) bool {
	return selection.Spec != nil && imagecatalog.SameImageRef(selection.Spec.Image, image)
}

// templateImageSources are the further pull references the selected template
// declares for its image, offered only while the deployment runs that image
// (ADR 0038). A request that overrides the image runs something the template
// did not describe, so it gets the platform's own sources for that image, not
// the template's. The deployment snapshot records what this returns, so a
// deployment created with an override never carries the template's sources.
func templateImageSources(selection templateSelection, image string) []string {
	if !templateOwnsImage(selection, image) {
		return nil
	}
	return selection.ImageSources
}
