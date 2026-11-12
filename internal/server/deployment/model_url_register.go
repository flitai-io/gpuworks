package deployment

import (
	"net/url"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// modelSourceEndpoint is the repository endpoint a model's source requests go
// to. Only an HTTP directory carries one: the directory URL is the repository,
// and nothing else in the row can reproduce it, so every call that hands a
// model to modelrepo or modelmeta passes it through here rather than assuming
// the source's public host.
func modelSourceEndpoint(model *store.Model) string {
	if model == nil || !strings.EqualFold(strings.TrimSpace(model.Source), modelmeta.SourceURL) {
		return ""
	}
	return strings.TrimSpace(model.SourceURL)
}

// minAgentVersionForURLModels is the first tagged release whose agent can
// download a Source="url" model. An older agent's file listing would treat the
// source as one it does not know and fail on the node with an error about the
// wrong host, so the download is refused here instead, where the message can
// say what to do.
const minAgentVersionForURLModels = "26.11.0"

// agentPredatesURLModels reports whether a node's reported agent version is
// provably older than the release that added HTTP directory downloads.
//
// Only a version that parses as a release is ever judged. A development build
// reports something that is not a release number at all ("dev", a bare commit
// id, an empty string on a node that has not said hello yet), and there is no
// way to order such a build against a release — including the build of this
// very tree, which does carry the feature. Refusing what cannot be ordered
// would make the feature impossible to run outside a tagged release, so an
// unparseable version is allowed through and left to fail, if it is genuinely
// old, with the agent's own listing error.
func agentPredatesURLModels(agentVersion string) bool {
	agent, ok := parseReleaseVersion(agentVersion)
	if !ok {
		return false
	}
	minimum, ok := parseReleaseVersion(minAgentVersionForURLModels)
	if !ok {
		return false
	}
	for index := range agent {
		if agent[index] != minimum[index] {
			return agent[index] < minimum[index]
		}
	}
	return false
}

// parseReleaseVersion reads the major.minor.patch prefix of a release version.
// A build suffix ("1.2.0-14-gabc1234", produced by git describe after a tag)
// describes a build after that tag, so it orders with the tag it names.
func parseReleaseVersion(value string) ([3]int, bool) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "v")
	if cut := strings.IndexAny(value, "-+"); cut >= 0 {
		value = value[:cut]
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var parsed [3]int
	for index, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return [3]int{}, false
		}
		parsed[index] = number
	}
	return parsed, true
}

// AdmitModelDownloadNode refuses a download onto a node whose agent cannot
// perform it. Registration is never gated: it is a global act that names no
// node, and an operator must be able to register a model before the nodes that
// will hold it are upgraded.
func AdmitModelDownloadNode(model *store.Model, node *store.Node) error {
	if model == nil || node == nil || modelSourceEndpoint(model) == "" {
		return nil
	}
	if !agentPredatesURLModels(node.AgentVersion) {
		return nil
	}
	return productError(messages.ErrorModelSourceRequiresNewerAgent,
		"the node's agent is too old to download a model from an HTTP directory; upgrade the agent first",
		map[string]any{"node": node.UUID, "agent_version": node.AgentVersion, "required_version": minAgentVersionForURLModels})
}

// modelSourceHost names a model's source for a person when the provider name
// does not: every HTTP directory reports the provider "url", and only the host
// distinguishes the mirror one deployment downloads from.
func modelSourceHost(model *store.Model) string {
	endpoint := modelSourceEndpoint(model)
	if endpoint == "" {
		return ""
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return parsed.Host
}
