package api

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

// nodeUCXNetlinkHangDTO is a node's UCX hang notice with the fixes that end
// it (the same list the deploy refusal carries), or nil for a node that does
// not hang UCX.
func nodeUCXNetlinkHangDTO(reported *proto.UCXNetlinkHang) *dto.NodeUCXNetlinkHang {
	if reported == nil {
		return nil
	}
	out := &dto.NodeUCXNetlinkHang{Kernel: reported.Kernel, Fixes: []dto.NodeUCXNetlinkHangFix{}}
	for _, fix := range pdmode.UCXNetlinkFixes() {
		out.Fixes = append(out.Fixes, dto.NodeUCXNetlinkHangFix{Code: fix.Code, Commands: fix.Commands})
	}
	return out
}

// fillNodeUCXNetlinkHang sets the UCX hang notice each online node last
// reported.
func (a *apiHandlers) fillNodeUCXNetlinkHang(out []dto.Node) {
	if a.d.NodeRuntime == nil {
		return
	}
	for i := range out {
		out[i].UCXNetlinkHang = nodeUCXNetlinkHangDTO(a.d.NodeRuntime.UCXNetlinkHang(out[i].UUID))
	}
}

// ucxNetlinkHangOf is the live hang fact the deployment use cases read, or nil
// where the server has no hub to ask.
func (a *apiHandlers) ucxNetlinkHangOf() func(uuid string) *proto.UCXNetlinkHang {
	if a.d.NodeRuntime == nil {
		return nil
	}
	return a.d.NodeRuntime.UCXNetlinkHang
}
