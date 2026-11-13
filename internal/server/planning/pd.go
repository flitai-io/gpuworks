// pd.go reads and validates the PD disaggregation part of a placement
// (ADR 0028). It is the one check evaluate, create and the controller share:
// the rules themselves come from package pdmode, this file only applies them
// to a placement and to the nodes it names.

package planning

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// PDPlacement is a PD deployment's roles: which mode, and for each role its
// tensor parallel degree and one unit per instance.
type PDPlacement struct {
	Mode  string            `json:"mode"`
	Roles map[string]PDRole `json:"roles"`
	// KVLoadFailure is the decode engine's policy when pulling KV fails
	// (pdmode.KVLoad*); empty is the default.
	KVLoadFailure string `json:"kv_load_failure,omitempty"`
	// Fallback is the gateway's behaviour when no prefill instance can take
	// a request (pdmode.Fallback*); empty is the default.
	Fallback string `json:"fallback,omitempty"`
}

// PDRole is one role's shape. TP and PP are degrees over one instance
// (package parallelism); every unit of the role runs them.
type PDRole struct {
	TP    int      `json:"tp,omitempty"`
	PP    int      `json:"pp,omitempty"`
	Units []PDUnit `json:"units"`
}

// PDUnit is one instance: the node its leader runs on and, optionally, the
// GPUs it is pinned to. Without GPUs the scheduler picks the rank's share on
// the node. Followers make the instance a multi-node group: each is one more
// headless rank on its own node, in rank order (plan stage 4c). The leader is
// what the gateway and the other role talk to.
type PDUnit struct {
	NodeUUID   string   `json:"node_uuid"`
	GPUIndexes []int    `json:"gpus,omitempty"`
	Followers  []PDRank `json:"followers,omitempty"`
}

// PDRank is one follower rank of a multi-node unit.
type PDRank struct {
	NodeUUID   string `json:"node_uuid"`
	GPUIndexes []int  `json:"gpus,omitempty"`
}

// parsePD reads the "pd" object of a placement document. A PD section that
// cannot be read is an error, never an ordinary deployment: silently dropping
// it would launch every instance without its KV connector.
func parsePD(document map[string]any) (*PDPlacement, error) {
	raw, present := document["pd"]
	if !present || raw == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("placement.pd: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	var pd PDPlacement
	if err := decoder.Decode(&pd); err != nil {
		return nil, fmt.Errorf("placement.pd: %w", err)
	}
	pd.Mode = strings.TrimSpace(pd.Mode)
	for role, shape := range pd.Roles {
		for i := range shape.Units {
			shape.Units[i].NodeUUID = strings.TrimSpace(shape.Units[i].NodeUUID)
			for j := range shape.Units[i].Followers {
				shape.Units[i].Followers[j].NodeUUID = strings.TrimSpace(shape.Units[i].Followers[j].NodeUUID)
			}
		}
		pd.Roles[role] = shape
	}
	return &pd, nil
}

// PDOf reads the PD section of a placement, stored (the column text) or
// requested (the document): nil without an error for any other placement, an
// error when the placement cannot be read. A placement that cannot be read
// is never quietly an ordinary one: each caller decides what refusing it
// means (the controller holds the deployment, the gateway does not route
// it). The mode is not checked here; ValidatePD does that.
func PDOf(value any) (*PDPlacement, error) {
	if text, ok := value.(string); ok && !strings.Contains(text, `"pd"`) {
		return nil, nil
	}
	placement, err := ParsePlacement(value)
	if err != nil {
		return nil, err
	}
	return placement.PD, nil
}

// EffectiveFallback is the gateway fallback the placement means: its value,
// or its mode's default.
func (p *PDPlacement) EffectiveFallback() string {
	if p == nil {
		return ""
	}
	mode, _ := pdmode.Lookup(p.Mode)
	return mode.EffectiveFallback(p.Fallback)
}

// EffectiveKVLoadFailure is the decode engine's policy the placement means:
// its value, or its mode's default (none for an engine without one).
func (p *PDPlacement) EffectiveKVLoadFailure() string {
	if p == nil {
		return ""
	}
	mode, _ := pdmode.Lookup(p.Mode)
	return mode.EffectiveKVLoadFailure(p.KVLoadFailure)
}

// TP is a role's tensor parallel degree: the stated one, else what the
// pinned GPUs of its first unit leave after the pipeline stages, else one.
// A multi-node unit must state it (parallelism.ErrTPRequired).
func (p *PDPlacement) TP(role string) int {
	shape := p.Roles[role]
	if shape.TP > 0 {
		return shape.TP
	}
	if len(shape.Units) > 0 && len(shape.Units[0].GPUIndexes) > 0 {
		pinned, pp := len(shape.Units[0].GPUIndexes), max(shape.PP, 1)
		if pinned%pp == 0 {
			return pinned / pp
		}
		return pinned
	}
	return 1
}

// RoleTPs is every role's TP, for launches whose connector names both sides.
func (p *PDPlacement) RoleTPs(mode pdmode.Mode) map[string]int {
	out := map[string]int{}
	for _, role := range mode.Roles {
		out[role] = p.TP(role)
	}
	return out
}

// Units is a role's instance list.
func (p *PDPlacement) Units(role string) []PDUnit {
	if p == nil {
		return nil
	}
	return p.Roles[role].Units
}

// NodeUUIDs lists the nodes the placement names, in the mode's role order and
// then unit order, without duplicates. The first one is the deployment's
// primary node.
func (p *PDPlacement) NodeUUIDs(mode pdmode.Mode) []string {
	seen := map[string]bool{}
	var out []string
	for _, role := range mode.Roles {
		for _, unit := range p.Roles[role].Units {
			for _, rank := range unit.Ranks() {
				if rank.NodeUUID != "" && !seen[rank.NodeUUID] {
					seen[rank.NodeUUID] = true
					out = append(out, rank.NodeUUID)
				}
			}
		}
	}
	return out
}

// InstanceCount is the number of instances all roles ask for.
func (p *PDPlacement) InstanceCount() int {
	total := 0
	for _, shape := range p.Roles {
		total += len(shape.Units)
	}
	return total
}

func pdError(code, fallback string, params map[string]any) error {
	return messages.NewProductError(code, fallback, params)
}

// ValidatePD checks a PD placement against its mode without looking at any
// node: the mode exists and serves the engine, every role the mode declares is
// present with 1..MaxUnitsPerRole units, pinned GPUs match the role's TP and
// do not overlap on a node, the TP direction rule holds, and the policy values
// are known. It returns the mode.
func ValidatePD(p *PDPlacement, engine string) (pdmode.Mode, error) {
	return validatePD(p, engine, false)
}

// ValidateRunningPD is ValidatePD for a deployment that already runs: a role
// the mode may scale to zero may have no units (plan D14).
func ValidateRunningPD(p *PDPlacement, engine string) (pdmode.Mode, error) {
	return validatePD(p, engine, true)
}

func validatePD(p *PDPlacement, engine string, running bool) (pdmode.Mode, error) {
	mode, ok := pdmode.Lookup(p.Mode)
	if !ok {
		return pdmode.Mode{}, pdError(messages.ErrorPDModeUnknown, fmt.Sprintf("unknown PD mode %q", p.Mode), map[string]any{"mode": p.Mode, "known": strings.Join(pdmode.Names(), ", ")})
	}
	if engine != "" && engine != mode.Engine {
		return mode, pdError(messages.ErrorPDModeEngineMismatch, fmt.Sprintf("PD mode %s runs on %s, not %s", mode.Name, mode.Engine, engine), map[string]any{"mode": mode.Name, "engine": engine, "required": mode.Engine})
	}
	roles := make([]string, 0, len(p.Roles))
	for role := range p.Roles {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		if !mode.HasRole(role) {
			return mode, pdError(messages.ErrorPDRoleUnknown, fmt.Sprintf("PD mode %s has no role %q", mode.Name, role), map[string]any{"mode": mode.Name, "role": role})
		}
	}
	type gpuKey struct {
		node string
		gpu  int
	}
	used := map[gpuKey]string{}
	for _, role := range mode.Roles {
		shape, present := p.Roles[role]
		if !present {
			return mode, pdError(messages.ErrorPDRoleMissing, fmt.Sprintf("PD placement needs the %s role", role), map[string]any{"role": role})
		}
		if len(shape.Units) == 0 && !(running && mode.MayBeEmpty(role)) {
			return mode, pdError(messages.ErrorPDRoleUnitsEmpty, fmt.Sprintf("the %s role needs at least one instance", role), map[string]any{"role": role})
		}
		if len(shape.Units) > pdmode.MaxUnitsPerRole {
			return mode, pdError(messages.ErrorPDRoleUnitsTooMany, fmt.Sprintf("the %s role has more than %d instances", role, pdmode.MaxUnitsPerRole), map[string]any{"role": role, "maximum": pdmode.MaxUnitsPerRole})
		}
		if shape.TP < 0 || shape.PP < 0 {
			return mode, pdError(messages.ErrorPDTPInvalid, fmt.Sprintf("the %s role has an invalid TP %d or PP %d", role, shape.TP, shape.PP), map[string]any{"role": role, "tp": shape.TP})
		}
		tp := p.TP(role)
		for _, unit := range shape.Units {
			if err := p.checkUnitRanks(role, unit); err != nil {
				return mode, err
			}
			share := p.RankGPUs(role, unit)
			for _, rank := range unit.Ranks() {
				if len(rank.GPUIndexes) == 0 {
					continue
				}
				if len(rank.GPUIndexes) != share {
					return mode, pdError(messages.ErrorPDUnitGPUCount, fmt.Sprintf("an instance of the %s role pins %d GPUs on %s but each of its ranks runs %d (TP %d)", role, len(rank.GPUIndexes), rank.NodeUUID, share, tp), map[string]any{"role": role, "node": rank.NodeUUID, "gpus": len(rank.GPUIndexes), "tp": tp, "required": share})
				}
				for _, gpu := range rank.GPUIndexes {
					key := gpuKey{rank.NodeUUID, gpu}
					if other, taken := used[key]; taken {
						return mode, pdError(messages.ErrorPDGPUOverlap, fmt.Sprintf("GPU %d on %s is used by two instances (%s and %s)", gpu, rank.NodeUUID, other, role), map[string]any{"node": rank.NodeUUID, "gpu": gpu, "role": role, "other": other})
					}
					used[key] = role
				}
			}
		}
	}
	if err := mode.CheckTP(p.TP(pdmode.RolePrefill), p.TP(pdmode.RoleDecode)); err != nil && len(p.Units(pdmode.RolePrefill)) > 0 {
		return mode, err
	}
	if !mode.ValidFallback(p.Fallback) {
		return mode, pdError(messages.ErrorPDFallbackInvalid, fmt.Sprintf("unknown PD fallback %q", p.Fallback), map[string]any{"value": p.Fallback})
	}
	if !mode.ValidKVLoadFailure(p.KVLoadFailure) {
		return mode, pdError(messages.ErrorPDKVLoadFailureInvalid, fmt.Sprintf("unknown KV load failure policy %q", p.KVLoadFailure), map[string]any{"value": p.KVLoadFailure})
	}
	return mode, nil
}

// peerMemoryVendor is the vendor whose peer-memory module a node needs: its
// cards', else the mode's.
func peerMemoryVendor(node *store.Node, mode pdmode.Mode) string {
	switch {
	case node.GPUVendor != "":
		return node.GPUVendor
	case len(node.GPUs) > 0 && node.GPUs[0].Vendor != "":
		return node.GPUs[0].Vendor
	case len(mode.Vendors) > 0:
		return mode.Vendors[0]
	}
	return ""
}

// ValidatePDNodes checks the placement against the nodes it names (by UUID,
// with GPUs loaded): every node exists, is online when requireOnline, carries
// the pinned GPUs or enough GPUs for its unpinned units, and every GPU is of a
// vendor the mode serves — the same vendor across roles when the mode says so.
// It returns the warnings a valid placement still deserves.
func ValidatePDNodes(p *PDPlacement, mode pdmode.Mode, nodes map[string]*store.Node, requireOnline bool) ([]messages.Warning, error) {
	type demand struct{ unpinned, pinned int }
	perNode := map[string]*demand{}
	vendors := map[string]bool{}
	models := map[string]bool{}
	for _, role := range mode.Roles {
		for _, unit := range p.Roles[role].Units {
			share := p.RankGPUs(role, unit)
			for _, rank := range unit.Ranks() {
				node := nodes[rank.NodeUUID]
				if node == nil {
					return nil, pdError(messages.ErrorPDNodeUnknown, fmt.Sprintf("node %s does not exist", rank.NodeUUID), map[string]any{"role": role, "node": rank.NodeUUID})
				}
				if requireOnline && node.State != store.NodeStateOnline {
					return nil, pdError(messages.ErrorPDNodeOffline, fmt.Sprintf("node %s is not online", node.DisplayName()), map[string]any{"role": role, "node": rank.NodeUUID})
				}
				if mode.PeerMemory && node.PeerMemory != proto.PeerMemoryLoaded {
					vendor := peerMemoryVendor(node, mode)
					return nil, pdError(messages.ErrorPDPeerMemoryMissing,
						fmt.Sprintf("node %s cannot register its cards' memory with the RDMA NIC (%s not loaded), which PD mode %s needs", node.DisplayName(), accelerator.PeerMemoryModule(vendor), mode.Name),
						map[string]any{"mode": mode.Name, "role": role, "node": node.DisplayName(), "module": accelerator.PeerMemoryModule(vendor), "fix": accelerator.PeerMemoryFix(vendor), "state": node.PeerMemory})
				}
				d := perNode[rank.NodeUUID]
				if d == nil {
					d = &demand{}
					perNode[rank.NodeUUID] = d
				}
				cards := rank.GPUIndexes
				if len(cards) == 0 {
					d.unpinned += share
					for _, gpu := range node.OwnedGPUs() {
						vendors[node.VendorOf(gpu)] = true
						models[gpu.Name] = true
					}
					continue
				}
				d.pinned += len(cards)
				for _, index := range cards {
					gpu, ok := node.GPUByIndex(index)
					if !ok {
						return nil, pdError(messages.ErrorPDNodeGPUMissing, fmt.Sprintf("node %s has no GPU %d", node.DisplayName(), index), map[string]any{"role": role, "node": rank.NodeUUID, "gpu": index})
					}
					vendors[node.VendorOf(gpu)] = true
					models[gpu.Name] = true
				}
			}
		}
	}
	for uuid, d := range perNode {
		node := nodes[uuid]
		available := len(node.OwnedGPUs())
		if need := d.unpinned + d.pinned; need > available {
			return nil, pdError(messages.ErrorPDNodeGPUsInsufficient, fmt.Sprintf("node %s has %d GPUs, the instances placed on it need %d", node.DisplayName(), available, need), map[string]any{"node": uuid, "available": available, "required": need})
		}
	}
	vendorList := make([]string, 0, len(vendors))
	for vendor := range vendors {
		vendorList = append(vendorList, vendor)
	}
	sort.Strings(vendorList)
	for _, vendor := range vendorList {
		if !mode.SupportsVendor(vendor) {
			return nil, pdError(messages.ErrorPDVendorUnsupported, fmt.Sprintf("PD mode %s does not run on %s", mode.Name, vendor), map[string]any{"mode": mode.Name, "vendor": vendor})
		}
	}
	if mode.SameVendor && len(vendorList) > 1 {
		return nil, pdError(messages.ErrorPDVendorMixed, "PD roles must run on one accelerator vendor", map[string]any{"vendors": strings.Join(vendorList, ", ")})
	}
	if mode.SameNode && len(perNode) > 1 {
		names := make([]string, 0, len(perNode))
		for uuid := range perNode {
			names = append(names, nodes[uuid].DisplayName())
		}
		sort.Strings(names)
		return nil, pdError(messages.ErrorPDSameNodeRequired, fmt.Sprintf("PD mode %s moves the KV inside one host; its instances are on %d nodes", mode.Name, len(perNode)), map[string]any{"mode": mode.Name, "nodes": strings.Join(names, ", ")})
	}
	var warnings []messages.Warning
	if len(models) > 1 {
		names := make([]string, 0, len(models))
		for name := range models {
			names = append(names, name)
		}
		sort.Strings(names)
		warnings = append(warnings, messages.Warning{Code: messages.WarningPDGPUModelsDiffer, Params: map[string]any{"models": strings.Join(names, ", ")}})
	}
	if len(perNode) == 1 && !mode.SameNode {
		warnings = append(warnings, messages.Warning{Code: messages.WarningPDSameNodeThroughput})
	}
	return warnings, nil
}

// CheckPDArgs rejects argument layers that state a flag the engine's adapter
// composes for a PD instance's KV connector (deploy.EngineHooks.
// PDConnectorFlags): the platform composes them, and a layer stating one
// would silently turn every instance into an ordinary replica.
func CheckPDArgs(engine string, layers ...[]string) error {
	flags := deploy.PDConnectorFlagsForEngine(engine)
	for _, args := range layers {
		for _, arg := range args {
			for _, flag := range flags {
				if arg == flag || strings.HasPrefix(arg, flag+"=") {
					return pdError(messages.ErrorPDConnectorArgConflict, fmt.Sprintf("%s is composed by the platform for a PD deployment; set KV offload with the launch parameters instead", flag), map[string]any{"flag": flag})
				}
			}
		}
	}
	return nil
}

// PDFeatureWarnings are the notices for combinations that are allowed but
// not verified, and where KV offload runs. Sleep is no longer one: it is
// verified with KV over Ethernet and refused over RDMA (CheckPDNetwork).
func PDFeatureWarnings(kvOffload, partitioned bool) []messages.Warning {
	var out []messages.Warning
	if kvOffload {
		out = append(out, messages.Warning{Code: messages.WarningPDKVOffloadPrefillOnly})
	}
	if partitioned {
		out = append(out, messages.Warning{Code: messages.WarningPDUnverified, Params: map[string]any{"feature": "dgpu"}})
	}
	return out
}

// PDGroup is the network a PD group's KV transfer uses, given the compute
// group the network service resolved for its nodes. A group confined to one
// host keeps the KV on it, so when no network there may carry compute
// traffic the side channel falls back to loopback instead of failing; a group
// across hosts has nothing to fall back to.
func PDGroup(nodes []store.Node, group netplan.Group, err error) (netplan.Group, error) {
	if err == nil {
		return group, nil
	}
	if len(nodes) != 1 {
		return group, err
	}
	id := nodes[0].ID
	return netplan.Group{Ranks: map[uint]netplan.Resolved{id: {NodeID: id, IP: "127.0.0.1"}}}, nil
}
