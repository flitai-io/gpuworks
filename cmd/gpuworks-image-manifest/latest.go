package main

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/imagecatalog/manifest"
)

var registryDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Only the engine version is decoded here for ordering. Admission of the
// complete tag always goes through the generator's release grammar.
var engineOrder = regexp.MustCompile(`^(1cat|b)?([0-9]+(?:\.[0-9]+)*)(?:(rc|\.dev|\.post)([0-9]+))?$`)

type runnerRelease struct {
	tier, variant           string
	numbers                 []int
	stage, serial, revision int
}

func readRunnerRelease(name string) (runnerRelease, error) {
	id, rev, ok := strings.Cut(name, "-r")
	if !ok || !releaseTagPattern.MatchString(name) {
		return runnerRelease{}, fmt.Errorf("invalid runner release %q", name)
	}
	tag, err := parseTag(id)
	if err != nil {
		return runnerRelease{}, err
	}
	v := engineOrder.FindStringSubmatch(tag.EngineVersion)
	if v == nil {
		return runnerRelease{}, fmt.Errorf("%s: cannot order engine version %q", name, tag.EngineVersion)
	}
	r := runnerRelease{tier: tag.Backend + tag.FrameworkVersion + "-" + tag.Model, variant: tag.Engine + "-" + v[1]}
	for _, part := range strings.Split(v[2], ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return r, err
		}
		r.numbers = append(r.numbers, n)
	}
	for len(r.numbers) > 1 && r.numbers[len(r.numbers)-1] == 0 {
		r.numbers = r.numbers[:len(r.numbers)-1]
	}
	r.stage = map[string]int{".dev": 0, "rc": 1, "": 2, ".post": 3}[v[3]]
	if v[4] != "" {
		r.serial, err = strconv.Atoi(v[4])
		if err != nil {
			return r, err
		}
	}
	r.revision, err = strconv.Atoi(rev)
	return r, err
}

func (r runnerRelease) compare(other runnerRelease) int {
	return cmp.Or(slices.Compare(r.numbers, other.numbers), cmp.Compare(r.stage, other.stage),
		cmp.Compare(r.serial, other.serial), cmp.Compare(r.revision, other.revision))
}

func preferRunner(r runnerRelease, v manifest.ManifestVersion, old runnerRelease, previous manifest.ManifestVersion) bool {
	if order := r.compare(old); order != 0 {
		return order > 0
	}
	if v.Arch != previous.Arch {
		return v.Arch == "multi"
	}
	return v.Name < previous.Name
}

// Choose one immutable release per hardware tier and CPU architecture. When
// one index wins both architectures it contributes just one catalog row.
func latestPlatformVersions(versions []manifest.ManifestVersion, pins map[string]bool) ([]manifest.ManifestVersion, error) {
	type slot struct{ tier, variant, arch string }
	type winner struct {
		index   int
		release runnerRelease
	}
	best := make(map[slot]winner)
	for i, v := range versions {
		if v.AutoSelect == "manual" {
			continue
		}
		r, err := readRunnerRelease(v.Name)
		if err != nil {
			return nil, err
		}
		arches := []string{v.Arch}
		if v.Arch == "multi" {
			arches = []string{"amd64", "arm64"}
		}
		for _, a := range arches {
			key := slot{r.tier, r.variant, a}
			old, exists := best[key]
			if !exists || preferRunner(r, v, old.release, versions[old.index]) {
				best[key] = winner{i, r}
			}
		}
	}
	arches := make([][]string, len(versions))
	for slot, w := range best {
		arches[w.index] = append(arches[w.index], slot.arch)
	}
	out := make([]manifest.ManifestVersion, 0, len(versions))
	for i, v := range versions {
		switch {
		case len(arches[i]) == 0:
			if v.AutoSelect != "manual" && !pins[v.Name] {
				continue
			}
			v.AutoSelect = "manual"
		case v.AutoSelect == "" && v.Arch == "multi" && len(arches[i]) == 1:
			v.AutoSelect = arches[i][0]
		}
		out = append(out, v)
	}
	return out, nil
}
