package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/kubemanifest"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// updateKubeCluster edits an entry's settings. The cluster keeps running the
// manifest it was given until that is applied again; the answer says
// whether that is needed and, for an entry made by hand, with what.
func (a *apiHandlers) updateKubeCluster(w http.ResponseWriter, r *http.Request) {
	// Unknown fields are refused: a namespace, server URL or state
	// directory sent here would otherwise look accepted.
	var req dto.KubeClusterUpdate
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	old, err := a.d.Store.KubeClusterByUUID(chi.URLParam(r, "uuid"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	c := *old
	text := func(dst *string, v *string) {
		if v != nil {
			*dst = strings.TrimSpace(*v)
		}
	}
	text(&c.Name, req.Name)
	text(&c.NodeSelector, req.NodeSelector)
	text(&c.AgentImage, req.AgentImage)
	text(&c.AgentImageSource, req.AgentImageSource)
	text(&c.ImagePullSecret, req.ImagePullSecret)
	text(&c.RuntimeClass, req.RuntimeClass)
	text(&c.ContainerdSocket, req.ContainerdSocket)
	text(&c.APIServer, req.APIServer)
	text(&c.EnginePorts, req.EnginePorts)
	c.EnginePorts = firstNonEmpty(c.EnginePorts, kubeDefaultEnginePorts)
	if req.IdleReturnSeconds != nil {
		c.IdleReturnSeconds = *req.IdleReturnSeconds
	}
	if req.Priority != nil {
		c.Priority = *req.Priority
	}
	if req.AllowPreemption != nil {
		c.AllowPreemption = *req.AllowPreemption
	}
	if c.ProviderID != nil && c.NodeSelector != old.NodeSelector {
		kubeInvalid(w, "node_selector", "an entry made from a cloud cluster selects its nodes by node pool")
		return
	}
	pools, err := a.d.Store.KubeClusterPools(c.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !kubeValid(w, c, pools[c.ID]) {
		return
	}
	before, err := renderKubeManifest(*old, pools[c.ID])
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// Instances on nodes the new selection leaves out are asked to stop in
	// the transaction that saves the change, after everything above and the
	// name check inside it: a change that fails stops nothing (kube_selection_stop.go).
	leave, ok := a.settleEntrySelection(w, r, *old, c, pools[c.ID], pools[c.ID], req.ConfirmDropInstances)
	if !ok {
		return
	}
	updated, stoppedRows, err := a.d.Store.UpdateKubeClusterLeaving(c, leave)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
		return
	case errors.Is(err, store.ErrKubeClusterNameTaken):
		writeProductError(w, http.StatusConflict, ErrorKubeClusterNameTaken, "another cluster is named "+c.Name, map[string]any{"name": c.Name})
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	stopped := kubeStoppedDTO(stoppedRows)
	a.logKubeStopped(updated.Name, stopped, leave)
	after, err := renderKubeManifest(*updated, pools[c.ID])
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	changed := kubeChangedFields(*old, *updated)
	if a.d.Log != nil && len(changed) > 0 {
		a.d.Log.Info("kubernetes cluster edited", "cluster", updated.Name, "changed", strings.Join(changed, ","))
	}
	a.pushKubeLiveSettings(updated.ID, changed)
	views, err := a.kubeEntryViews(r.Context(), []store.KubeCluster{*updated}, true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := dto.KubeClusterEdit{Cluster: kubeClusterDTO(*updated, views[updated.ID]), Changed: changed, Reapply: before != after, Commands: []string{},
		StoppedInstances: stopped, Warnings: []messages.Warning{}}
	if slices.Contains(changed, "engine_ports") {
		// Running instances keep their ports; the ones outside the new range are reported, not refused.
		gone := make(map[string]bool, len(stopped))
		for _, s := range stopped {
			gone[s.InstanceUUID] = true
		}
		if out.Warnings, err = a.kubeEnginePortsWarnings(*updated, gone); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	if out.Reapply && updated.ProviderID == nil && updated.RevokedAt == nil {
		// The PriorityClass goes first whatever this edit changed: an earlier
		// edit of its value may not have been applied, and Kubernetes does not
		// change it in place. One line, so no time passes without the class
		// between the two.
		out.Commands = append(out.Commands, "kubectl delete priorityclass "+kubemanifest.PriorityClass(updated.Namespace)+" --ignore-not-found && "+out.Cluster.ApplyCommand)
	}
	writeJSON(w, http.StatusOK, out)
}

func renderKubeManifest(c store.KubeCluster, pools []store.KubeClusterPool) (string, error) {
	params, err := kubeManifestParams(c, pools)
	if err != nil {
		return "", err
	}
	return kubemanifest.Render(params)
}

// kubeChangedFields names, by their JSON names and sorted, the editable
// settings that differ between two versions of an entry.
func kubeChangedFields(a, b store.KubeCluster) []string {
	out := []string{}
	add := func(name string, differ bool) {
		if differ {
			out = append(out, name)
		}
	}
	add("name", a.Name != b.Name)
	add("node_selector", a.NodeSelector != b.NodeSelector)
	add("agent_image", a.AgentImage != b.AgentImage)
	add("agent_image_source", a.AgentImageSource != b.AgentImageSource)
	add("image_pull_secret", a.ImagePullSecret != b.ImagePullSecret)
	add("runtime_class", a.RuntimeClass != b.RuntimeClass)
	add("containerd_socket", a.ContainerdSocket != b.ContainerdSocket)
	add("api_server", a.APIServer != b.APIServer)
	add("engine_ports", a.EnginePorts != b.EnginePorts)
	add("idle_return_seconds", a.IdleReturnSeconds != b.IdleReturnSeconds)
	add("priority", a.Priority != b.Priority)
	add("allow_preemption", a.AllowPreemption != b.AllowPreemption)
	sort.Strings(out)
	return out
}
