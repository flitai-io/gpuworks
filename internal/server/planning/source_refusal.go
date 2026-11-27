package planning

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// SourceRefusal is why a model cannot be read on a node now, as
// store.ModelSourceOn decided it, or nil while it is readable or on its way.
// It is the one place such a reason is built: admission, the pool's trial
// and the controller report the same reason whichever of them noticed.
//
// A refused source names the storage and why. An acquisition or cache fill
// that failed (or waits for an operator to pick a source) is a stuck model on
// the node; one still running is not a refusal. node may be nil for a preview
// that names no node.
func SourceRefusal(src store.ModelSource, node *store.Node, model *store.Model) *scheduler.Reason {
	storage := src.Location.Storage
	switch src.Action {
	case store.SourceRefused:
		code, why := scheduler.ReasonStorageUnavailable, src.Reason
		switch src.Reason {
		case store.LocationStorageDisabled:
			code = scheduler.ReasonStorageDisabled
		case store.LocationModelMissingInStore:
			code = scheduler.ReasonModelMissingInStorage
			if shared := src.Location.Shared; shared != nil && shared.Message != "" {
				why = shared.Message
			}
		}
		params := map[string]any{"storage": storage.Name, "storage_id": storage.ID, "reason": why}
		if node != nil {
			params["node"] = node.UUID
		}
		return &scheduler.Reason{Code: code, Params: params}
	case store.SourceAcquire, store.SourceFillCache:
		mf := src.Location.File
		// A download withdrawn with its service starts again for the
		// deployment that needs it (the controller's acquireLocalModel).
		if mf == nil || mf.WithdrawnWithService() {
			return nil
		}
		switch mf.State {
		case proto.MFCanceled, proto.MFFailed, proto.MFDownloadRequired, proto.MFInsufficientDisk, proto.MFDeleting:
		default:
			return nil
		}
		params := map[string]any{"state": mf.State, "storage": storage.Name}
		if node != nil {
			params["node"] = node.DisplayName()
		}
		if model != nil {
			params["model"] = model.Name
		}
		if mf.Message != "" {
			params["detail"] = mf.Message
		}
		return &scheduler.Reason{Code: scheduler.ReasonModelUnavailable, Params: params}
	}
	return nil
}

// SourceBlocksAdmission reports whether a refused source rules the node out
// when a launch is planned. A shared storage that lacks the model is not: a
// create that downloads into it is planned before the download starts. A
// read-only storage is not either: admission does not know an acquisition is
// coming. Both are reported once the controller tries.
func SourceBlocksAdmission(src store.ModelSource) bool {
	if src.Action != store.SourceRefused {
		return false
	}
	switch src.Reason {
	case store.LocationModelMissingInStore, store.LocationStorageDisabled:
		return false
	}
	return true
}
