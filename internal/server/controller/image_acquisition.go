package controller

import (
	"context"
	"errors"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// imagePeerSyncer is the hub's node-to-node image copy, which the controller
// starts for a node that lacks a deployment's image when a node of its sync
// group has it.
type imagePeerSyncer interface {
	SyncImageFromRate(ctx context.Context, imageID uint, targetNodeUUID, sourceNodeUUID string, rateLimitKiB int64) (*store.ImageNode, error)
}

// peerCooldown is how long a pair of nodes that failed to copy to one another
// is not tried again automatically: a pair that cannot reach each other (two
// cloud containers behind their own NAT) fails fast, and retrying it on every
// launch would only delay the pull that works.
const peerCooldown = 3 * time.Hour

// pairCooldown remembers node pairs whose automatic copy failed, in memory: a
// server restart forgets them, which costs one more failed attempt.
type pairCooldown struct {
	mu    sync.Mutex
	until map[[2]uint]time.Time
}

func (p *pairCooldown) cool(source, target uint, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.until == nil {
		p.until = map[[2]uint]time.Time{}
	}
	p.until[[2]uint{source, target}] = now.Add(peerCooldown)
}

func (p *pairCooldown) cooling(source, target uint, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.until[[2]uint{source, target}]
	if !ok {
		return false
	}
	if now.After(until) {
		delete(p.until, [2]uint{source, target})
		return false
	}
	return true
}

// imageWaits remembers which deployments wait for an image to arrive on a
// node by copy, so an image event re-queues those and not every deployment.
type imageWaits struct {
	mu    sync.Mutex
	byDep map[uint]uint // deployment → image row
}

func (w *imageWaits) set(dep, image uint) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.byDep == nil {
		w.byDep = map[uint]uint{}
	}
	w.byDep[dep] = image
}

func (w *imageWaits) clear(dep uint) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.byDep, dep)
}

func (w *imageWaits) waiting(image uint) []uint {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []uint
	for dep, id := range w.byDep {
		if id == image {
			out = append(out, dep)
		}
	}
	return out
}

// imageDelivery is what decides whether a replica on one node can start: the
// image is there, or is not coming by copy (the instance pulls it itself), or
// is being copied from a node of the same sync group and the launch waits.
type imageDelivery struct {
	present bool
}

// deploymentImageDelivery gets a deployment's image onto node the cheapest
// way: a node of its sync group that has it copies it over; otherwise the
// replica pulls it at start, through the node's pull source rules. Only an
// image the catalog knows can be copied (the ledger that tracks copies is
// keyed by catalog row); any other image is always pulled.
//
// A copy that fails is not retried with that pair for peerCooldown; the
// replica then pulls the image itself. Nothing here is the deployment's
// choice, so nothing is waited for that could not arrive.
func (c *Controller) deploymentImageDelivery(ctx context.Context, dep *store.Deployment, node *store.Node) imageDelivery {
	if dep == nil || node == nil {
		return imageDelivery{present: true}
	}
	image, err := c.store.ResolveImageRef(dep.Image)
	if err != nil {
		c.imageWaits.clear(dep.ID)
		return imageDelivery{present: true}
	}
	now := c.now()
	row, err := c.store.ImageNode(image.ID, node.ID)
	if err == nil {
		switch row.State {
		case store.ImageStatePresent:
			c.imageWaits.clear(dep.ID)
			return imageDelivery{present: true}
		case store.ImageStateSyncing, store.ImageStatePulling:
			if watcher, ok := c.hub.(interface{ WatchImageTransfer(uint, uint) }); ok {
				watcher.WatchImageTransfer(image.ID, node.ID)
			}
			c.imageWaits.set(dep.ID, image.ID)
			c.recordUnschedulable(dep, scheduler.Reason{Code: scheduler.ReasonImageDeliveryWaiting, Params: map[string]any{"node": node.DisplayName(), "image": image.ImageRef}})
			return imageDelivery{}
		case store.ImageStateFailed:
			if row.Source == "peer" && row.TransferErrorCode == messages.ErrorPeerRelayBusy {
				if syncer, ok := c.hub.(imagePeerSyncer); ok {
					if source, e := c.store.NodeByID(row.SourceNodeID); e == nil {
						_, _ = syncer.SyncImageFromRate(ctx, image.ID, node.UUID, source.UUID, row.LastRateLimitKiB)
					}
				}
				c.imageWaits.set(dep.ID, image.ID)
				return imageDelivery{}
			}
			if row.Source == "peer" && row.SourceNodeID != 0 {
				c.peerCool.cool(row.SourceNodeID, node.ID, now)
			}
		}
	}
	source, rate := c.imageCopySource(image.ID, node, now)
	syncer, ok := c.hub.(imagePeerSyncer)
	if source == nil || !ok {
		c.imageWaits.clear(dep.ID)
		return imageDelivery{present: true}
	}
	if _, err := syncer.SyncImageFromRate(ctx, image.ID, node.UUID, source.UUID, rate); err != nil {
		if errors.Is(err, messages.ErrPeerRelayBusy) {
			c.imageWaits.set(dep.ID, image.ID)
			return imageDelivery{}
		}
		c.log.Info("image copy not started; the replica pulls the image itself", "dep", dep.ID, "image", image.ImageRef, "node", node.UUID, "source", source.UUID, "err", err)
		c.peerCool.cool(source.ID, node.ID, now)
		c.imageWaits.clear(dep.ID)
		return imageDelivery{present: true}
	}
	c.imageWaits.set(dep.ID, image.ID)
	c.recordUnschedulable(dep, scheduler.Reason{Code: scheduler.ReasonImageDeliveryWaiting, Params: map[string]any{"node": node.DisplayName(), "image": image.ImageRef}})
	return imageDelivery{}
}

// imageCopySource picks the node to copy an image to target from: an online
// node of target's sync groups that has it, preferring one on the same storage
// sync network, skipping pairs that failed recently.
func (c *Controller) imageCopySource(imageID uint, target *store.Node, now time.Time) (*store.Node, int64) {
	held, err := c.store.ImageHolders(imageID, target.ID)
	if err != nil {
		return nil, 0
	}
	ids := held[:0]
	for _, id := range held {
		if !c.peerCool.cooling(id, target.ID, now) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, 0
	}
	// One read for every holder: an image the whole fleet holds is asked
	// about on each launch of a node that lacks it.
	nodes, err := c.store.NodesForLabelsByID(ids, true)
	if err != nil {
		return nil, 0
	}
	holders := nodes[:0]
	for _, node := range nodes {
		if c.hub.Online(node.UUID) {
			holders = append(holders, node)
		}
	}
	if len(holders) == 0 {
		return nil, 0
	}
	peers, err := planning.SyncPeersFor(c.store, c.networks.Resolve, target, holders)
	if err != nil || len(peers) == 0 {
		return nil, 0
	}
	byID := make(map[uint]*store.Node, len(holders))
	for i := range holders {
		byID[holders[i].ID] = &holders[i]
	}
	best := peers[0]
	if len(peers) > 1 {
		// A peer on the target's own storage sync network goes first; the
		// networks of all of them are resolved in one pass.
		nodes := make([]store.Node, 0, len(peers)+1)
		nodes = append(nodes, *target)
		for _, peer := range peers {
			nodes = append(nodes, *byID[peer.NodeID])
		}
		if resolved, err := c.networks.Resolve(nodes); err == nil {
			if own, ok := resolved.For(target.ID, netplan.RoleStorageSync); ok && !own.IsManagement() {
				for _, peer := range peers {
					if theirs, ok := resolved.For(peer.NodeID, netplan.RoleStorageSync); ok && !theirs.IsManagement() && theirs.Network.ID == own.Network.ID {
						best = peer
						break
					}
				}
			}
		}
	}
	return byID[best.NodeID], best.RateLimitKiB
}
