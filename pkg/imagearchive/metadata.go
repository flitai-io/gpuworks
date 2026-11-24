package imagearchive

import (
	"encoding/json"
	"strings"
)

// ImageMetadata contains only catalog fields declared by the image config.
type ImageMetadata struct {
	Engine           string `json:"engine,omitempty"`
	EngineVersion    string `json:"engine_version,omitempty"`
	Framework        string `json:"framework,omitempty"`
	FrameworkVersion string `json:"framework_version,omitempty"`
	Arch             string `json:"arch,omitempty"`
}

// Contents associates each tagged reference with its own image configuration.
type Contents struct {
	RepoTags []string `json:"repo_tags"`

	MetadataByTag map[string]ImageMetadata `json:"metadata_by_tag,omitempty"`
}

type manifestEntry struct {
	Config   string
	RepoTags []string
}

type descriptor struct {
	Digest      string
	Annotations map[string]string
}

type archiveMetadata struct {
	budgetLimited bool

	docker        []manifestEntry
	dockerConfigs map[string]bool
	oci           []descriptor
	// OCI references point to a manifest, which points to a config blob.
	configPaths map[string]string
	configs     map[string]ImageMetadata
}

func blobPath(digest string) string {
	algorithm, sum, ok := strings.Cut(digest, ":")
	if !ok || algorithm == "" || sum == "" {
		return ""
	}
	return "blobs/" + algorithm + "/" + sum
}

func (m *archiveMetadata) setDockerManifest(data []byte) {
	m.docker = dockerManifest(data)
	m.dockerConfigs = make(map[string]bool)
	for _, entry := range m.docker {
		if len(entry.RepoTags) > 0 && entry.Config != "" {
			m.dockerConfigs[normalizeEntry(entry.Config)] = true
		}
	}
}

func (m *archiveMetadata) configCandidate(name string) bool {
	if len(m.dockerConfigs) > 0 {
		return m.dockerConfigs[name]
	}
	// Before the manifest, keep only candidate configs and OCI descriptors.
	return strings.HasSuffix(name, ".json") || strings.HasPrefix(name, "sha256:") || strings.HasPrefix(name, "blobs/")
}

// record reports whether a manifest or usable config was found.
func (m *archiveMetadata) record(name string, data []byte) bool {
	switch name {
	case "manifest.json":
		m.setDockerManifest(data)
	case "index.json":
		m.oci = ociDescriptors(data)
	default:
		var blob struct {
			Architecture string
			Config       struct {
				Labels map[string]string
				Digest string
			}
		}
		if json.Unmarshal(data, &blob) != nil {
			return false
		}
		if path := blobPath(blob.Config.Digest); path != "" {
			if m.configPaths == nil {
				m.configPaths = make(map[string]string)
			}
			m.configPaths[name] = path
			return true
		}
		info := MetadataFromConfig(blob.Architecture, blob.Config.Labels)
		if info == (ImageMetadata{}) {
			return false
		}
		if m.configs == nil {
			m.configs = make(map[string]ImageMetadata)
		}
		m.configs[name] = info
	}
	return true
}

func (m *archiveMetadata) contents() Contents {
	// Docker manifests may repeat a tag for different platforms. Only use a
	// config when its reference unambiguously identifies it.
	paths := make(map[string]string)
	var result Contents
	add := func(ref, path string) {
		ref = strings.TrimSpace(ref)
		// An OCI ref-name can be a bare tag; the form needs a repository too.
		name := ref[strings.LastIndexByte(ref, '/')+1:]
		repo, tag, ok := strings.Cut(name, ":")
		if !ok || repo == "" || tag == "" || strings.ContainsAny(ref, "@ \t\r\n") {
			return
		}
		previous, exists := paths[ref]
		if !exists {
			result.RepoTags = append(result.RepoTags, ref)
		} else if previous != path {
			path = ""
		}
		paths[ref] = path
	}

	for _, entry := range m.docker {
		for _, ref := range entry.RepoTags {
			add(ref, normalizeEntry(entry.Config))
		}
	}
	if len(result.RepoTags) == 0 {
		for _, manifest := range m.oci {
			add(manifest.Annotations[refNameAnnotation], m.configPaths[blobPath(manifest.Digest)])
		}
	}
	for _, ref := range result.RepoTags {
		if info, ok := m.configs[paths[ref]]; ok {
			if result.MetadataByTag == nil {
				result.MetadataByTag = make(map[string]ImageMetadata)
			}
			result.MetadataByTag[ref] = info
		}
	}
	return result
}

// MetadataFromConfig reads the catalog fields shared by archived and installed OCI images.
func MetadataFromConfig(arch string, labels map[string]string) ImageMetadata {
	return ImageMetadata{
		Arch:             arch,
		Engine:           labels["io.gpuworks.engine"],
		EngineVersion:    labels["io.gpuworks.engine.version"],
		Framework:        labels["io.gpuworks.framework"],
		FrameworkVersion: labels["io.gpuworks.framework.version"],
	}
}
