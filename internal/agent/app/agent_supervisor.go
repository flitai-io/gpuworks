package app

import (
	"os"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

// selfCgroupFile is where the kernel says which cgroup this process is in;
// a variable so a test can stand in for the host.
var selfCgroupFile = "/proc/self/cgroup"

// agentSupervisor is what restarts this agent (proto.AgentSupervisor*): the
// install script's systemd unit, gpuworks-agent.service, puts the agent in
// a cgroup of that name; a nohup install leaves agent-restart.sh in the
// state directory (and a systemd install removes it). "" when neither
// holds, as for an agent someone started by hand.
func agentSupervisor(stateDir string) string {
	if raw, err := os.ReadFile(selfCgroupFile); err == nil {
		for line := range strings.SplitSeq(string(raw), "\n") {
			if strings.HasSuffix(strings.TrimSpace(line), "/gpuworks-agent.service") {
				return proto.AgentSupervisorSystemd
			}
		}
	}
	if stateDir != "" {
		if info, err := os.Stat(filepath.Join(stateDir, "agent-restart.sh")); err == nil && info.Mode().IsRegular() {
			return proto.AgentSupervisorNohup
		}
	}
	return ""
}
