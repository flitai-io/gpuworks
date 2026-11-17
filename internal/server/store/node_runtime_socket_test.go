package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// The socket facts of a report are kept for the operator only with their
// reason, and the next report replaces them: a fallback for another reason,
// or none, clears them; facts under another reason are not stored at all.
func TestTheSocketFactsLastAsLongAsTheirReason(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-socket", HashToken("t"), "socket-host"); err != nil {
		t.Fatal(err)
	}
	report := func(sys proto.SysInfo) Node {
		t.Helper()
		if err := s.UpdateNodeStatus("uuid-socket", proto.NodeStatus{Sys: sys}); err != nil {
			t.Fatal(err)
		}
		node, err := s.NodeByUUID("uuid-socket")
		if err != nil {
			t.Fatal(err)
		}
		return *node
	}
	denied := proto.RuntimeSocketDenial{Socket: "/var/run/docker.sock", Group: "docker", GID: 999, GroupHeld: true, Account: "gpusvc",
		SameNameGID: 1002, RestartPending: true, Supervisor: proto.AgentSupervisorNohup, StateDir: "/home/gpusvc/gpuworks"}
	node := report(proto.SysInfo{RuntimeDriver: "udocker", RuntimeFallbackFrom: "docker",
		RuntimeFallbackReason: proto.RuntimeFallbackSocketPermissionDenied, RuntimeFallbackSocket: &denied})
	if got := node.RuntimeSocketDenial(); got == nil || *got != denied {
		t.Fatalf("stored socket = %+v, want %+v", got, denied)
	}
	// An unread gid stays unread, not root's gid 0.
	unread := proto.RuntimeSocketDenial{Socket: "/var/run/docker.sock", GID: proto.GIDUnknown, Account: "gpusvc"}
	node = report(proto.SysInfo{RuntimeDriver: "udocker", RuntimeFallbackFrom: "docker",
		RuntimeFallbackReason: proto.RuntimeFallbackSocketPermissionDenied, RuntimeFallbackSocket: &unread})
	if got := node.RuntimeSocketDenial(); got == nil || *got != unread {
		t.Fatalf("stored socket = %+v, want %+v", got, unread)
	}

	node = report(proto.SysInfo{RuntimeDriver: "udocker", RuntimeFallbackFrom: "docker",
		RuntimeFallbackReason: proto.RuntimeFallbackDaemonNotReady, RuntimeFallbackSocket: &denied})
	if got := node.RuntimeSocketDenial(); got != nil || node.RuntimeFallbackSocket != "" || node.RuntimeFallbackSocketGID != 0 || node.RuntimeFallbackSocketRestartPending {
		t.Fatalf("socket facts under daemon_not_ready: %+v / row %q gid=%d", got, node.RuntimeFallbackSocket, node.RuntimeFallbackSocketGID)
	}

	report(proto.SysInfo{RuntimeDriver: "udocker", RuntimeFallbackFrom: "docker",
		RuntimeFallbackReason: proto.RuntimeFallbackSocketPermissionDenied, RuntimeFallbackSocket: &denied})
	node = report(proto.SysInfo{RuntimeDriver: "docker"})
	if node.RuntimeSocketDenial() != nil || node.RuntimeFallbackSocketAccount != "" || node.RuntimeFallbackSocketStateDir != "" {
		t.Fatalf("socket facts outlived the fallback: %+v", node)
	}
}
