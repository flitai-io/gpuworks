package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/rpccontext"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// ErrorLocalImageCatalogChanged reports a catalog conflict while the node was confirming an import.
const ErrorLocalImageCatalogChanged = "LOCAL_IMAGE_CATALOG_CHANGED"

type selectedNodeImage struct {
	NodeUUID                    string `json:"node_uuid" yaml:"node_uuid"`
	proto.RuntimeImageSelection `yaml:",inline"`
}

func (a *apiHandlers) runtimeImageCall(w http.ResponseWriter, r *http.Request, nodeUUID, method string, p proto.RuntimeImageSelection, out any) (*store.Node, bool) {
	node, err := a.d.Store.NodeByUUID(nodeUUID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return nil, false
	}
	if a.d.Online != nil && !a.d.Online(node.UUID) {
		writeProductError(w, http.StatusConflict, ErrorNodeOffline, "node is offline", nil)
		return nil, false
	}
	if a.d.CallNode == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorImageInspectionUnavailable, "node image inspection is unavailable", nil)
		return nil, false
	}
	timeout := time.Minute
	if method == proto.RPCImageRuntimeImport {
		timeout = proto.ImageOperationTimeout
	}
	ctx, cancel := rpccontext.WithTimeout(r.Context(), timeout)
	defer cancel()
	result, err := a.d.CallNode(ctx, node.UUID, method, p)
	if err != nil {
		code := proto.ErrorLocalImageUnavailable
		var rpc *proto.RPCError
		if errors.Is(err, proto.ErrRPCUnknownMethod) {
			code = messages.ErrorAgentUpgradeRequired
		} else if errors.As(err, &rpc) {
			switch rpc.Code {
			case proto.ErrorLocalImageBusy, proto.ErrorLocalImageChanged, proto.ErrorLocalImageConflict, proto.ErrorLocalImageUnsupported:
				code = rpc.Code
			}
		}
		writeProductError(w, http.StatusConflict, code, "the node could not complete the local image operation", nil)
		return nil, false
	}
	if err := json.Unmarshal(result, out); err != nil {
		writeProductError(w, http.StatusBadGateway, proto.ErrorLocalImageUnavailable, "invalid node image response", nil)
		return nil, false
	}
	return node, true
}

func (a *apiHandlers) listRuntimeImages(w http.ResponseWriter, r *http.Request) {
	var out proto.RuntimeImageList
	p := proto.RuntimeImageSelection{Namespace: r.URL.Query().Get("namespace")}
	if _, ok := a.runtimeImageCall(w, r, chi.URLParam(r, "uuid"), proto.RPCImageRuntimeList, p, &out); ok {
		writeJSON(w, http.StatusOK, out)
	}
}

func (a *apiHandlers) inspectRuntimeImage(w http.ResponseWriter, r *http.Request) {
	var p proto.RuntimeImageSelection
	if !decodeJSONBody(w, r, &p) {
		return
	}
	var out proto.RuntimeImageDetails
	if _, ok := a.runtimeImageCall(w, r, chi.URLParam(r, "uuid"), proto.RPCImageRuntimeInspect, p, &out); ok {
		out.Metadata = imagecatalog.CatalogImageMetadata(out.Metadata)
		writeJSON(w, http.StatusOK, out)
	}
}

func (a *apiHandlers) registerRuntimeImage(w http.ResponseWriter, r *http.Request, selection selectedNodeImage, inputs []imageInput) {
	if len(inputs) != 1 || inputs[0].Source != "custom" || inputs[0].ImageRef != selection.Ref || selection.NodeUUID == "" || selection.Identity == "" {
		writeProductError(w, http.StatusBadRequest, proto.ErrorLocalImageChanged, "select one tagged custom image on a node", nil)
		return
	}
	in := inputs[0]
	series, version, err := a.prepareImageInput(in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	expectedID, err := a.d.Store.CheckLocalImageRegistration(*series, *version)
	if err != nil {
		writeLocalRegistrationError(w, in.ImageRef, err)
		return
	}

	var installed proto.RuntimeImageDetails
	node, ok := a.runtimeImageCall(w, r, selection.NodeUUID, proto.RPCImageRuntimeImport, selection.RuntimeImageSelection, &installed)
	if !ok {
		return
	}
	if installed.ImageID == "" {
		writeProductError(w, http.StatusBadGateway, proto.ErrorLocalImageUnavailable, "the node did not confirm the installed image", nil)
		return
	}
	saved, id, err := a.d.Store.RegisterLocalImage(node.ID, expectedID, *series, *version, proto.ImagePresence{
		ImageID: installed.ImageID, RuntimeSizeBytes: installed.RuntimeSizeBytes, Present: true,
	})
	if err != nil {
		writeLocalRegistrationError(w, in.ImageRef, err)
		return
	}
	result := a.imageSeriesDTO(saved, true)
	if !slices.ContainsFunc(result.Versions, func(v dto.Image) bool { return v.ID == id }) {
		writeLocalRegistrationError(w, in.ImageRef, store.ErrLocalImageCatalogConflict)
		return
	}
	writeJSON(w, http.StatusOK, dto.NewList([]dto.ImageSeries{result}))
}

func writeLocalRegistrationError(w http.ResponseWriter, ref string, err error) {
	switch {
	case errors.Is(err, store.ErrLocalImageCatalogConflict):
		writeProductError(w, http.StatusConflict, ErrorLocalImageCatalogChanged, "the catalog entry changed or belongs to another source", map[string]any{"image": ref})
	case containsProductCode(err, ErrorImageFrameworkVersionRequired):
		writeErr(w, http.StatusBadRequest, err)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}
