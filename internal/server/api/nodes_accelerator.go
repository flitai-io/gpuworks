package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// updateNodeAccelerator chooses which vendor's cards a node manages, on a
// host whose bus has more than one vendor's. Nothing may run on the node:
// its cards are replaced by the other vendor's. The agent restarts to
// manage them, and the choice is stored once it has accepted.
func (a *apiHandlers) updateNodeAccelerator(w http.ResponseWriter, r *http.Request) {
	if a.d.CallNode == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorNodeRPCUnavailable, "node RPC is not configured", nil)
		return
	}
	uuid := chi.URLParam(r, "uuid")
	var req struct {
		Accelerator string `json:"accelerator"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	preference, ok := store.NormalizeAcceleratorPreference(req.Accelerator)
	if !ok {
		writeProductError(w, http.StatusBadRequest, ErrorNodeAcceleratorInvalid, "accelerator must be auto or a vendor whose cards the host has", map[string]any{"accelerator": strings.TrimSpace(req.Accelerator)})
		return
	}
	node, err := a.d.Store.NodeByUUID(uuid)
	if err != nil {
		writeNodeLookupError(w, err)
		return
	}
	// A Kubernetes node's agent manages only the vendors that run there.
	if preference != proto.AcceleratorPreferenceAuto && node.KubeClusterID != 0 && !kubeManages(preference) {
		writeProductError(w, http.StatusBadRequest, ErrorNodeAcceleratorInvalid, "that vendor's cards do not run on Kubernetes nodes", map[string]any{"accelerator": preference})
		return
	}
	if preference != proto.AcceleratorPreferenceAuto && !node.HostHasAcceleratorVendor(preference) {
		writeProductError(w, http.StatusBadRequest, ErrorNodeAcceleratorNotOnHost, "the node's host has no cards of that vendor", map[string]any{"accelerator": preference})
		return
	}
	ds, err := a.d.Store.DesiredStateForNode(node.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// What the agent would keep running, counted as its own idle check
	// counts it: an instance being removed is replayed as stopped.
	running := 0
	for _, spec := range ds.Instances {
		if spec.DesiredState != "stopped" {
			running++
		}
	}
	if n := running; n > 0 {
		writeProductError(w, http.StatusConflict, ErrorNodeAcceleratorHasInstances, "stop the instances on this node before switching accelerators", map[string]any{"count": n})
		return
	}
	raw, err := a.d.CallNode(r.Context(), uuid, proto.RPCNodeSetAccelerator, map[string]string{"accelerator": preference})
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	var result map[string]string
	if err := json.Unmarshal(raw, &result); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if err := a.d.Store.SetNodeAcceleratorPreference(uuid, preference); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	n, err := a.d.Store.NodeWithGPUs(uuid)
	if err != nil {
		writeNodeLookupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toNodeDTO(n))
}

func writeNodeLookupError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeErr(w, http.StatusInternalServerError, err)
}

// kubeManages says whether a vendor's cards run on Kubernetes nodes.
func kubeManages(vendor string) bool {
	_, ok := accelerator.KubeProfileFor(vendor)
	return ok
}

// acceleratorChoices are the vendors a node can be told to manage: every
// vendor on its host's bus, and on a Kubernetes node only those that run
// there.
func acceleratorChoices(n *store.Node) []proto.AcceleratorPresence {
	all := n.AcceleratorVendorList()
	if n.KubeClusterID == 0 {
		return all
	}
	out := make([]proto.AcceleratorPresence, 0, len(all))
	for _, v := range all {
		if kubeManages(v.Vendor) {
			out = append(out, v)
		}
	}
	return out
}
