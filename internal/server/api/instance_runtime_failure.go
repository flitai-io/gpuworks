package api

import (
	"encoding/json"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func instanceRuntimeFailure(in *store.Instance) *dto.ProductMessage {
	if in.RuntimeFailure == "" || in.Message == "" {
		return nil
	}
	var failure proto.RuntimeFailure
	if json.Unmarshal([]byte(in.RuntimeFailure), &failure) != nil || failure.Code == "" {
		return nil
	}
	brand := accelerator.VendorBrand(failure.Vendor)
	if brand == "" {
		brand = failure.Vendor
	}
	return &dto.ProductMessage{Code: failure.Code, Message: in.Message, Params: map[string]any{
		"vendor": brand, "os": failure.OS, "arch": failure.Arch, "runtime": failure.Runtime,
		"required_os": failure.RequiredOS, "required_arch": failure.RequiredArch, "required_runtime": failure.RequiredRuntime, "resource": failure.Resource,
	}}
}
