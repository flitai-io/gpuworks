package planning

import (
	"context"
	"errors"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
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
// storages, each nil when reqs name none. The result is nil when some node
// fits.
//
// A shape whose launch cannot be resolved on its node (LaunchResolveError:
// the cards it gets do not match the stated parallelism, say) cannot run the
// deployment, but it does not veto the pool: the next shape is planned. When
// no shape fits and one of them failed that way, the first such error is the
// answer, as it was before other shapes were tried - it names what to change
// more precisely than a refusal would. A canceled context, a failed build and
// any other planning failure (a store read) stop the check as errors.
func EverFits(ctx context.Context, st *store.Store, networks Networks, nodes []*store.Node, facts NodeFacts, reqs nodelabel.Requirements, build func(*store.Node) (LaunchRequest, deploy.CatalogSelection, error)) (*NeverFits, error) {
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
			return nil, err
		}
		req, layer, err := build(node)
		if err != nil {
			return nil, err
		}
		req.Preview, req.Unloaded = true, true
		_, refusal, err := PlanTemplateLaunch(ctx, st, networks, node, req, layer)
		var resolveErr *LaunchResolveError
		if errors.As(err, &resolveErr) {
			if unresolved == nil {
				unresolved = err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if refusal == nil {
			return nil, nil
		}
		if out.Node == "" {
			out.Node, out.Reason = node.DisplayName(), *refusal
		}
	}
	if unresolved != nil {
		return nil, unresolved
	}
	out.Shapes = len(shapes)
	return out, nil
}
