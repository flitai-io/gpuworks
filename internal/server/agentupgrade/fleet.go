package agentupgrade

import (
	"context"
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func (s *Service) candidate(n store.Node) Candidate {
	c := Candidate{NodeUUID: n.UUID, Name: n.Name, FromVersion: n.AgentVersion, OS: n.OS, Arch: n.Arch, Eligibility: "eligible"}
	if c.Name == "" {
		c.Name = n.Hostname
	}
	if c.Name == "" {
		c.Name = n.UUID
	}
	conn, online := s.cfg.Transport.AgentUpgradeConnection(n.UUID)
	switch {
	case !online:
		c.Eligibility, c.Reason = "offline", "node is offline"
	case n.AgentVersion == s.cfg.TargetVersion:
		c.Eligibility, c.Reason = "already_current", "agent already matches the target"
	case strings.TrimSpace(n.AgentVersion) == "" || n.AgentVersion == "dev":
		c.Eligibility, c.Reason = "unknown_version", "the agent version cannot be identified"
	case n.KubeClusterID != 0 && !slices.Contains(conn.Caps, proto.CapKubeBinaryUpgrade):
		// An agent of a DaemonSet that could not hand over to the
		// node's state directory (kubeboot; a noexec mount, say) runs
		// its image's own binary, and a binary pushed into the pod is
		// gone at its next restart.
		c.Eligibility, c.Reason = "kubernetes", "the agent could not start from the node's state directory and runs its image's own binary, which an upgrade cannot replace"
	case n.Arch == "" || (n.OS != "" && n.OS != "linux" && n.OS != "darwin"):
		c.Eligibility, c.Reason = "unsupported", "agent platform is unsupported or unknown"
	default:
		if slices.Contains(conn.Caps, proto.CapUpgradePush) {
			c.Transport = "push"
		} else if n.ConnectMode == "server_dial" {
			c.Eligibility, c.Reason = "legacy_unverified", "this older server-dial agent cannot prove a download address"
		} else {
			c.Transport = "pull"
		}
	}

	return c
}

type Fleet struct {
	Target      string
	RunID       uint
	Eligibility map[string]string
}

func (s *Service) Fleet(ctx context.Context, nodes []store.Node) (Fleet, error) {
	f := Fleet{Target: s.cfg.TargetVersion, Eligibility: make(map[string]string, len(nodes))}
	pending, err := s.st.AgentUpgradePendingItems()
	if err != nil {
		return f, err
	}
	for _, item := range pending {
		f.RunID = item.RunID
		f.Eligibility[item.NodeUUID] = "busy"
	}
	if strings.TrimSpace(f.Target) == "" || f.Target == "dev" {
		return f, nil
	}
	available := map[string]bool{}
	for _, n := range nodes {
		if err := ctx.Err(); err != nil {
			return f, err
		}
		if f.Eligibility[n.UUID] == "busy" {
			continue
		}
		c := s.candidate(n)
		if c.Eligibility == "eligible" {
			name := proto.AgentBinaryName(n.OS, n.Arch)
			ok, checked := available[name]
			if !checked {
				ok = s.cfg.BinaryAvailable != nil && s.cfg.BinaryAvailable(name)
				available[name] = ok
			}
			if !ok {
				c.Eligibility = "artifact_missing"
			}
		}
		f.Eligibility[n.UUID] = c.Eligibility
	}
	return f, nil
}
