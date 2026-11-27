package deployment

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// resolveNodes settles the deployment's node and what each target must
// accept: the partition backend, the image platform and sleep host memory.
func (c *createState) resolveNodes() error {
	var err error
	if c.pdReq != nil {
		c.distNodes = c.pdReq.otherNodes()
	}
	if sleepErr := planning.SleepDeclarable(c.req.Sleepable, c.eng.Name(), c.targetVendor); sleepErr != nil {
		return invalid(SleepDeclarationError(sleepErr))
	}
	c.yieldPolicy, err = createYieldPolicy(*c.req, c.eng.Name(), c.pdReq != nil)
	if err != nil {
		return invalid(err)
	}
	if c.req.NNodes > 1 && c.pdReq == nil {
		c.node, err = c.s.d.Store.NodeWithGPUs(c.distNodes[0].UUID)
		if err != nil {
			return invalid(err)
		}
	} else {
		c.node, err = c.s.RequestNode(c.req.NodeUUID)
		if err != nil {
			return invalid(err)
		}
		if err := RequestCardsOnNode(c.req, c.node); err != nil {
			return invalid(err)
		}
	}
	// A refused backend is a ProductError with the capability's reason code
	// (partitioncap.ValidateReady), whose params name the node.
	c.req.PartitionBackend, err = ResolvePartitionBackend(c.node, c.req.VRAMLimitGiB, c.req.PartitionBackend)
	if err != nil {
		return invalid(err)
	}
	for _, distNode := range c.distNodes {
		if _, err := ResolvePartitionBackend(distNode, c.req.VRAMLimitGiB, c.req.PartitionBackend); err != nil {
			return invalid(distributedPartitionRefusal(distNode, c.req.PartitionBackend, err))
		}
	}
	if err := c.s.admitImagePlatforms(c.eng.Name(), c.selection, c.req.Image, append([]*store.Node{c.node}, c.distNodes...)); err != nil {
		return invalid(err)
	}
	c.targets = append([]*store.Node{c.node}, c.distNodes...)
	if err := c.s.sleepHostBacking(c.eng.Name(), c.req, c.quick, c.targets...); err != nil {
		return conflict(err)
	}
	return nil
}

// admitWeights decides where the weights and components are read from or
// saved to, and admits each on its nodes.
func (c *createState) admitWeights() error {
	var err error
	if c.req.NNodes > 1 && c.pdReq == nil {
		c.rankNodes = c.distNodes
	}
	c.weights, err = c.s.weightTargets(c.selection, c.model, c.targets, c.rankNodes)
	if err != nil {
		return internal(err)
	}
	c.weightStorage, err = c.s.resolveSaveToStorage(c.req.ModelSource, c.model, c.weights[0].nodes)
	if err != nil {
		return conflict(err)
	}
	for _, weight := range c.weights[1:] {
		if _, err := c.s.resolveSaveToStorage(c.req.ModelSource, weight.model, weight.nodes); err != nil {
			return conflict(err)
		}
	}
	// What the request names is what the deployment keeps: 0 leaves where a
	// node saves a model it must fetch to the default order.
	c.saveTo = c.s.previewSaveToStorageID(c.req.ModelSource, c.model, c.node)
	c.components, err = c.s.templateComponents(c.selection.Components)
	if err != nil {
		return internal(err)
	}
	admitted := append([]weightTarget(nil), c.weights...)
	for _, component := range c.components {
		nodes := component.nodesOf(c.targets, c.rankNodes)
		if _, err := c.s.resolveSaveToStorage(c.req.ModelSource, component.model, nodes); err != nil {
			return conflict(err)
		}
		admitted = append(admitted, weightTarget{model: component.model, nodes: nodes})
	}
	// A template component no target can read yet follows the primary into
	// the shared storage it already sits on, rather than into every node's
	// local storage; the evaluate forecast shows the same plans. The
	// deployment still keeps only what the request named.
	c.componentPlans, err = c.s.planComponents(c.req.ModelSource, c.model, c.components, c.weightStorage, store.StoragePins(c.reqs), c.targets, c.rankNodes)
	if err != nil {
		return internal(err)
	}
	rules := store.SourceRules{Pinned: store.StoragePins(c.reqs), SaveTo: c.saveTo}
	for _, admitted := range admitted {
		if err := c.s.admitDeploymentModel(c.ctx, admitted.model, rules, admitted.nodes); err != nil {
			return conflict(err)
		}
	}
	return nil
}

// resolveImage settles the runtime image, then asks whether the request can
// ever fit and whether an automatic pick still holds.
func (c *createState) resolveImage() error {
	imageResolution := c.s.rankImage(c.eng.Name(), c.selection, c.req.Image, c.node)
	if strings.TrimSpace(imageResolution.Image) == "" {
		code, params := messages.ErrorAcquisitionImageRequired, map[string]any(nil)
		if len(imageResolution.Warnings) > 0 {
			code, params = imageResolution.Warnings[0].Code, imageResolution.Warnings[0].Params
		}
		return invalid(productError(code, "no runtime image is available for the target node", params))
	}
	c.req.Image = imageResolution.Image
	c.reqs = planning.WithNodeVendor(c.reqs, c.node)
	warnings, err := c.s.checkCreateCanEverFit(c.ctx, c.req, c.pdReq != nil, c.reqs, c.eng, c.model, c.selection, c.placement, c.quick, c.launch.UserResidual, c.modelMount, c.componentMounts)
	if err != nil {
		return invalid(err)
	}
	// An automatically placed create re-runs the plan the evaluation answered
	// with: resources may have moved since, and the answer is the same
	// conflict the client handles for stale labels - not a doomed row.
	if IsAutomaticPlacement(c.req) && c.req.NNodes <= 1 && c.pdReq == nil {
		requestOverrides, _ := RequestContainerOverrides(c.req.ContainerOverrides)
		warnings, err = c.s.revalidateAutomaticPlacement(c.ctx, c.req, c.eng, c.model, c.selection, c.placement, c.quick, c.launch.UserResidual, strings.TrimSpace(c.req.ServedName), c.node, c.modelMount, c.componentMounts, requestOverrides, c.saveTo)
		if err != nil {
			return conflict(err)
		}
	}
	for _, warning := range warnings {
		if warning.Code == messages.WarningDGPUStartupMemory {
			c.warnings = append(c.warnings, warning)
		}
	}
	return nil
}
