package store

import (
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// holdColumns are the node columns a hold and its release decide on.
var holdColumns = append(append([]string(nil), holdFactColumns...),
	"scheduling", "scheduling_by", "runtime_gpu_blocked", "kube_unschedulable")

// ApplySchedulingHolds asks the registered holders about every node now,
// rather than at each node's next status report: a node a holder holds is
// cordoned under its source, and a node cordoned under a holder's source
// that no holder holds any more is released to the platform's cordon while
// the platform has a reason, otherwise to schedulable. Operator cordons,
// drains and the platform's own cordons are left to their owners. It is one
// writer transaction over the fleet, for a holder whose answers changed (at
// boot, or when what it decides on changes), never per report; each changed
// node is published as a scheduling change. It returns the nodes it held
// and those it released. A holder that knows which nodes its answers
// changed for uses ApplySchedulingHoldsFor instead.
//
// The same pass asks the card holders about every card of those nodes
// (card_hold.go) and writes each card whose answer changed; a node with
// such a card is published as a scheduling change too, but is not counted
// in held or released, which are about nodes.
func (s *Store) ApplySchedulingHolds() (held, released []string, err error) {
	return s.applySchedulingHolds(nil)
}

// ApplySchedulingHoldsFor is ApplySchedulingHolds over the nodes with the
// given ids only (ids that no longer exist are skipped): the pass for a
// holder whose answers changed for a few nodes, which reads and writes just
// those rather than the fleet. With no ids it does nothing.
func (s *Store) ApplySchedulingHoldsFor(ids []uint) (held, released []string, err error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}
	return s.applySchedulingHolds(ids)
}

// applySchedulingHolds is a pass over the nodes in ids, or every node when
// ids is nil.
func (s *Store) applySchedulingHolds(ids []uint) (held, released []string, err error) {
	var cardsChanged []string
	err = s.db.Transaction(func(tx *gorm.DB) error {
		held, released = nil, nil
		var err error
		if cardsChanged, err = s.applyCardHoldsTx(tx, ids); err != nil {
			return err
		}
		var nodes []Node
		q := tx.Select(holdColumns)
		if ids != nil {
			q = q.Where("id IN ?", ids)
		}
		if err := q.Order("id").Find(&nodes).Error; err != nil {
			return err
		}
		for i := range nodes {
			n := &nodes[i]
			hold, _ := s.admission.holdOf(nodeHoldFacts(n))
			if hold == SchedulingBySelf && !heldByHolder(n) {
				continue
			}
			changed, err := s.applyHoldTx(tx, n, hold)
			if err != nil {
				return err
			}
			switch {
			case changed && hold != SchedulingBySelf:
				held = append(held, n.UUID)
			case changed:
				released = append(released, n.UUID)
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	s.publishSchedulingChanges(held, released, cardsOnly(cardsChanged, held, released))
	return held, released, nil
}

// cardsOnly is the nodes in cards that are in neither held nor released,
// so a node is published once.
func cardsOnly(cards []string, held, released []string) []string {
	var out []string
	for _, uuid := range cards {
		if !stringIn(uuid, held) && !stringIn(uuid, released) {
			out = append(out, uuid)
		}
	}
	return out
}

func stringIn(v string, list []string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// ReleaseUnownedHolds releases every node cordoned under a holder source
// that is neither a registered holder's nor one of known, as
// ApplySchedulingHolds releases a hold that ended. A server that no longer
// runs the extension which held them (another build, the extension
// removed) calls it at boot, so its cordons do not outlive it. known names
// sources whose holder is expected but not registered yet. It returns the
// released nodes.
//
// Cards marked by such a source are released the same way; a node with
// such a card is published as a scheduling change, not counted in released.
func (s *Store) ReleaseUnownedHolds(known ...SchedulingSource) (released []string, err error) {
	var cardsReleased []string
	err = s.db.Transaction(func(tx *gorm.DB) error {
		released = nil
		var err error
		if cardsReleased, err = s.releaseUnownedCardHoldsTx(tx, known); err != nil {
			return err
		}
		var nodes []Node
		if err := tx.Select(holdColumns).
			Where("scheduling = ? AND scheduling_by NOT IN ?", NodeCordoned,
				[]SchedulingSource{SchedulingBySelf, SchedulingByOperator, SchedulingByPlatform}).
			Order("id").Find(&nodes).Error; err != nil {
			return err
		}
		for i := range nodes {
			n := &nodes[i]
			if s.admission.registeredSource(n.SchedulingBy) || sourceIn(n.SchedulingBy, known) {
				continue
			}
			changed, err := s.applyHoldTx(tx, n, SchedulingBySelf)
			if err != nil {
				return err
			}
			if changed {
				released = append(released, n.UUID)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.publishSchedulingChanges(released, cardsOnly(cardsReleased, nil, released))
	return released, nil
}

// applyHoldTx writes what decideScheduling makes of n under hold. Only a
// release needs the platform's reason, which reads the node's cards.
func (s *Store) applyHoldTx(tx *gorm.DB, n *Node, hold SchedulingSource) (bool, error) {
	reason := ""
	if hold == SchedulingBySelf {
		var cards []GPU
		if err := tx.Select("ownership", "health_code").Where("node_id = ?", n.ID).Find(&cards).Error; err != nil {
			return false, err
		}
		infos := make([]proto.GPUInfo, 0, len(cards))
		for _, c := range cards {
			infos = append(infos, proto.GPUInfo{Ownership: c.Ownership, HealthCode: c.HealthCode})
		}
		reason = PlatformCordonReason(n.RuntimeGPUBlocked, n.KubeUnschedulable, n.SlurmUnavailable, infos)
	}
	d := decideScheduling(n.Scheduling, n.SchedulingBy, reason, false, hold)
	if !d.changed {
		return false, nil
	}
	err := tx.Model(&Node{}).Where("id = ?", n.ID).
		Updates(map[string]any{"scheduling": d.scheduling, "scheduling_by": d.source}).Error
	return err == nil, err
}

func (s *Store) publishSchedulingChanges(lists ...[]string) {
	for _, uuids := range lists {
		for _, uuid := range uuids {
			s.publishNode(uuid, NodeChangeScheduling)
		}
	}
}

// heldByHolder reports whether n is cordoned under a holder's source.
func heldByHolder(n *Node) bool {
	return n.Scheduling == NodeCordoned && isHolderSource(n.SchedulingBy)
}

func sourceIn(src SchedulingSource, list []SchedulingSource) bool {
	for _, s := range list {
		if s == src {
			return true
		}
	}
	return false
}
