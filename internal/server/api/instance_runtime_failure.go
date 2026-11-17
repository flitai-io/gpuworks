package api

import (
	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// instanceRuntimeFailure is the instance's structured failure reason as a
// product message the client renders in its locale; nil for a row without
// one (an older agent's report or an older row), whose Message is all there
// is.
func instanceRuntimeFailure(in *store.Instance) *dto.ProductMessage {
	failure := in.Failure()
	if failure == nil {
		return nil
	}
	return runtimeFailureMessage(*failure, in.Message)
}

// runtimeFailureMessage maps a runtime failure to a product message: the
// code, its typed fields as params, message as the English fallback (the
// code itself when there is none). The params map exists only here, at the
// API boundary (ADR 0032).
func runtimeFailureMessage(failure proto.RuntimeFailure, message string) *dto.ProductMessage {
	if message == "" {
		message = failure.Code
	}
	brand := accelerator.VendorBrand(failure.Vendor)
	if brand == "" {
		brand = failure.Vendor
	}
	params := map[string]any{
		"vendor": brand, "os": failure.OS, "arch": failure.Arch, "runtime": failure.Runtime,
		"required_os": failure.RequiredOS, "required_arch": failure.RequiredArch, "required_runtime": failure.RequiredRuntime,
		"resource": failure.Resource, "feature": failure.Feature, "phase": failure.Phase,
		"detail": failure.Detail, "node": failure.Node,
	}
	if failure.ExitCode != nil {
		params["exit_code"] = *failure.ExitCode
	}
	if failure.OOMKilled {
		params["oom_killed"] = true
	}
	if failure.Port != 0 {
		params["port"] = failure.Port
	}
	if failure.Rank != nil {
		params["rank"] = *failure.Rank
	}
	if len(failure.Evidence) > 0 {
		params["evidence"] = failure.Evidence
	}
	if len(failure.Causes) > 0 {
		causes := make([]dto.ProductMessage, len(failure.Causes))
		for i, c := range failure.Causes {
			causes[i] = failureCauseMessage(c)
		}
		params["causes"] = causes
	}
	return &dto.ProductMessage{Code: failure.Code, Params: params, Message: message}
}

// failureCauseMessage maps one known engine failure the output showed.
func failureCauseMessage(c proto.FailureCause) dto.ProductMessage {
	params := map[string]any{}
	if c.GLIBC != "" {
		params["glibc"] = c.GLIBC
	}
	if c.Device != nil {
		params["device"] = *c.Device
		params["needed_bytes"] = c.NeededBytes
		params["limit_bytes"] = c.LimitBytes
	}
	if len(c.Evidence) > 0 {
		params["evidence"] = c.Evidence
	}
	return dto.ProductMessage{Code: c.Code, Params: params, Message: aihelper.CauseFallback(c)}
}

// eventRuntimeFailure replaces the structured reason a failed transition's
// event stores with the product message the client renders; the event's
// detail param stays its English fallback.
func eventRuntimeFailure(params map[string]any) {
	if _, ok := params["failure"]; !ok {
		return
	}
	failure := store.EventFailure(params)
	if failure == nil {
		delete(params, "failure")
		return
	}
	detail, _ := params["detail"].(string)
	params["failure"] = runtimeFailureMessage(*failure, detail)
}
