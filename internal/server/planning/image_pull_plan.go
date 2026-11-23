package planning

import (
	"errors"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/pullref"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/sourceurl"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// NodeResolver resolves the networks of nodes (network.Service.Resolve). A
// nil resolver leaves network requirements unmet, as LoadNodeFacts does.
type NodeResolver func([]store.Node) (netplan.Resolution, error)

// ImagePullPlan is how one instance gets its runtime image: the references
// that name the image (recorded on the instance), where this node pulls it
// from, and the digest the node already holds.
type ImagePullPlan struct {
	Sources    []string
	Candidates []proto.ImagePullCandidate
	DigestID   string
}

// Refs is the flat order an agent that cannot rank candidates tries.
func (p ImagePullPlan) Refs() []string {
	out := make([]string, 0, len(p.Candidates))
	for _, candidate := range p.Candidates {
		out = append(out, candidate.Ref)
	}
	return out
}

// ImagePullPlanFor reads one launch's image facts for one node. The launch's
// image was resolved once, at create time; this reads the catalog row for the
// stored ref instead of resolving it again. The row contributes acquisition
// facts only - its registry sources and the node's digest (ADR 0031).
//
// Where the node pulls from is not the deployment's to say: the image's own
// copies (the template's sources, ADR 0038, then the catalog's) are planned
// against the pull source rules that apply to the node (PullCandidatesFor).
func ImagePullPlanFor(st *store.Store, resolve NodeResolver, source LaunchSource, node *store.Node, selection deploy.CatalogSelection) (ImagePullPlan, error) {
	if node == nil {
		return ImagePullPlan{}, fmt.Errorf("image pull plan requires a node")
	}
	runtimeImage := strings.TrimSpace(source.Image)
	if runtimeImage == "" {
		return ImagePullPlan{}, fmt.Errorf("deployment %d has no runtime image", source.DeploymentID)
	}
	var plan ImagePullPlan
	if st != nil {
		if row, err := st.ResolveImageRef(runtimeImage); err == nil {
			if imageNode, nodeErr := st.ImageNode(row.ID, node.ID); nodeErr == nil {
				plan.DigestID = imageNode.ImageDigestID
			}
		}
		plan.Sources = st.ImageCopies(runtimeImage, selection.ImageSourcesFor(runtimeImage))
	} else {
		plan.Sources = []string{runtimeImage}
	}
	candidates, err := PullCandidatesFor(st, resolve, node, runtimeImage, plan.Sources)
	if err != nil {
		return plan, err
	}
	plan.Candidates = candidates
	return plan, nil
}

// PullCandidatesFor plans where node pulls canonical from, given the
// references that name the same image (store.ImageCopies), with the pull
// source rules that apply to node. It is the one place a pull's candidates
// are decided: a launch, a desired-state replay, the image page and a
// multi-node pre-pull all come here (a replay through PullCandidatesWith, so
// the node's rules are selected once for all its instances). A node the rules
// leave nothing to pull from is an ErrorImageSourceForbidden product error
// wrapping pullref.ErrNoAllowedSource.
func PullCandidatesFor(st *store.Store, resolve NodeResolver, node *store.Node, canonical string, copies []string) ([]proto.ImagePullCandidate, error) {
	rules, err := RulesFor(st, resolve, node)
	if err != nil {
		return nil, err
	}
	return PullCandidatesWith(ImageRules(rules), node, canonical, copies)
}

// PullCandidatesWith is PullCandidatesFor with the node's image rules already
// selected (RulesFor, then ImageRules).
func PullCandidatesWith(rules []pullref.SourceRule, node *store.Node, canonical string, copies []string) ([]proto.ImagePullCandidate, error) {
	planned, err := pullref.PlanCandidates(canonical, copies, rules)
	if errors.Is(err, pullref.ErrNoAllowedSource) {
		return nil, messages.WrapProductError(messages.ErrorImageSourceForbidden, err.Error(),
			map[string]any{"node": node.DisplayName(), "route": "/api/v1/pull/source-rules"}, err)
	}
	if err != nil {
		return nil, err
	}
	out := make([]proto.ImagePullCandidate, 0, len(planned))
	for _, candidate := range planned {
		out = append(out, proto.ImagePullCandidate{Ref: candidate.Ref, Rank: candidate.Rank})
	}
	return out, nil
}

// ImageSourceRefusal is the scheduling refusal for a launch planning error
// that says the pull source rules leave node nothing to pull image from. The
// launch and the deploy form's evaluation both report it this way, so the
// form says what a replica would.
func ImageSourceRefusal(err error, node *store.Node, image string) (scheduler.Reason, bool) {
	if !errors.Is(err, pullref.ErrNoAllowedSource) || node == nil {
		return scheduler.Reason{}, false
	}
	return scheduler.ImageSourceForbidden(node.DisplayName(), image), true
}

// RulesFor returns the pull source rules of both kinds that apply to node,
// in the order they are tried. A rule applies when node meets its node
// requirements; a rule with none applies to every node. Image pulls and model
// fetches each take their own kind (ImageRules, URLRules) from this one
// selection, so a node's labels are read once for both.
func RulesFor(st *store.Store, resolve NodeResolver, node *store.Node) ([]store.PullSourceRuleSpec, error) {
	selection, err := PullSettingsFor(st, resolve, node)
	return selection.Rules, err
}

// NodePullSettings is what the pull settings say about one node: the rules
// that apply to it, and the proxy entry it uses (none when Proxy is nil).
type NodePullSettings struct {
	Rules []store.PullSourceRuleSpec
	Proxy *store.PullProxySpec
}

// Network is the node's proxy as it is sent in the desired state: the
// entry's addresses and login, or nothing.
func (s NodePullSettings) Network() proto.NetworkConfig {
	if s.Proxy == nil {
		return proto.NetworkConfig{}
	}
	return s.Proxy.NetworkConfig()
}

// PullSettingsFor selects the rules and the proxy entry that apply to node,
// reading its labels once for both. A rule applies when node meets its
// requirements; the proxy is the first entry, in their order, whose
// requirements node meets - a node has one proxy, while rules add up.
func PullSettingsFor(st *store.Store, resolve NodeResolver, node *store.Node) (NodePullSettings, error) {
	if st == nil || node == nil {
		return NodePullSettings{}, nil
	}
	specs, err := st.PullSourceRules()
	if err != nil {
		return NodePullSettings{}, err
	}
	proxies, err := st.PullProxies()
	if err != nil {
		return NodePullSettings{}, err
	}
	if len(specs) == 0 && len(proxies) == 0 {
		return NodePullSettings{}, nil
	}
	reqs := make([]nodelabel.Requirements, 0, len(specs)+len(proxies))
	for _, spec := range specs {
		reqs = append(reqs, spec.Requirements)
	}
	for _, proxy := range proxies {
		reqs = append(reqs, proxy.Requirements)
	}
	facts := loadSelectorFacts(st, resolve, []store.Node{*node}, reqs)
	var out NodePullSettings
	for _, spec := range specs {
		if MatchRequirements(node, facts, spec.Requirements) == nil {
			out.Rules = append(out.Rules, spec)
		}
	}
	for i := range proxies {
		if MatchRequirements(node, facts, proxies[i].Requirements) == nil {
			proxy := proxies[i]
			out.Proxy = &proxy
			break
		}
	}
	return out, nil
}

// ControlPlaneNetwork is ControlPlaneProxy as an outbound client applies it
// (outbound.Transport): the entry's addresses and login, or nothing.
func ControlPlaneNetwork(st *store.Store) (proto.NetworkConfig, error) {
	if st == nil {
		return proto.NetworkConfig{}, nil
	}
	proxy, err := ControlPlaneProxy(st)
	if err != nil || proxy == nil {
		return proto.NetworkConfig{}, err
	}
	return proxy.NetworkConfig(), nil
}

// ControlPlaneProxy is the proxy the management node's outbound requests go
// through: the first entry chosen for it, if any. The management node is not
// a node and no requirement selects it.
func ControlPlaneProxy(st *store.Store) (*store.PullProxySpec, error) {
	proxies, err := st.PullProxies()
	if err != nil {
		return nil, err
	}
	for i := range proxies {
		if proxies[i].ControlPlane {
			proxy := proxies[i]
			return &proxy, nil
		}
	}
	return nil, nil
}

// ImageRules keeps the registry rules of rules, as the image planner reads them.
func ImageRules(rules []store.PullSourceRuleSpec) []pullref.SourceRule {
	var out []pullref.SourceRule
	for _, rule := range rules {
		if rule.Kind() == store.PullRuleImage {
			out = append(out, rule.SourceRule())
		}
	}
	return out
}

// URLRules keeps the site rules of rules, as the fetch planner reads them.
func URLRules(rules []store.PullSourceRuleSpec) []sourceurl.Rule {
	var out []sourceurl.Rule
	for _, rule := range rules {
		if rule.Kind() == store.PullRuleURL {
			out = append(out, rule.URLRule())
		}
	}
	return out
}

// SyncPeer is a node a target may copy a model or image from automatically,
// with the rate cap of the groups they share (0 = none).
type SyncPeer struct {
	NodeID       uint
	RateLimitKiB int64
	// Network names the network a copy from this peer takes, once
	// WithStoragePaths has looked: netplan.ManagementName for the management
	// network.
	Network string
}

// WithStoragePaths keeps the peers target shares a network with that may
// carry storage sync, each named by that network, those on a storage-sync
// network of their own first: sharing a sync group says a copy is wanted, not
// that the two nodes have a way to make it. sources must hold every peer.
func WithStoragePaths(resolve NodeResolver, target *store.Node, sources []store.Node, peers []SyncPeer) ([]SyncPeer, error) {
	if resolve == nil || target == nil || len(peers) == 0 {
		return nil, nil
	}
	byID := make(map[uint]*store.Node, len(sources))
	nodes := make([]store.Node, 0, len(peers)+1)
	nodes = append(nodes, *target)
	for i := range sources {
		byID[sources[i].ID] = &sources[i]
	}
	for _, peer := range peers {
		if node := byID[peer.NodeID]; node != nil {
			nodes = append(nodes, *node)
		}
	}
	resolved, err := resolve(nodes)
	if err != nil {
		return nil, err
	}
	to := network.Node(*target)
	var own, shared []SyncPeer
	for _, peer := range peers {
		node := byID[peer.NodeID]
		if node == nil {
			continue
		}
		path, err := netplan.StoragePathBetween(network.Node(*node), to, resolved)
		if err != nil {
			continue
		}
		peer.Network = path.Network
		if path.Network == netplan.ManagementName {
			shared = append(shared, peer)
		} else {
			own = append(own, peer)
		}
	}
	return append(own, shared...), nil
}

// SyncPeersFor filters sources down to the nodes target shares a sync group
// with, in the order given. A node in several groups may copy from a node of
// any of them; when two nodes share several groups the strictest cap applies.
// A node in no group copies from no one automatically.
func SyncPeersFor(st *store.Store, resolve NodeResolver, target *store.Node, sources []store.Node) ([]SyncPeer, error) {
	if st == nil || target == nil || len(sources) == 0 {
		return nil, nil
	}
	groups, err := st.SyncGroups()
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, nil
	}
	nodes := append([]store.Node{*target}, sources...)
	reqs := make([]nodelabel.Requirements, 0, len(groups))
	for _, group := range groups {
		reqs = append(reqs, group.Requirements)
	}
	facts := loadSelectorFacts(st, resolve, nodes, reqs)
	var targetGroups []store.SyncGroupSpec
	for _, group := range groups {
		if MatchRequirements(target, facts, group.Requirements) == nil {
			targetGroups = append(targetGroups, group)
		}
	}
	if len(targetGroups) == 0 {
		return nil, nil
	}
	out := make([]SyncPeer, 0, len(sources))
	for i := range sources {
		source := &sources[i]
		if source.ID == target.ID {
			continue
		}
		shared, limit := false, int64(0)
		for _, group := range targetGroups {
			if MatchRequirements(source, facts, group.Requirements) != nil {
				continue
			}
			shared = true
			if group.RateLimitKiB > 0 && (limit == 0 || group.RateLimitKiB < limit) {
				limit = group.RateLimitKiB
			}
		}
		if shared {
			out = append(out, SyncPeer{NodeID: source.ID, RateLimitKiB: limit})
		}
	}
	return out, nil
}

// loadSelectorFacts loads the facts matching any of reqs against nodes reads.
func loadSelectorFacts(st *store.Store, resolve NodeResolver, nodes []store.Node, reqs []nodelabel.Requirements) NodeFacts {
	merged := nodelabel.Requirements{}
	for _, req := range reqs {
		for key, values := range req {
			merged[key] = append(merged[key], values...)
		}
	}
	return LoadNodeFacts(st, resolve, nodes, merged)
}
