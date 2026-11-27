package api

import (
	"net/http"
	"net/url"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/kubemanifest"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// kubePrecheckFileName is what the console saves the pre-check script as.
const kubePrecheckFileName = "gpuworks-precheck.sh"

// kubePrecheck renders the pre-check for the fields of a cluster form, before
// any entry exists: the script starts the agent image on every node the
// agents would run on and says per node whether it pulled the image and
// reached the server. It needs no token and stores nothing.
func (a *apiHandlers) kubePrecheck(w http.ResponseWriter, r *http.Request) {
	var req dto.KubePrecheckRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	// The same values, and the same defaults, an entry made from this form
	// would have.
	c := store.KubeCluster{
		Namespace:        firstNonEmpty(strings.TrimSpace(req.Namespace), kubeDefaultNamespace),
		ServerURL:        strings.TrimRight(strings.TrimSpace(req.ServerURL), "/"),
		AgentImage:       strings.TrimSpace(req.AgentImage),
		AgentImageSource: firstNonEmpty(strings.TrimSpace(req.AgentImageSource), kubemanifest.DefaultAgentImageSource),
		NodeSelector:     strings.TrimSpace(req.NodeSelector),
		ImagePullSecret:  strings.TrimSpace(req.ImagePullSecret),
	}
	if !kubemanifest.ValidAgentImageSource(c.AgentImageSource) {
		kubeInvalid(w, "agent_image_source", "must be one of "+strings.Join(kubemanifest.AgentImageSources, ", "))
		return
	}
	if c.ImagePullSecret != "" && !kubemanifest.ValidSecretName(c.ImagePullSecret) {
		kubeInvalid(w, "image_pull_secret", "must be the name of a Secret in the namespace")
		return
	}
	sel, err := kubemanifest.ParseNodeSelector(c.NodeSelector)
	if err != nil {
		kubeInvalid(w, "node_selector", err.Error())
		return
	}
	server, err := kubemanifest.ServerAddress(c.ServerURL)
	if err != nil || !strings.HasPrefix(c.ServerURL, "http://") && !strings.HasPrefix(c.ServerURL, "https://") {
		kubeInvalid(w, "server_url", "must be an http:// or https:// address")
		return
	}
	image := kubeAgentImage(c)
	script, err := kubemanifest.Precheck(kubemanifest.Params{
		Namespace: c.Namespace, ServerURL: c.ServerURL, AgentImage: image,
		NodeSelector: sel, ImagePullSecret: c.ImagePullSecret,
	})
	if err != nil {
		kubeInvalid(w, "manifest", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, dto.KubePrecheck{Script: script, FileName: kubePrecheckFileName, Image: image, Server: server})
}

// kubeCheckByToken serves the check after an apply to whoever holds the
// entry's enrollment token, as the manifest is served, so that the command
// next to `kubectl apply` needs no login.
func (a *apiHandlers) kubeCheckByToken(w http.ResponseWriter, r *http.Request) {
	c, ok := a.kubeClusterOfToken(w, r)
	if !ok {
		return
	}
	script, err := kubemanifest.Verify(c.Namespace)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(script))
}

// kubeVerifyCommand fetches the check after an apply and runs it with bash.
func kubeVerifyCommand(c store.KubeCluster) string {
	u := strings.TrimRight(c.ServerURL, "/") + "/kube/check.sh?token=" + url.QueryEscape(c.Token)
	return "curl -fsSL '" + u + "' | bash"
}

func kubeCheckCodes() dto.KubeCheckCodes {
	str := func(in []kubemanifest.CheckCode) []string {
		out := make([]string, len(in))
		for i, c := range in {
			out[i] = string(c)
		}
		return out
	}
	return dto.KubeCheckCodes{Precheck: str(kubemanifest.PrecheckCodes), Verify: str(kubemanifest.VerifyCodes)}
}
