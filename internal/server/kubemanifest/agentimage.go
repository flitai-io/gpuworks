package kubemanifest

// AgentImageRevision is the agent image's runtime revision, the tag the
// manifest names (build/agent-image/REVISION, which a test holds this to).
// The image is the agent's runtime only: the agents run from their nodes'
// state directories and are upgraded there, so the tag changes only when the
// runtime does. Moving it moves every cluster's DaemonSet to the new image
// on the node's next upgrade.
const AgentImageRevision = "r1"

// The registries the agent image is published to (the release pipeline's
// release-agent-image job), by the id a cluster entry records.
const (
	AgentImageDockerHub = "dockerhub"
	AgentImageSWR       = "swr"
	AgentImageQuay      = "quay"
)

// AgentImageSources lists the registries in the order they are offered,
// the default first.
var AgentImageSources = []string{AgentImageDockerHub, AgentImageSWR, AgentImageQuay}

var agentImageRepos = map[string]string{
	AgentImageDockerHub: "gpuworks/gpuworks-agent",
	AgentImageSWR:       "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-agent",
	AgentImageQuay:      "quay.io/gpuworks/gpuworks-agent",
}

// ValidAgentImageSource says whether source names a registry; empty is the
// default's.
func ValidAgentImageSource(source string) bool {
	_, ok := agentImageRepos[source]
	return ok || source == ""
}

// AgentImage is the published agent image at source, empty meaning Docker
// Hub.
func AgentImage(source string) string {
	repo, ok := agentImageRepos[source]
	if !ok {
		repo = agentImageRepos[AgentImageDockerHub]
	}
	return repo + ":" + AgentImageRevision
}
