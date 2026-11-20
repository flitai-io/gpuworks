package store

import (
	"encoding/json"
	"errors"
	"strings"

	"gorm.io/gorm"
)

var (
	// ErrNetworkNameTaken reports a duplicate network name.
	ErrNetworkNameTaken = errors.New("network name already exists")
	// ErrNetworkAttachmentExists reports a second attachment of one network to
	// the same node or the same label.
	ErrNetworkAttachmentExists = errors.New("network is already attached to this target")
)

// DecodeStringList decodes a JSON list column such as Network.Roles.
func DecodeStringList(raw string) []string {
	var roles []string
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &roles)
	}
	return roles
}

// EncodeStringList encodes a list column; nil and empty both store "[]".
func EncodeStringList(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(values)
	return string(b)
}

// CreateNetwork persists a network whose fields the caller has validated.
func (s *Store) CreateNetwork(n *Network) (*Network, error) {
	if err := s.networkNameFree(n.Name, 0); err != nil {
		return nil, err
	}
	if err := s.db.Create(n).Error; err != nil {
		return nil, err
	}
	s.Bus.PublishChange("network", uintKey(n.ID), "created")
	return n, nil
}

// UpdateNetwork replaces the editable fields of a network.
func (s *Store) UpdateNetwork(n *Network) (*Network, error) {
	if _, err := s.GetNetwork(n.ID); err != nil {
		return nil, err
	}
	if err := s.networkNameFree(n.Name, n.ID); err != nil {
		return nil, err
	}
	updates := map[string]any{
		"name": n.Name, "description": n.Description, "roles": n.Roles, "type": n.Type,
		"cidr": n.CIDR, "interface": n.Interface,
	}
	if err := s.db.Model(&Network{}).Where("id = ?", n.ID).Updates(updates).Error; err != nil {
		return nil, err
	}
	s.Bus.PublishChange("network", uintKey(n.ID), "updated")
	return s.GetNetwork(n.ID)
}

func (s *Store) networkNameFree(name string, self uint) error {
	var count int64
	if err := s.db.Model(&Network{}).Where("lower(name) = lower(?) AND id <> ?", name, self).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return ErrNetworkNameTaken
	}
	return nil
}

// GetNetwork loads a network by id.
func (s *Store) GetNetwork(id uint) (*Network, error) {
	var n Network
	err := s.db.First(&n, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &n, err
}

// ListNetworks returns all networks in creation order.
func (s *Store) ListNetworks() ([]Network, error) {
	var ns []Network
	err := s.db.Order("id").Find(&ns).Error
	return ns, err
}

// DeleteNetwork removes a network and its attachments. Running instances keep
// the network facts they were launched with.
func (s *Store) DeleteNetwork(id uint) error {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		res := tx.Delete(&Network{}, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
		if err := tx.Where("network_id = ?", id).Delete(&NetworkAttachment{}).Error; err != nil {
			return err
		}
		return tx.Where("network_id = ?", id).Delete(&NetworkTestRun{}).Error
	})
	if err != nil {
		return err
	}
	s.Bus.PublishChange("network", uintKey(id), "deleted")
	return nil
}

// ListNetworkAttachments returns every attachment in creation order, which is
// the order the resolver uses to break ties.
func (s *Store) ListNetworkAttachments() ([]NetworkAttachment, error) {
	var as []NetworkAttachment
	err := s.db.Order("id").Find(&as).Error
	return as, err
}

// CreateNetworkAttachment attaches a network to a node or a label.
func (s *Store) CreateNetworkAttachment(a *NetworkAttachment) (*NetworkAttachment, error) {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&Network{}, a.NetworkID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		q := tx.Model(&NetworkAttachment{}).Where("network_id = ?", a.NetworkID)
		if a.NodeID != 0 {
			q = q.Where("node_id = ?", a.NodeID)
		} else {
			q = q.Where("node_id = 0 AND label_key = ? AND label_value = ?", a.LabelKey, a.LabelValue)
		}
		var count int64
		if err := q.Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return ErrNetworkAttachmentExists
		}
		return tx.Create(a).Error
	})
	if err != nil {
		return nil, err
	}
	s.Bus.PublishChange("network", uintKey(a.NetworkID), "attached")
	return a, nil
}

// UpdateNetworkAttachmentLabel changes only the selector, preserving its original
// creation time and ID (the tie-breaker for overlapping attachments).
func (s *Store) UpdateNetworkAttachmentLabel(networkID, id uint, key, value string) error {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var att NetworkAttachment
		if err := tx.Where("network_id = ? AND id = ? AND node_id = 0", networkID, id).First(&att).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		var count int64
		err := tx.Model(&NetworkAttachment{}).Where("network_id = ? AND node_id = 0 AND id <> ? AND label_key = ? AND label_value = ?", networkID, id, key, value).Count(&count).Error
		if err != nil {
			return err
		}
		if count != 0 {
			return ErrNetworkAttachmentExists
		}
		return tx.Model(&att).Updates(map[string]any{"label_key": key, "label_value": value}).Error
	})
	if err == nil {
		s.Bus.PublishChange("network", uintKey(networkID), "attachment_updated")
	}
	return err
}

// UpdateNetworkAttachmentOverrides replaces a node attachment's overrides.
func (s *Store) UpdateNetworkAttachmentOverrides(networkID, id uint, iface, ip string, hcas []string) (*NetworkAttachment, error) {
	res := s.db.Model(&NetworkAttachment{}).Where("id = ? AND network_id = ?", id, networkID).
		Updates(map[string]any{"interface": iface, "ip": ip, "hcas": EncodeStringList(hcas)})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, ErrNotFound
	}
	var a NetworkAttachment
	if err := s.db.First(&a, id).Error; err != nil {
		return nil, err
	}
	s.Bus.PublishChange("network", uintKey(networkID), "attachment_updated")
	return &a, nil
}

// DeleteNetworkAttachment detaches one attachment of a network.
func (s *Store) DeleteNetworkAttachment(networkID, id uint) error {
	res := s.db.Where("id = ? AND network_id = ?", id, networkID).Delete(&NetworkAttachment{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	s.Bus.PublishChange("network", uintKey(networkID), "detached")
	return nil
}

// networkNodeColumns are the node columns network resolution reads.
var networkNodeColumns = []string{"id", "uuid", "name", "hostname", "labels", "network_inventory", "management_roles_off", "token"}

// NodesForNetworks loads the named nodes, or every node when none is named,
// with only the columns network resolution reads, in id order. It is read on
// every gateway route rebuild, so it skips GPUs and credentials. A named node
// that does not exist is ErrNotFound.
func (s *Store) NodesForNetworks(ids ...uint) ([]Node, error) {
	var nodes []Node
	q := s.reader().Select(networkNodeColumns).Order("id")
	if len(ids) > 0 {
		q = q.Where("id IN ?", ids)
	}
	if err := q.Find(&nodes).Error; err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		found := make(map[uint]bool, len(nodes))
		for _, n := range nodes {
			found[n.ID] = true
		}
		for _, id := range ids {
			if !found[id] {
				return nil, ErrNotFound
			}
		}
	}
	return nodes, nil
}

// PinNetworkRails replaces the pinned RDMA rails of several nodes on one
// network in one transaction: all change or none does. A node without a node
// attachment (reached only through a label) gets one holding just its rails,
// which then outranks the label attachment. An empty list un-pins the node.
func (s *Store) PinNetworkRails(networkID uint, rails map[uint][]string) error {
	if len(rails) == 0 {
		return nil
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&Network{}, networkID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		for nodeID, list := range rails {
			var att NetworkAttachment
			err := tx.Where("network_id = ? AND node_id = ?", networkID, nodeID).First(&att).Error
			switch {
			case errors.Is(err, gorm.ErrRecordNotFound):
				if len(list) == 0 {
					continue
				}
				if err := tx.Create(&NetworkAttachment{NetworkID: networkID, NodeID: nodeID, HCAs: EncodeStringList(list)}).Error; err != nil {
					return err
				}
			case err != nil:
				return err
			default:
				if err := tx.Model(&att).Update("hcas", EncodeStringList(list)).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.Bus.PublishChange("network", uintKey(networkID), "attachment_updated")
	return nil
}
