package hub

import (
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// duplicateAgentCode is the stable code logged when two agent processes
// trade one node's connection.
const duplicateAgentCode = messages.ErrorAgentDuplicateInstance

const (
	// duplicateAgentWindow: an evicted process that takes the node back
	// within it is a second agent, not a restart.
	duplicateAgentWindow = 10 * time.Minute
	// duplicateAgentReportEvery rate-limits the report per node.
	duplicateAgentReportEvery = time.Hour
	// maxInstanceIDLen bounds what a hello may make the hub remember; a
	// longer id is treated as none.
	maxInstanceIDLen = 64
)

// duplicateAgents notices two agent processes sharing one node's identity
// (a token copied with a cloned disk, a second state directory holding the
// same token, an agent from before the state-directory lock): each one's
// registration replaces the other's session, so the node flaps and streams
// through it are cut. The agent-side lock cannot see a second host; the hub
// sees both. Per node it remembers only the last process a registration
// evicted, O(1) per handshake, in memory, nothing stored. A trace lives
// until neither the window nor the report period covers it, so the map holds
// only nodes replaced in the last hour; a deleted node's trace expires the
// same way.
//
// Agents that send no instance id (older than the field) are never
// remembered as the evicted process, so they cannot be reported against
// themselves; one that sends an id is still caught trading with one that
// does not.
type duplicateAgents struct {
	mu    sync.Mutex
	nodes map[string]*duplicateTrace
	swept time.Time
}

// duplicateTrace is the last eviction on a node: which process lost the
// session, and the host and address of the agent that took it.
type duplicateTrace struct {
	evicted        string
	byHost, byAddr string
	at             time.Time
	reported       time.Time
}

// duplicateReport names the other process of a reported trade.
type duplicateReport struct {
	otherHost, otherRemote string
}

// observe records that s registered at now, replacing the live session old
// (nil when there was none). It reports whether s is a process the node
// evicted within the window taking the node back, the first time per
// reporting period, naming the process it took the node from.
func (d *duplicateAgents) observe(s, old *Session, now time.Time) (duplicateReport, bool) {
	oldInstance := ""
	if old != nil {
		oldInstance = old.instanceID
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweep(now)
	if s.instanceID == "" && oldInstance == "" {
		return duplicateReport{}, false
	}
	trace := d.nodes[s.NodeUUID]
	var report duplicateReport
	flagged := false
	if trace != nil && s.instanceID != "" && trace.evicted == s.instanceID && now.Sub(trace.at) <= duplicateAgentWindow &&
		(trace.reported.IsZero() || now.Sub(trace.reported) >= duplicateAgentReportEvery) {
		trace.reported = now
		flagged = true
		report = duplicateReport{otherHost: trace.byHost, otherRemote: trace.byAddr}
		if old != nil {
			report = duplicateReport{otherHost: old.hostname, otherRemote: old.remote}
		}
	}
	if oldInstance != "" && oldInstance != s.instanceID {
		if trace == nil {
			if d.nodes == nil {
				d.nodes = map[string]*duplicateTrace{}
			}
			trace = &duplicateTrace{}
			d.nodes[s.NodeUUID] = trace
		}
		trace.evicted, trace.byHost, trace.byAddr, trace.at = oldInstance, s.hostname, s.remote, now
	}
	return report, flagged
}

// sweep drops traces that can no longer flag or rate-limit anything, at
// most once per window, so the cost is amortized over the handshakes.
func (d *duplicateAgents) sweep(now time.Time) {
	if now.Sub(d.swept) < duplicateAgentWindow {
		return
	}
	d.swept = now
	for node, trace := range d.nodes {
		if d.expired(trace, now) {
			delete(d.nodes, node)
		}
	}
}

func (d *duplicateAgents) expired(trace *duplicateTrace, now time.Time) bool {
	return now.Sub(trace.at) > duplicateAgentWindow &&
		(trace.reported.IsZero() || now.Sub(trace.reported) >= duplicateAgentReportEvery)
}

// tracked is how many nodes have a trace (tests).
func (d *duplicateAgents) tracked() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.nodes)
}

// logDuplicateAgents logs, coded, that s took node back from the process it
// had evicted (report names that other process).
func (h *Hub) logDuplicateAgents(s *Session, report duplicateReport) {
	h.log.Warn("two agent processes share this node's identity and keep replacing each other's connection; stop the extra gpuworks-agent, or give a cloned host its own node token",
		"code", duplicateAgentCode, "node", s.NodeUUID,
		"host", s.hostname, "remote", s.remote,
		"other_host", report.otherHost, "other_remote", report.otherRemote)
}
