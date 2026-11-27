package main

import (
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
)

// A template keeps its tested tag and the pull sources on that catalog row.
// Other repositories continue to take their sources from the template itself.
var templateRunnerTags = sync.OnceValues(func() (map[string]bool, error) {
	catalog, err := seed.Catalog()
	if err != nil {
		return nil, err
	}
	repos := append([]string{DefaultRegistry, "docker.io/" + DefaultRegistry}, publishedRegistries...)
	tags := map[string]bool{}
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			for _, ref := range append([]string{spec.Image}, spec.ImageSources...) {
				for _, repo := range repos {
					if tag, ok := strings.CutPrefix(ref, repo+":"); ok {
						tags[tag] = true
					}
				}
			}
		}
	}
	return tags, nil
})
