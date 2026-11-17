// Package lifecycle holds what a server's sleep, wake and replica orders
// share: the lock that serializes them, and the nodes an operator named for a
// deployment's next replicas. One Orders serves one server; the controller
// builds it and the app hands it to the API and the automation.
package lifecycle

import "sync"

// Orders is a server's order lock and placement hints. Its zero value is
// ready to use.
type Orders struct {
	// mu serializes sleep and wake orders, whoever gives them. Admission
	// reads the ledger and then writes an intent the ledger reads next, so
	// two wakes checked at once could both fit a card only one of them gets.
	// Replica orders do not take it.
	mu sync.Mutex

	hintMu sync.Mutex
	// hints are the node UUIDs queued for each deployment's next replica
	// starts, by deployment.
	hints map[uint][]string
}

// Lock takes the order lock and returns its release.
func (o *Orders) Lock() (unlock func()) {
	o.mu.Lock()
	return o.mu.Unlock
}

// HintPlacement queues node UUIDs for the deployment's next replica starts,
// replacing what was queued before; no nodes drops the queue. An operator
// scaling a single-node deployment up may name the nodes its new replicas
// start on. The hint covers only their first placement: the controller tries
// the named node first when it next starts a replica, and afterwards the
// replica is an ordinary one, rebuilt and moved like any other. Hints live in
// memory; a server restart before the replicas start leaves them to the
// candidate pool.
func (o *Orders) HintPlacement(deploymentID uint, nodes []string) {
	o.hintMu.Lock()
	defer o.hintMu.Unlock()
	if len(nodes) == 0 {
		delete(o.hints, deploymentID)
		return
	}
	if o.hints == nil {
		o.hints = map[uint][]string{}
	}
	o.hints[deploymentID] = append([]string(nil), nodes...)
}

// NextPlacementHint is the node the deployment's next replica should try
// first, if one is queued.
func (o *Orders) NextPlacementHint(deploymentID uint) (string, bool) {
	o.hintMu.Lock()
	defer o.hintMu.Unlock()
	queued := o.hints[deploymentID]
	if len(queued) == 0 {
		return "", false
	}
	return queued[0], true
}

// ConsumePlacementHint drops the first queued node once a replica start has
// tried it, whether it took the replica or not.
func (o *Orders) ConsumePlacementHint(deploymentID uint, node string) {
	o.hintMu.Lock()
	defer o.hintMu.Unlock()
	queued := o.hints[deploymentID]
	if len(queued) == 0 || queued[0] != node {
		return
	}
	if len(queued) == 1 {
		delete(o.hints, deploymentID)
		return
	}
	o.hints[deploymentID] = queued[1:]
}

// ForgetPlacementHint drops the deployment's queued nodes.
func (o *Orders) ForgetPlacementHint(deploymentID uint) {
	o.HintPlacement(deploymentID, nil)
}
