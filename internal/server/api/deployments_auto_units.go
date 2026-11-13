package api

// Automatic placement of a multi-node or PD deployment (placement_mode
// "auto"). The server picks the nodes a request leaves out - a multi-node
// replica's ranks, a PD role's units - from the free cards each node has, in
// the deployment's placement strategy (binpack unless stated): one compute
// network and one GPU architecture per replica or per PD deployment, RDMA
// before Ethernet, nodes with the model first. The request then names its
// nodes and goes through the same checks as one that named them itself. The
// pick is a capacity choice, not a launch plan: when a unit starts, the
// controller plans it and moves what does not fit (controller/unit_auto.go).
