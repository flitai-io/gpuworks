package deployment

import (
	"encoding/json"
	"errors"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/partitioncap"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Eligibility reason codes. They ride the ScheduleReason vocabulary so the
// client translates them the way it translates every other schedule reason;
// they describe the draft's own constraints, so they belong to the deploy form
// rather than the scheduler.
const (
	ReasonNodeOffline         = "node_offline"
	ReasonNodeCordoned        = "node_cordoned"
	ReasonRequirementsUnmet   = "node_requirements_unmet"
	ReasonRequirementsInvalid = "node_requirements_invalid"
	ReasonVendorMismatch      = "vendor_mismatch"
	ReasonChipMismatch        = "chip_mismatch"
)

// evaluateEligibleNodes runs every node through the predicate chain the draft
// itself states - online state, whether it takes new replicas, the node requirements (vendor included) and
// the template's GPU predicate - and reports each node eligible or not with
// its reasons. It is O(nodes) over the same facts evaluate already resolves
// and deliberately does no scheduling: scoring is the candidates list's job,
// not this one. Requirements that could not be read (reqsErr) make every
// node ineligible: the dropdown must not offer nodes the draft may exclude.
//
// For an automatic draft (automatic) it also answers which dGPU backend the
// eligible pool offers: the first eligible node's ready one
// (partitioncap.Preferred), nil when none has one. Such a draft has no node to
// read that from until a placement fits, and a placement that only fits as a
// dGPU slice never fits before the quota is set, so the form gates its quota
// on this instead. A draft that names its node reads the node's own, so the
// per-node derivation is skipped for it.
func (s *Service) evaluateEligibleNodes(selection templateSelection, reqs map[string][]string, reqsErr error, automatic bool, loadTargetRequests func(*store.Node) map[int]planning.TargetCardRequest) ([]dto.EligibleNode, *proto.PartitionCapability) {
	nodes, err := s.d.Store.ListNodes()
	if err != nil || len(nodes) == 0 {
		return nil, nil
	}
	var predicate *chipmatch.Predicate
	var unsupported []chipmatch.Unsupported
	if selection.Spec != nil {
		decoded, err := DecodeSpecPredicate(*selection.Spec)
		if err != nil {
			predicate = nil
		} else {
			predicate = &decoded
		}
		if selection.Model != nil {
			unsupported, _ = decodeUnsupported(*selection.Model)
		}
	}
	all := make([]*store.Node, 0, len(nodes))
	for index := range nodes {
		all = append(all, &nodes[index])
	}
	resolved := s.resolveForRequirements(all, reqs)
	// The default target vendor is a hardware constraint, not evidence that a
	// user's label selection worked. Match the remaining conditions independently
	// so a vendor failure cannot mask a later label mismatch.
	explicit := make(map[string][]string, len(reqs))
	for key, values := range reqs {
		if !planning.IsVendorRequirement(&planning.UnmetRequirement{Key: key}) {
			explicit[key] = values
		}
	}
	out := make([]dto.EligibleNode, 0, len(nodes))
	var poolPartition *proto.PartitionCapability
	for _, node := range all {
		name := node.Name
		if name == "" {
			name = node.Hostname
		}
		entry := dto.EligibleNode{UUID: node.UUID, Name: name, Eligible: true}
		if reqsErr == nil && len(explicit) > 0 {
			matches := planning.MatchRequirements(node, resolved, explicit) == nil
			entry.RequirementsMatch = &matches
		}
		reason := func(code string, params map[string]any) {
			entry.Eligible = false
			entry.Reasons = append(entry.Reasons, dto.ScheduleReason{Code: code, Params: params})
		}
		if node.State != store.NodeStateOnline {
			reason(ReasonNodeOffline, nil)
		}
		if !node.Scheduling.TakesReplicas() {
			reason(ReasonNodeCordoned, map[string]any{"node": node.DisplayName()})
		}
		if reqsErr != nil {
			reason(ReasonRequirementsInvalid, map[string]any{"detail": reqsErr.Error()})
		} else if err := planning.MatchRequirements(node, resolved, reqs); err != nil {
			var unmet *planning.UnmetRequirement
			switch {
			case errors.As(err, &unmet) && planning.IsVendorRequirement(unmet):
				reason(ReasonVendorMismatch, map[string]any{"vendor": planning.RequirementVendor(reqs), "actual_gpus": reportedGPUFacts(node)})
			default:
				reason(ReasonRequirementsUnmet, map[string]any{"detail": err.Error()})
			}
		}
		if predicate != nil && !nodeMatchesChipPredicate(node, *predicate, unsupported) {
			var required struct {
				ArchID string `json:"arch_id"`
			}
			_ = json.Unmarshal([]byte(selection.Spec.GPUs), &required)
			params := map[string]any{"predicate": strings.TrimSpace(selection.Spec.GPUs), "required_vendor": predicate.Vendor, "required_cc": predicate.CC, "required_name": predicate.Name, "actual_gpus": reportedGPUFacts(node)}
			if archID := strings.TrimSpace(required.ArchID); archID != "" {
				params["required_arch_id"] = archID
			}
			reason(ReasonChipMismatch, params)
		}
		// Only a node that passed the ordinary eligibility filters needs the
		// resolved launch capability preflight. Mixed nodes are checked by physical
		// card below; offline, cordoned, mismatched and template-ineligible nodes
		// do not trigger request parsing.
		if entry.Eligible && planning.NeedsTargetPreflight(node) {
			requests := map[int]planning.TargetCardRequest{}
			if loadTargetRequests != nil {
				requests = loadTargetRequests(node)
			}
			for _, refusal := range planning.TargetRefusalsByCard(node, func(gpu store.GPU) bool {
				return predicate == nil || gpuMatchesChipPredicate(gpu, *predicate, unsupported)
			}, func(gpu store.GPU) planning.TargetCardRequest { return requests[gpu.Index] }) {
				reason(refusal.Code, refusal.Params)
			}
		}
		if automatic && entry.Eligible && poolPartition == nil {
			if capability, ok := partitioncap.Preferred(node); ok {
				poolPartition = &capability
			}
		}
		out = append(out, entry)
	}
	return out, poolPartition
}

// nodeMatchesChipPredicate answers whether any reported GPU satisfies the
// template's GPU predicate under the model's unsupported rules.
func nodeMatchesChipPredicate(node *store.Node, predicate chipmatch.Predicate, unsupported []chipmatch.Unsupported) bool {
	for _, gpu := range node.OwnedGPUs() {
		if gpuMatchesChipPredicate(gpu, predicate, unsupported) {
			return true
		}
	}
	return false
}

// Report facts, not a second predicate implementation. Empty CC stays unknown;
// mixed cards retain their own capabilities instead of inventing a node-wide CC.
func reportedGPUFacts(node *store.Node) []map[string]string {
	out := make([]map[string]string, 0, len(node.OwnedGPUs()))
	for _, gpu := range node.OwnedGPUs() {
		reported := chipmatch.ReportedGPU(gpu.Name, gpu.Vendor, gpu.ArchID, gpu.ComputeCapability)
		facts := map[string]string{"name": reported.Name, "vendor": reported.Vendor, "cc": strings.TrimSpace(gpu.ComputeCapability)}
		if archID := strings.TrimSpace(gpu.ArchID); archID != "" {
			facts["arch_id"] = archID
		}
		out = append(out, facts)
	}
	return out
}

func gpuMatchesChipPredicate(gpu store.GPU, predicate chipmatch.Predicate, unsupported []chipmatch.Unsupported) bool {
	reported := chipmatch.ReportedGPU(gpu.Name, gpu.Vendor, gpu.ArchID, gpu.ComputeCapability)
	return chipmatch.Select(reported, []chipmatch.Spec{{GPUs: predicate}}, unsupported).Matched
}
