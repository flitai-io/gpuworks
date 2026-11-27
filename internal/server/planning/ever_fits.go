package planning

import (
	"context"
	"errors"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// NeverFits says why no node of a pool could ever run a replica: how many
// nodes the requirements admit, how many hardware shapes they come in, and
// the first shape's refusal on the node that stood for it. Node is empty when
// the requirements admit no node at all.
type NeverFits struct {
	Pool   int
	Shapes int
	Node   string
	Reason scheduler.Reason
}

// EverFits checks whether any node reqs admit - online or not - could run one
// replica with nothing else on it. It plans one node of each hardware shape
// (HardwareShape) until one fits, so a pool of a thousand nodes of three
// kinds costs three preview plans. build is the launch for a node; it is
// planned as an unloaded preview (Input.Unloaded) with the template's
// hardware contract (PlanTemplateLaunch). facts are the nodes' networks and
// storages, each nil when reqs name none. When a shape fits, the result carries that plan’s warnings.
//
// A shape whose launch cannot be resolved on its node (LaunchResolveError:
// the cards it gets do not match the stated parallelism, say) cannot run the
// deployment, but it does not veto the pool: the next shape is planned. When
// no shape fits and one of them failed that way, the first such error is the
// answer, as it was before other shapes were tried - it names what to change
// more precisely than a refusal would. A canceled context, a failed build and
// any other planning failure (a store read) stop the check as errors.
func EverFits(ctx context.Context, st *store.Store, networks Networks, nodes []*store.Node, facts NodeFacts, reqs nodelabel.Requirements, build func(*store.Node) (LaunchRequest, deploy.CatalogSelection, error)) ([]messages.Warning, *NeverFits, error) {
	shapes := map[string]bool{}
	out := &NeverFits{}
	var unresolved error
	for _, node := range nodes {
		if MatchRequirements(node, facts, reqs) != nil {
			continue
		}
		out.Pool++
		shape := HardwareShape(node, networks.CanServe(*node))
		if shapes[shape] {
			continue
		}
		shapes[shape] = true
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		req, layer, err := build(node)
		if err != nil {
			return nil, nil, err
		}
		plan, refusal, err := PlanTemplateLaunch(ctx, st, networks, node, req.UnloadedPreview(), layer)
		var resolveErr *LaunchResolveError
		if errors.As(err, &resolveErr) {
			if unresolved == nil {
				unresolved = err
			}
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if refusal == nil {
			return plan.Output.Warnings, nil, nil
		}
		if out.Node == "" {
			out.Node, out.Reason = node.DisplayName(), *refusal
		}
	}
	if unresolved != nil {
		return nil, nil, unresolved
	}
	out.Shapes = len(shapes)
	return nil, out, nil
}

// UnloadedRefusal reads the unloaded plan of one node (req.UnloadedPreview
// through PlanTemplateLaunch, whose three results it takes) as why that node
// could not run the launch even with nothing else on it, nil when it could:
// the answer NeverFitting takes for a node. When the node's cards cannot hold
// the launch, that is the answer, with the scheduler's figures. The launch of
// such a plan is still assembled, on a card count nobody chose
// (previewPlacement takes one card when the request states no degree), and a
// command that states its own degree then fails to resolve against that
// count; EverFits reports that error for the pool, but said of one node it
// would blame a count the node was never asked for. A launch that cannot be
// resolved on cards the node does give is returned as the error.
func UnloadedRefusal(node *store.Node, req LaunchRequest, plan LaunchPlan, refusal *scheduler.Reason, err error) (*scheduler.Reason, error) {
	var resolveErr *LaunchResolveError
	if errors.As(err, &resolveErr) && plan.Input.Node != nil && !plan.Output.Fits() {
		reason := plan.Output.Refusal(node, yieldpolicy.Policy{Yield: req.YieldPolicy})
		return &reason, nil
	}
	return refusal, err
}

// UnloadedPreview is r planned as EverFits plans it: a preview on the node
// with nothing else on it (Input.Unloaded), so only what the node is, never
// what currently runs on it, can refuse.
func (r LaunchRequest) UnloadedPreview() LaunchRequest {
	r.Preview, r.Unloaded = true, true
	return r
}

// NeverFittingBudget is the most unloaded plans one NeverFitting answer costs.
// A fleet with more kinds of node than this leaves the rest unjudged (on
// offer), so an answer asked on every change of a form stays bounded however
// large or varied the fleet is.
const NeverFittingBudget = 16

// NeverFitting answers which of nodes could not run a replica even with
// nothing else on them, each with the refusal a create naming it would be
// given, keyed by node UUID. refusal is a node's unloaded plan
// (UnloadedPreview through PlanTemplateLaunch): nil when it fits.
//
// Nodes are judged by kind: one node of each hardware shape (HardwareShape)
// that also agrees in own is asked, and its answer is every such node's. own
// names whatever else of a node an unloaded plan reads - the storages that
// apply to it and their cache caps - so two nodes that differ in it never
// share an answer, whatever order they come in; nil when nothing else is
// read. At most NeverFittingBudget kinds are asked; the nodes of the others
// are not in the answer.
//
// A node whose launch cannot be resolved (LaunchResolveError: a command that
// needs more cards than the node has, say) cannot run the deployment either,
// and is answered as a rejected placement carrying that error without the
// planner's own prefix - but only when some other kind fits: when none does,
// the error may be the request's own, which its own check reports, and it
// must not turn every node down. Any other failure to plan a kind leaves its
// nodes unjudged and is returned as failed, once, beside the answer for the
// rest; a canceled context ends the answer. fits reports that some node asked
// could run the replica.
func NeverFitting(ctx context.Context, networks Networks, nodes []*store.Node, own func(*store.Node) string, refusal func(*store.Node) (*scheduler.Reason, error)) (never map[string]scheduler.Reason, fits bool, failed error) {
	type verdict struct {
		refused    *scheduler.Reason
		unresolved bool
	}
	kinds := map[string]*verdict{}
	kindOf := make(map[string]string, len(nodes))
	asked := 0
	for _, node := range nodes {
		kind := HardwareShape(node, networks.CanServe(*node))
		if own != nil {
			kind += "\x00" + own(node)
		}
		kindOf[node.UUID] = kind
		if _, answered := kinds[kind]; answered {
			continue
		}
		if asked == NeverFittingBudget {
			kinds[kind] = nil
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		asked++
		refused, err := refusal(node)
		var resolveErr *LaunchResolveError
		switch {
		case errors.As(err, &resolveErr):
			kinds[kind] = &verdict{refused: &scheduler.Reason{Code: scheduler.ReasonPlacementRejected, Params: map[string]any{"detail": resolveErr.Err.Error()}}, unresolved: true}
		case err != nil:
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, false, ctxErr
			}
			kinds[kind] = nil
			if failed == nil {
				failed = err
			}
		default:
			fits = fits || refused == nil
			kinds[kind] = &verdict{refused: refused}
		}
	}
	never = map[string]scheduler.Reason{}
	for _, node := range nodes {
		if v := kinds[kindOf[node.UUID]]; v != nil && v.refused != nil && (fits || !v.unresolved) {
			never[node.UUID] = *v.refused
		}
	}
	return never, fits, failed
}
