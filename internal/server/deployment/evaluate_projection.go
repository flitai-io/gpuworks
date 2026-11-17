package deployment

import (
	"errors"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// evaluateWeightSourceOptions projects the selected spec's declared weight
// sources with the two facts the form's chooser needs: whether the server
// suggests a source and whether a matching model row already exists. The
// matching rule is the one the create path already applies
// (templateWeightMatchesModel); this projection only reads it.
func (s *Service) evaluateWeightSourceOptions(selection templateSelection) []dto.WeightSourceOption {
	sources := SpecWeightSources(selection.Spec)
	if len(sources) == 0 {
		return nil
	}
	models, err := s.d.Store.ModelArtifactsBySourceIDs(WeightSourceRepoIDs(sources)...)
	if err != nil {
		models = nil
	}
	out := make([]dto.WeightSourceOption, 0, len(sources))
	for _, source := range sources {
		option := dto.WeightSourceOption{Provider: source.Provider, RepoID: source.RepoID, Revision: source.Revision, Subfolder: source.Subfolder}
		// The suggestion is an identity (provider + repository); its revision
		// may have been resolved further than the declared row spells.
		option.Suggested = selection.SuggestedWeight.Provider == source.Provider && strings.EqualFold(selection.SuggestedWeight.RepoID, source.RepoID)
		for i := range models {
			if TemplateWeightMatchesModel(source, &models[i]) {
				option.Registered = true
				break
			}
		}
		out = append(out, option)
	}
	return out
}

// Inline preflight check IDs (ADR 0067: lower_snake). A per-node check
// repeats its ID once per node, with params.node naming it.
const (
	PreflightCheckNodeCount     = "node_count"
	PreflightCheckNodeList      = "node_list"
	PreflightCheckPort          = "port_range"
	PreflightCheckNodeOnline    = "node_online"
	PreflightCheckSharedNetwork = "shared_network"
)

// preflightRequestCheck is the request-level check a validator refusal
// belongs to, in the order the validator runs them.
func preflightRequestCheck(code string) string {
	switch code {
	case messages.ErrorPreflightMinimumNodes, messages.ErrorPreflightMaximumNodes:
		return PreflightCheckNodeCount
	case messages.ErrorPreflightNodeRequired, messages.ErrorPreflightNodeWhitespace, messages.ErrorPreflightDuplicateNode:
		return PreflightCheckNodeList
	default:
		return PreflightCheckPort
	}
}

// evaluateInlinePreflight answers the structural preflight judgements a
// multi-node draft needs on every keystroke. The request-level rules come from
// the preflight RPC's own validator (count, duplicates, whitespace, port
// range) so the two paths can never disagree; each named node's presence and
// online state are then checked individually, and last whether the nodes
// share a network. Every check is listed, passing ones included; a check that
// an earlier failure kept from running is skip with blocked_by. Deep network
// probing - listeners and bandwidth measurements over agent RPCs - stays with
// the preflight RPC, which the CLI keeps calling.
func (s *Service) evaluateInlinePreflight(req *dto.DeployRequest) *dto.PreflightSummary {
	summary := &dto.PreflightSummary{OK: true}
	add := func(check dto.PreflightCheck) {
		if check.Status == proto.CheckFail {
			summary.OK = false
		}
		summary.Checks = append(summary.Checks, check)
	}
	blocked := func(id, by string, params map[string]any) dto.PreflightCheck {
		if params == nil {
			params = map[string]any{}
		}
		params["blocked_by"] = by
		return dto.PreflightCheck{ID: id, Status: proto.CheckSkip, Code: messages.CheckPreflightBlocked, Params: params,
			Message: "not checked: check " + by + " failed first", BlockedBy: []string{by}}
	}
	preflightReq := DistributedPreflightRequest{NodeUUIDs: req.NodeUUIDs, MasterAddr: req.MasterAddr}
	if preflightReq.NodeUUIDs == nil && req.NodeUUID != "" {
		preflightReq.NodeUUIDs = []string{req.NodeUUID}
	}
	if req.MasterPort != 0 {
		preflightReq.Port = req.MasterPort
	}
	requestChecks := []string{PreflightCheckNodeCount, PreflightCheckNodeList, PreflightCheckPort}
	if err := ValidatePreflightRequest(&preflightReq); err != nil {
		failed := dto.PreflightCheck{ID: PreflightCheckPort, Status: proto.CheckFail, Code: messages.ErrorFieldInvalid,
			Message: "the preflight request is invalid", Evidence: err.Error()}
		var productErr *messages.ProductError
		if errors.As(err, &productErr) {
			failed = dto.PreflightCheck{ID: preflightRequestCheck(productErr.Code()), Status: proto.CheckFail,
				Code: productErr.Code(), Params: productErr.Params(), Message: productErr.Error()}
		}
		reached := false
		for _, id := range requestChecks {
			switch {
			case id == failed.ID:
				add(failed)
				reached = true
			case reached:
				add(blocked(id, failed.ID, nil))
			default:
				add(passedRequestCheck(id, preflightReq))
			}
		}
		for _, uuid := range preflightReq.NodeUUIDs {
			add(blocked(PreflightCheckNodeOnline, failed.ID, map[string]any{"node": uuid}))
		}
		add(blocked(PreflightCheckSharedNetwork, failed.ID, nil))
		return summary
	}
	for _, id := range requestChecks {
		add(passedRequestCheck(id, preflightReq))
	}
	nodes := make([]*store.Node, 0, len(preflightReq.NodeUUIDs))
	for _, uuid := range preflightReq.NodeUUIDs {
		params := map[string]any{"node": uuid}
		node, err := s.d.Store.NodeWithGPUs(uuid)
		switch {
		case errors.Is(err, store.ErrNotFound):
			add(dto.PreflightCheck{ID: PreflightCheckNodeOnline, Status: proto.CheckFail, Code: messages.CheckPreflightNodeNotFound,
				Params: params, Message: "node " + uuid + " does not exist"})
		case err != nil:
			// A store failure is the server's own; its text is logged, not shown.
			add(dto.PreflightCheck{ID: PreflightCheckNodeOnline, Status: proto.CheckFail, Code: messages.CheckPreflightNodeUnreadable,
				Params: params, Message: "node " + uuid + " could not be read"})
		case node.State != store.NodeStateOnline:
			add(dto.PreflightCheck{ID: PreflightCheckNodeOnline, Status: proto.CheckFail, Code: messages.ErrorPreflightNodeOffline,
				Params: params, Message: "node " + uuid + " is offline"})
		default:
			add(dto.PreflightCheck{ID: PreflightCheckNodeOnline, Status: proto.CheckPass, Code: messages.CheckPreflightNodeOnline,
				Params: params, Message: "node " + uuid + " is online"})
			nodes = append(nodes, node)
		}
	}
	if len(nodes) != len(preflightReq.NodeUUIDs) {
		add(blocked(PreflightCheckSharedNetwork, PreflightCheckNodeOnline, nil))
		return summary
	}
	// The ranks must share a network before they can be probed on it.
	group, err := s.ComputeGroup(nodes)
	var productErr *messages.ProductError
	switch {
	case err == nil:
		add(dto.PreflightCheck{ID: PreflightCheckSharedNetwork, Status: proto.CheckPass, Code: messages.CheckPreflightSharedNetworkOK,
			Params: map[string]any{"network": group.Network.Name}, Message: "the nodes share network " + group.Network.Name})
	case errors.As(err, &productErr):
		add(dto.PreflightCheck{ID: PreflightCheckSharedNetwork, Status: proto.CheckFail, Code: productErr.Code(),
			Params: productErr.Params(), Message: productErr.Error()})
	default:
		add(dto.PreflightCheck{ID: PreflightCheckSharedNetwork, Status: proto.CheckFail, Code: messages.CheckPreflightSharedNetworkUnknown,
			Message: "whether the nodes share a network could not be determined", Evidence: err.Error()})
	}
	return summary
}

// passedRequestCheck is a request-level check the validator accepted.
func passedRequestCheck(id string, req DistributedPreflightRequest) dto.PreflightCheck {
	switch id {
	case PreflightCheckNodeCount:
		return dto.PreflightCheck{ID: id, Status: proto.CheckPass, Code: messages.CheckPreflightNodeCountOK,
			Params: map[string]any{"count": len(req.NodeUUIDs)}, Message: fmt.Sprintf("%d nodes selected", len(req.NodeUUIDs))}
	case PreflightCheckNodeList:
		return dto.PreflightCheck{ID: id, Status: proto.CheckPass, Code: messages.CheckPreflightNodeListOK, Message: "every node is selected once"}
	default:
		return dto.PreflightCheck{ID: id, Status: proto.CheckPass, Code: messages.CheckPreflightPortOK,
			Params: map[string]any{"port": req.Port}, Message: fmt.Sprintf("port %d is in range", req.Port)}
	}
}
