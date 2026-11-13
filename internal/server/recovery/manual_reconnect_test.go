package recovery

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type reconnectingHub struct {
	*fakeHub
	reconnect func(context.Context, string) error
}

func (h *reconnectingHub) ReconnectServerDial(ctx context.Context, uuid string) error {
	return h.reconnect(ctx, uuid)
}

func TestManualRecoveryReconnectsRunningServerDialAgent(t *testing.T) {
	for _, reachable := range []bool{true, false} {
		name := "reachable"
		if !reachable {
			name = "still_unreachable"
		}
		t.Run(name, func(t *testing.T) {
			r, st, hub, provisioner := newRecoveryHarness(t)
			node := newOfflineNode(t, st, true)
			if err := st.DB().Model(node).Update("connect_mode", agentconf.ServerDial).Error; err != nil {
				t.Fatal(err)
			}
			provisioner.probeResult = provision.ProbeResult{Reachable: true, AgentInstalled: true, AgentRunning: true}
			calls := 0
			r.hub = &reconnectingHub{fakeHub: hub, reconnect: func(_ context.Context, uuid string) error {
				calls++
				if uuid != node.UUID {
					t.Fatalf("reconnecting %q, want %q", uuid, node.UUID)
				}
				if !reachable {
					return errors.New("connection refused")
				}
				hub.online[uuid] = true
				if err := st.SetNodeState(uuid, store.NodeStateOnline); err != nil {
					return err
				}
				return st.ClearNodeRecovery(uuid)
			}}
			outcome, err := r.Trigger(context.Background(), node.UUID)
			if err != nil || calls != 1 || outcome.Recovered != reachable {
				t.Fatalf("outcome=%+v calls=%d err=%v", outcome, calls, err)
			}
			if provisioner.restarts != 0 || provisioner.installs != 0 {
				t.Fatalf("a running agent was changed: restart=%d install=%d", provisioner.restarts, provisioner.installs)
			}
			updated, err := st.NodeByUUID(node.UUID)
			if err != nil {
				t.Fatal(err)
			}
			if reachable {
				if updated.ConnectionState() != store.NodeStateOnline || updated.RecoveryPhase != "" || outcome.Error != "" {
					t.Fatalf("successful reconnect not reflected: node=%+v outcome=%+v", updated, outcome)
				}
			} else if updated.ConnectionState() != store.NodeStateOffline || !strings.Contains(outcome.Error, "connection refused") {
				t.Fatalf("failed reconnect must be offline with its cause: node=%+v outcome=%+v", updated, outcome)
			}
		})
	}
}

func TestRunningAgentProbeEndsOfflineAndRemainsRetryable(t *testing.T) {
	r, st, _, provisioner := newRecoveryHarness(t)
	node := newOfflineNode(t, st, true)
	provisioner.probeResult = provision.ProbeResult{Reachable: true, AgentInstalled: true, AgentRunning: true}
	outcome, err := r.recoverNode(context.Background(), node, false)
	if err != nil || outcome.Recovered || outcome.Error == "" {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	updated, err := st.NodeByUUID(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ConnectionState() != store.NodeStateOffline {
		t.Fatalf("completed probe still reads as %q", updated.ConnectionState())
	}
	due, err := st.NodesNeedingRecovery()
	if err != nil || len(due) != 1 || due[0].UUID != node.UUID {
		t.Fatalf("first failed probe should remain retryable: due=%+v err=%v", due, err)
	}
	outcome, err = r.recoverNode(context.Background(), updated, true)
	if err != nil || outcome.Phase != store.RecoveryPhaseBlocked {
		t.Fatalf("repeated failed probe must retain the existing stop policy: %+v err=%v", outcome, err)
	}
}

func TestLegacyFailedProbeKeepsRepeatedFailurePolicy(t *testing.T) {
	r, st, _, provisioner := newRecoveryHarness(t)
	node := newOfflineNode(t, st, true)
	if err := st.SetNodeRecoveryPhase(node.UUID, store.RecoveryPhaseProbing, 1, timePtr(r.now()), "agent is running but has not connected"); err != nil {
		t.Fatal(err)
	}
	provisioner.probeResult = provision.ProbeResult{Reachable: true, AgentInstalled: true, AgentRunning: true}
	outcome, err := r.Trigger(context.Background(), node.UUID)
	if err != nil || outcome.Phase != store.RecoveryPhaseBlocked {
		t.Fatalf("legacy failed probe lost its retry count: %+v err=%v", outcome, err)
	}
	updated, err := st.NodeByUUID(node.UUID)
	if err != nil || updated.ConnectionState() != store.NodeStateOffline || updated.RecoveryAttempts != 2 {
		t.Fatalf("legacy result not persisted as ended: %+v err=%v", updated, err)
	}
}

func TestRunningAgentRedialOnlyForManualServerDialRecovery(t *testing.T) {
	for _, mode := range []string{agentconf.AgentDial, agentconf.ServerDial} {
		for _, force := range []bool{false, true} {
			if mode == agentconf.ServerDial && force {
				continue
			}
			name := mode + "_automatic"
			if force {
				name = mode + "_manual"
			}
			t.Run(name, func(t *testing.T) {
				r, st, hub, provisioner := newRecoveryHarness(t)
				node := newOfflineNode(t, st, true)
				node.ConnectMode = mode
				provisioner.probeResult = provision.ProbeResult{Reachable: true, AgentInstalled: true, AgentRunning: true}
				r.hub = &reconnectingHub{fakeHub: hub, reconnect: func(context.Context, string) error {
					t.Error("unexpected redial")
					return errors.New("unexpected redial")
				}}
				outcome, err := r.recoverNode(context.Background(), node, force)
				if err != nil || outcome.Recovered || outcome.Phase != store.RecoveryPhaseProbeFailed || provisioner.restarts != 0 || provisioner.installs != 0 {
					t.Fatalf("outcome=%+v restart=%d install=%d err=%v", outcome, provisioner.restarts, provisioner.installs, err)
				}
			})
		}
	}
}
