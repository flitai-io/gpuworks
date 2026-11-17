package store

import "gpuworks.ai/gpuworks/internal/proto"

// runtimeFallbackSocketUpdates writes the socket facts of a status report
// (Node.RuntimeFallbackSocket*). Every report replaces them: a report
// without them — another reason, no fallback, an older agent — clears them,
// so they never outlive the reason they explain.
func runtimeFallbackSocketUpdates(updates map[string]any, sys proto.SysInfo) {
	var d proto.RuntimeSocketDenial
	if sys.RuntimeFallbackReason == proto.RuntimeFallbackSocketPermissionDenied && sys.RuntimeFallbackSocket != nil {
		d = *sys.RuntimeFallbackSocket
	}
	updates["runtime_fallback_socket"] = truncate(d.Socket, 512)
	updates["runtime_fallback_socket_group"] = truncate(d.Group, 64)
	updates["runtime_fallback_socket_gid"] = d.GID
	updates["runtime_fallback_socket_group_held"] = d.GroupHeld
	updates["runtime_fallback_socket_account"] = truncate(d.Account, 64)
	updates["runtime_fallback_socket_same_name_gid"] = d.SameNameGID
	updates["runtime_fallback_socket_restart_pending"] = d.RestartPending
	updates["runtime_fallback_socket_supervisor"] = truncate(d.Supervisor, 16)
	updates["runtime_fallback_socket_state_dir"] = truncate(d.StateDir, 512)
}

// RuntimeSocketDenial is the socket facts the node's agent last reported,
// nil unless the node fell back for proto.RuntimeFallbackSocketPermissionDenied.
func (n *Node) RuntimeSocketDenial() *proto.RuntimeSocketDenial {
	if n == nil || n.RuntimeFallbackReason != proto.RuntimeFallbackSocketPermissionDenied || n.RuntimeFallbackSocket == "" {
		return nil
	}
	return &proto.RuntimeSocketDenial{
		Socket:         n.RuntimeFallbackSocket,
		Group:          n.RuntimeFallbackSocketGroup,
		GID:            n.RuntimeFallbackSocketGID,
		GroupHeld:      n.RuntimeFallbackSocketGroupHeld,
		Account:        n.RuntimeFallbackSocketAccount,
		SameNameGID:    n.RuntimeFallbackSocketSameNameGID,
		RestartPending: n.RuntimeFallbackSocketRestartPending,
		Supervisor:     n.RuntimeFallbackSocketSupervisor,
		StateDir:       n.RuntimeFallbackSocketStateDir,
	}
}
