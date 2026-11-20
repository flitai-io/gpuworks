package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Test kinds; they match nettest's modes.
const (
	networkTestTCP      = "tcp"
	networkTestRDMATool = "rdma_tool"
)

// maxNetworkTestNodes bounds one mesh run: N nodes probe N*(N-1) ordered
// pairs in N-1 rounds, so a larger fleet is tested as a ring, by label or by
// explicit nodes. A ring is N pairs in one round.
const (
	maxNetworkTestNodes     = 128
	maxNetworkRingTestNodes = 1024
)

// networkTestable reports whether a network carries a role internal tests
// cover; serving-only networks are reached from the server, not node to node.
func networkTestable(n store.Network) bool {
	return network.Network(n).Testable()
}

// networkTestPlan is a test request checked against the network: what would
// be recorded, and for a card test how large it is.
type networkTestPlan struct {
	state      networkState
	networkID  uint
	kind       string
	pattern    string
	scope      string
	labelKey   string
	labelValue string
	uuids      []string
	bandwidth  bool
	seconds    int
	card       *network.CardTestPlan
}

// planNetworkTest validates a test request; starting a test and estimating
// one both go through it, so an estimate that passes is a request that
// starts. It writes the error response itself when it returns false.
func (a *apiHandlers) planNetworkTest(w http.ResponseWriter, r *http.Request) (networkTestPlan, bool) {
	id := parseUint(chi.URLParam(r, "id"))
	var req dto.NetworkTestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return networkTestPlan{}, false
	}
	st, err := a.loadNetworkState()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return networkTestPlan{}, false
	}
	var tested *netplan.Network
	if isManagementNetworkID(chi.URLParam(r, "id")) {
		mgmt := netplan.Management()
		tested = &mgmt
	}
	for _, n := range st.Networks {
		if n.ID == id {
			converted := network.Network(n)
			tested = &converted
		}
	}
	if tested == nil {
		writeProductError(w, http.StatusNotFound, ErrorNetworkNotFound, "network not found", nil)
		return networkTestPlan{}, false
	}
	if !tested.Testable() {
		writeProductError(w, http.StatusBadRequest, ErrorNetworkTestUnsupported, "only storage-sync and compute networks can be tested", nil)
		return networkTestPlan{}, false
	}
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = networkTestTCP
	}
	switch {
	case kind != networkTestTCP && kind != networkTestRDMATool && !network.CardTest(kind):
		writeProductError(w, http.StatusBadRequest, ErrorNetworkTestKindInvalid, "test kind must be tcp, rdma_tool, card_ping or card_matrix", map[string]any{"kind": kind})
		return networkTestPlan{}, false
	case kind == networkTestRDMATool && tested.Type != netplan.TypeHostRDMA:
		writeProductError(w, http.StatusBadRequest, ErrorNetworkTestKindInvalid, "rdma_tool tests need an RDMA network", map[string]any{"kind": kind})
		return networkTestPlan{}, false
	case network.CardTest(kind) && !netplan.IsAccelerator(tested.Type):
		writeProductError(w, http.StatusBadRequest, ErrorNetworkTestKindInvalid, "card tests need an accelerator network", map[string]any{"kind": kind})
		return networkTestPlan{}, false
	}
	pattern := strings.TrimSpace(req.Pattern)
	if pattern == "" {
		pattern = network.TestPatternMesh
	}
	if pattern != network.TestPatternMesh && pattern != network.TestPatternRing {
		writeProductError(w, http.StatusBadRequest, ErrorNetworkTestPatternInvalid, "test pattern must be mesh or ring", map[string]any{"pattern": pattern})
		return networkTestPlan{}, false
	}
	uuids, labelKey, labelValue, ok := a.networkTestNodes(w, st, tested.ID, req)
	if !ok {
		return networkTestPlan{}, false
	}
	maximum := maxNetworkTestNodes
	if pattern == network.TestPatternRing {
		maximum = maxNetworkRingTestNodes
	}
	if len(uuids) < 2 || len(uuids) > maximum {
		writeProductError(w, http.StatusBadRequest, ErrorNetworkTestNodeCount, fmt.Sprintf("a %s network test needs 2 to %d nodes on the network", pattern, maximum),
			map[string]any{"count": len(uuids), "minimum": 2, "maximum": maximum})
		return networkTestPlan{}, false
	}
	plan := networkTestPlan{state: st, networkID: tested.ID, kind: kind, pattern: pattern, scope: req.Scope, labelKey: labelKey, labelValue: labelValue, uuids: uuids}
	if network.CardTest(kind) {
		card, err := network.PlanCardTest(kind, pattern, st.networkRails(tested.ID, tested.Type, uuids))
		var tooLong *network.CardTestTooLongError
		if errors.As(err, &tooLong) {
			writeProductError(w, http.StatusBadRequest, ErrorNetworkTestTooLong, err.Error(),
				map[string]any{"rounds": card.Rounds, "seconds": card.Seconds, "maximum": card.MaxSeconds})
			return networkTestPlan{}, false
		}
		plan.card = &card
		return plan, true
	}
	plan.bandwidth = req.Bandwidth == nil || *req.Bandwidth || kind == networkTestRDMATool
	plan.seconds = req.Seconds
	if plan.seconds == 0 {
		plan.seconds = 3
	}
	if plan.seconds < 1 || plan.seconds > 10 {
		writeProductError(w, http.StatusBadRequest, ErrorPreflightDurationInvalid, "bandwidth duration must be between 1 and 10 seconds", map[string]any{"minimum": 1, "maximum": 10})
		return networkTestPlan{}, false
	}
	return plan, true
}

// estimateNetworkTest says how large a test request is without starting it:
// the nodes it resolves to, and for a card test its rounds and the longest it
// can take. A request that could not start is refused the same way.
func (a *apiHandlers) estimateNetworkTest(w http.ResponseWriter, r *http.Request) {
	plan, ok := a.planNetworkTest(w, r)
	if !ok {
		return
	}
	out := dto.NetworkTestEstimate{Kind: plan.kind, Pattern: plan.pattern, NodeCount: len(plan.uuids), PairCount: network.TestPairCount(plan.kind, plan.pattern, len(plan.uuids))}
	if plan.card != nil {
		out.PairCount = plan.card.Pairs
		out.Card = &dto.NetworkCardTestEstimate{Rounds: plan.card.Rounds, Probes: plan.card.Probes, Seconds: plan.card.Seconds, MaxSeconds: plan.card.MaxSeconds}
	}
	writeJSON(w, http.StatusOK, out)
}

// startNetworkTest records a test request; the controller runs it.
func (a *apiHandlers) startNetworkTest(w http.ResponseWriter, r *http.Request) {
	plan, ok := a.planNetworkTest(w, r)
	if !ok {
		return
	}
	nodesJSON, _ := json.Marshal(plan.uuids)
	run, err := a.d.Store.StartNetworkTestRun(&store.NetworkTestRun{
		NetworkID: plan.networkID, Kind: plan.kind, Pattern: plan.pattern, Scope: plan.scope, LabelKey: plan.labelKey, LabelValue: plan.labelValue, Bandwidth: plan.bandwidth, Seconds: plan.seconds,
		Nodes: string(nodesJSON), NodeCount: len(plan.uuids),
	})
	switch {
	case errors.Is(err, store.ErrNetworkTestRunning):
		writeProductError(w, http.StatusConflict, ErrorNetworkTestRunning, "a test of this network is already running", nil)
		return
	case err != nil:
		writeNetworkErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, networkTestDTO(run, plan.state.nodeByID()))
}

// networkRails lists, node by node in the given order, the rails each node
// uses on a network.
func (st networkState) networkRails(networkID uint, networkType string, uuids []string) [][]netplan.Rail {
	byUUID := map[string]store.Node{}
	for _, node := range st.Nodes {
		byUUID[node.UUID] = node
	}
	hcas := map[uint][]string{}
	for _, r := range st.Resolved {
		if r.Network.ID == networkID {
			hcas[r.NodeID] = r.HCAs
		}
	}
	out := make([][]netplan.Rail, len(uuids))
	for i, uuid := range uuids {
		node := byUUID[uuid]
		out[i] = netplan.Rails(network.Inventory(node), networkType, hcas[node.ID])
	}
	return out
}

// networkTestNodes picks the nodes a request covers among those the network
// resolved to for a testable role, and the label a label-scoped request
// selects them by. It writes the error response itself.
func (a *apiHandlers) networkTestNodes(w http.ResponseWriter, st networkState, networkID uint, req dto.NetworkTestRequest) (uuids []string, labelKey, labelValue string, ok bool) {
	nodes := st.nodeByID()
	endpoints, err := a.networks().Endpoints(networkID, st.Nodes)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return nil, "", "", false
	}
	onNetwork := map[string]store.Node{}
	for _, r := range endpoints {
		if netplan.TestedRole(r.Role) {
			node := nodes[r.NodeID]
			onNetwork[node.UUID] = node
		}
	}
	switch req.Scope {
	case "all":
		for uuid := range onNetwork {
			uuids = append(uuids, uuid)
		}
	case "nodes":
		seen := map[string]bool{}
		for _, uuid := range req.NodeUUIDs {
			uuid = strings.TrimSpace(uuid)
			if seen[uuid] {
				continue
			}
			seen[uuid] = true
			if _, ok := onNetwork[uuid]; !ok {
				writeProductError(w, http.StatusBadRequest, ErrorNetworkTestNodeInvalid, "node is not on this network", map[string]any{"node": uuid})
				return nil, "", "", false
			}
			uuids = append(uuids, uuid)
		}
	case "label":
		key, value := strings.TrimSpace(req.LabelKey), strings.TrimSpace(req.LabelValue)
		if key == "" {
			writeProductError(w, http.StatusBadRequest, ErrorNetworkTestScopeInvalid, "label scope needs a label key", nil)
			return nil, "", "", false
		}
		for uuid, node := range onNetwork {
			labels, _ := store.NodeLabels(node.Labels)
			if v, ok := labels[key]; ok && v == value {
				uuids = append(uuids, uuid)
			}
		}
		labelKey, labelValue = key, value
	default:
		writeProductError(w, http.StatusBadRequest, ErrorNetworkTestScopeInvalid, "scope must be all, nodes or label", nil)
		return nil, "", "", false
	}
	sort.Strings(uuids)
	return uuids, labelKey, labelValue, true
}

func networkTestDTO(run *store.NetworkTestRun, nodes map[uint]store.Node) *dto.NetworkTestRun {
	if run == nil {
		return nil
	}
	names := map[string]string{}
	for _, n := range nodes {
		node := n
		names[n.UUID] = nodeDisplayName(&node)
	}
	kind := run.Kind
	if kind == "" {
		kind = networkTestTCP
	}
	pattern := run.Pattern
	if pattern == "" {
		pattern = network.TestPatternMesh
	}
	pairCount := run.PairCount
	if run.Status == store.NetworkTestRunning {
		pairCount = network.TestPairCount(kind, pattern, run.NodeCount)
	}
	out := &dto.NetworkTestRun{
		ID: run.ID, Status: run.Status, Kind: kind, Pattern: pattern, Scope: run.Scope, LabelKey: run.LabelKey, LabelValue: run.LabelValue, Bandwidth: run.Bandwidth,
		NodeCount: run.NodeCount, NodeUUIDs: append([]string{}, store.DecodeStringList(run.Nodes)...), PairCount: pairCount, PassedCount: run.PassedCount,
		Pairs: []dto.NetworkTestPair{}, StartedAt: run.StartedAt, FinishedAt: run.FinishedAt,
	}
	pairs := network.DecodeTestPairs(run.Pairs)
	card := network.CardTest(kind)
	for _, p := range pairs {
		status := network.TestPairStatus(kind, p, run.Bandwidth)
		if status == network.TestPartial {
			out.PartialCount++
		}
		var measuredRails int
		var railBandwidth float64
		rails := []dto.NetworkTestRail{}
		for _, r := range p.Rails {
			out.RailCount++
			if r.OK() {
				out.RailPassedCount++
				if kind == networkTestRDMATool && r.Gbps > 0 {
					measuredRails++
					railBandwidth += r.Gbps
				}
			}
			rails = append(rails, dto.NetworkTestRail{Rail: r.Rail, SourceIP: r.SourceIP, TargetRail: r.TargetRail, TargetIP: r.TargetIP, Gbps: r.Gbps, Reachable: r.Reachable, RTTMS: r.RTTMS, Code: r.Code, Side: r.Side, Error: r.Error})
		}
		if len(rails) == 0 {
			rails = nil
		}
		pair := dto.NetworkTestPair{
			SourceUUID: p.SourceUUID, SourceName: firstNonEmpty(names[p.SourceUUID], p.SourceUUID),
			TargetUUID: p.TargetUUID, TargetName: firstNonEmpty(names[p.TargetUUID], p.TargetUUID),
			Target: p.Target, Status: status, Passed: status == network.TestPassed, Reachable: p.Reachable,
			RTTP50MS: p.RTTP50MS, RTTP99MS: p.RTTP99MS, Gbps: p.Gbps, Code: p.Code, Side: p.Side, Error: p.Error, Rails: rails,
		}
		if measuredRails > 0 {
			pair.AvgRailGbps = railBandwidth / float64(measuredRails)
		}
		if card {
			pair.CardShape = network.CardPairShape(p)
		}
		out.Pairs = append(out.Pairs, pair)
	}
	if card && run.Status != store.NetworkTestRunning {
		// A card run counts the card pairs that were pinged: a card without
		// a namesake on the other node has a row but was never a probe.
		summary := network.SummarizeCardTest(pairs)
		out.CardShape, out.RailCount, out.RailPassedCount = summary.Shape, summary.Probed, summary.Reachable
	}
	return out
}
