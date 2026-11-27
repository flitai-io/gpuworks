package messages

// Node requirement product error codes: where a deployment may run, decided in
// package planning and passed on by the api layer.
const (
	// ErrorNodeRequirementsInvalid: the requirements a request states cannot
	// be stored ({detail}).
	ErrorNodeRequirementsInvalid = "NODE_REQUIREMENTS_INVALID"
	// ErrorNodeRequirementsUnmet: a node the request names does not satisfy
	// the deployment's requirements ({detail}).
	ErrorNodeRequirementsUnmet = "NODE_REQUIREMENTS_UNMET"
	// ErrorPlacementStrategyInvalid: placement_strategy is not one of the
	// strategies ({strategy}).
	ErrorPlacementStrategyInvalid = "PLACEMENT_STRATEGY_INVALID"
	// ErrorDeploymentNeverFits: no node of the pool ({pool} nodes, {shapes}
	// hardware shapes) could run a replica even with nothing else on it;
	// {node} refused for {reason}. {memory_contract_hint} (true, otherwise
	// absent) says the refusal is the weight-derived memory estimate alone,
	// which a template's placement contract could replace.
	ErrorDeploymentNeverFits = "DEPLOYMENT_NEVER_FITS"
	// ErrorNodeRequirementsNotEditable: the deployment's replicas are not
	// placed from a pool ({shape}: multi_node or pd), so it has no node
	// requirements to edit.
	ErrorNodeRequirementsNotEditable = "NODE_REQUIREMENTS_NOT_EDITABLE"
	// ErrorUnitPlacementNone: no group of nodes can take the deployment now;
	// how many nodes there are ({nodes}) and how many each check left out
	// ({unmet}, {cordoned}, {offline}, {busy}, {no_network}), and the largest
	// group short of the ranks ({largest}).
	ErrorUnitPlacementNone = "UNIT_PLACEMENT_NONE"
	// ErrorAutoPlacementPDMultiNode: an automatic PD request has a role whose
	// units span nodes ({role}); those name their nodes.
	ErrorAutoPlacementPDMultiNode = "AUTO_PLACEMENT_PD_MULTI_NODE"
	// ErrorPlacementGPUNotOnNode: the request names GPU index {gpu_index},
	// which node {node} ({node_gpus} usable GPUs) does not have.
	ErrorPlacementGPUNotOnNode = "PLACEMENT_GPU_NOT_ON_NODE"
)
