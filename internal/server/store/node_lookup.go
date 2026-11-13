package store

import (
	"time"

	"gorm.io/gorm"
)

// NodesByUUID reads the named nodes as ListNodes reads every node: with their
// GPUs and credential flags, newest first. A UUID with no node is skipped.
func (s *Store) NodesByUUID(uuids []string) ([]Node, error) {
	if len(uuids) == 0 {
		return nil, nil
	}
	var nodes []Node
	err := s.withCards(s.reader(), func(q *gorm.DB) error { return q.Where("uuid IN ?", uuids).Order("id desc").Find(&nodes).Error })
	if err == nil {
		err = s.markNodeCredentialFlags(nodes)
	}
	return nodes, err
}

// CountNodes counts every node, whatever its state.
// CountNodesUnseenSince counts the nodes whose agents have not reported since
// at (or never have): after a server start, the ones still reconnecting.
func (s *Store) CountNodesUnseenSince(at time.Time) (int, error) {
	var n int64
	// Heartbeats are stored in UTC; SQLite compares their timestamp text.
	err := s.reader().Model(&Node{}).Where("last_seen_at IS NULL OR last_seen_at < ?", at.UTC()).Count(&n).Error
	return int(n), err
}

func (s *Store) CountNodes() (int, error) {
	var n int64
	err := s.reader().Model(&Node{}).Count(&n).Error
	return int(n), err
}

// labelNodeColumns are the node columns its labels are derived from
// (planning.SystemLabels and the written labels) plus those network
// resolution reads, for a requirement naming a network.
var labelNodeColumns = append([]string{"gpu_vendor", "framework", "arch"}, networkNodeColumns...)

// NodesForLabels reads every node, with its GPUs when withGPUs, but only the
// columns its labels are derived from, newest first. Counting how many nodes
// carry a requirement's labels needs the whole fleet; reading every column
// and card of it on each deployment create or read cost more than the rest
// of the request. Only the card labels need the GPUs.
func (s *Store) NodesForLabels(withGPUs bool) ([]Node, error) {
	var nodes []Node
	find := func(q *gorm.DB) error { return q.Select(labelNodeColumns).Order("id desc").Find(&nodes).Error }
	if withGPUs {
		return nodes, s.withCards(s.reader(), find)
	}
	return nodes, find(s.reader())
}

// NodeNamesByID is the display name (Node.DisplayName) of the nodes of ids,
// one query of the columns it is made from; an unknown id is absent.
func (s *Store) NodeNamesByID(ids []uint) (map[uint]string, error) {
	refs, err := s.NodeRefsByID(ids)
	if err != nil {
		return nil, err
	}
	out := make(map[uint]string, len(refs))
	for id, ref := range refs {
		out[id] = ref.Name
	}
	return out, nil
}

// NodesForLabelsByID is NodesForLabels for the named nodes only, in id order:
// one query however many there are, for a caller matching a handful of
// candidates against selectors (sync groups, pull source rules). Unknown ids
// are skipped.
func (s *Store) NodesForLabelsByID(ids []uint, withGPUs bool) ([]Node, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var nodes []Node
	find := func(q *gorm.DB) error {
		return q.Select(labelNodeColumns).Where("id IN ?", ids).Order("id").Find(&nodes).Error
	}
	if withGPUs {
		return nodes, s.withCards(s.reader(), find)
	}
	return nodes, find(s.reader())
}

// NodesForAgentUpgrade reads the facts needed for fleet eligibility without loading cards or credentials.
func (s *Store) NodesForAgentUpgrade() ([]Node, error) {
	var nodes []Node
	err := s.reader().Select("uuid", "agent_version", "os", "arch", "connect_mode", "kube_cluster_id").Find(&nodes).Error
	return nodes, err
}
