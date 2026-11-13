package store

import (
	"crypto/hmac"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// ---- providers ----

// CreateProvider persists a provider with pre-encrypted credentials.
func (s *Store) CreateProvider(p *Provider) (*Provider, error) {
	if p.State == "" {
		p.State = "valid"
	}
	if err := s.db.Create(p).Error; err != nil {
		return nil, err
	}
	s.Bus.PublishChange("provider", uintKey(p.ID), "created")
	return p, nil
}

// UpdateProvider updates a provider's display name, encrypted credentials, and
// state while keeping the row identity intact.
func (s *Store) UpdateProvider(id uint, name, credsCipher, state string) (*Provider, error) {
	updates := map[string]any{
		"name":         name,
		"creds_cipher": credsCipher,
	}
	if state != "" {
		updates["state"] = state
	}
	if err := s.db.Model(&Provider{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		return nil, err
	}
	return s.GetProvider(id)
}

// GetProvider loads a provider by id.
func (s *Store) GetProvider(id uint) (*Provider, error) {
	var p Provider
	err := s.db.First(&p, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &p, err
}

// ListProviders returns all providers.
func (s *Store) ListProviders() ([]Provider, error) {
	var ps []Provider
	err := s.db.Order("id desc").Find(&ps).Error
	return ps, err
}

// DeleteProvider removes a provider and its instances.
func (s *Store) DeleteProvider(id uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("provider_id = ?", id).Delete(&ProviderInstance{}).Error; err != nil {
			return err
		}
		if err := tx.Where("provider_id = ?", id).Delete(&ProviderBootstrapToken{}).Error; err != nil {
			return err
		}
		// Clusters made from the account stay, as entries made by hand.
		if err := tx.Model(&KubeCluster{}).Where("provider_id = ?", id).Updates(map[string]any{"provider_id": nil, "provider_cluster_id": ""}).Error; err != nil {
			return err
		}
		return tx.Delete(&Provider{}, id).Error
	})
}

// SetProviderState updates validity.
func (s *Store) SetProviderState(id uint, state string) error {
	return s.db.Model(&Provider{}).Where("id = ?", id).Update("state", state).Error
}

// ---- provider instances ----

// UpsertProviderInstance records/updates a discovered instance.
func (s *Store) UpsertProviderInstance(pi *ProviderInstance) error {
	return upsertProviderInstance(s.db, pi)
}

func upsertProviderInstance(db *gorm.DB, pi *ProviderInstance) error {
	var existing ProviderInstance
	err := db.Where("provider_id = ? AND external_id = ?", pi.ProviderID, pi.ExternalID).First(&existing).Error
	if err == nil {
		pi.ID = existing.ID
		pi.NodeID = existing.NodeID // preserve linkage
		return db.Model(&existing).Select(
			"name", "gpu_model", "gpu_count", "power", "price_per_hr", "region", "public_ip",
			"ssh_port", "ssh_user", "ssh_connections", "kind", "managed_cluster", "updated_at",
		).Updates(pi).Error
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return db.Create(pi).Error
}

// SyncProviderInstances makes a provider's rows match one complete vendor
// listing in a single transaction: listed instances are upserted (keeping any
// node link) and rows for instances absent from the listing are removed, so a
// released instance stops being shown. Callers pass only a listing the vendor
// returned successfully; an empty listing means every instance is gone. Nodes
// linked to removed rows are kept, see removeProviderInstances.
func (s *Store) SyncProviderInstances(providerID uint, listed []ProviderInstance) error {
	var stopped []string
	err := s.db.Transaction(func(tx *gorm.DB) error {
		keep := make([]string, 0, len(listed))
		for i := range listed {
			pi := listed[i]
			pi.ProviderID = providerID
			if err := upsertProviderInstance(tx, &pi); err != nil {
				return fmt.Errorf("persist provider instance %s: %w", pi.ExternalID, err)
			}
			keep = append(keep, pi.ExternalID)
		}
		query := tx.Where("provider_id = ?", providerID)
		if len(keep) > 0 {
			query = query.Where("external_id NOT IN ?", keep)
		}
		var gone []ProviderInstance
		if err := query.Find(&gone).Error; err != nil {
			return err
		}
		var err error
		stopped, err = removeProviderInstances(tx, gone)
		return err
	})
	if err != nil {
		return err
	}
	s.publishRecoveryStopped(stopped)
	return nil
}

// DeleteProviderInstance removes one instance row after the vendor confirmed
// its release. Removing an absent row succeeds, so a retried release does not
// fail on bookkeeping. A linked node is kept, see removeProviderInstances.
func (s *Store) DeleteProviderInstance(providerID uint, externalID string) error {
	var stopped []string
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var gone []ProviderInstance
		if err := tx.Where("provider_id = ? AND external_id = ?", providerID, externalID).Find(&gone).Error; err != nil {
			return err
		}
		var err error
		stopped, err = removeProviderInstances(tx, gone)
		return err
	})
	if err != nil {
		return err
	}
	s.publishRecoveryStopped(stopped)
	s.Bus.PublishChange("provider", uintKey(providerID), "instance_deleted")
	return nil
}

// removeProviderInstances deletes rows whose vendor instance no longer exists.
// The linked gpuworks nodes stay (their deployments and history are the
// operator's to clean up), but SSH recovery for them is stopped: the row was
// the only record tying a node to its vendor instance, and the recovery loop
// reads it to learn that the machine is gone. It returns the stopped nodes'
// UUIDs so the caller can publish after commit.
func removeProviderInstances(tx *gorm.DB, gone []ProviderInstance) ([]string, error) {
	if len(gone) == 0 {
		return nil, nil
	}
	ids := make([]uint, 0, len(gone))
	nodeIDs := make([]uint, 0, len(gone))
	for _, pi := range gone {
		ids = append(ids, pi.ID)
		if pi.NodeID != 0 {
			nodeIDs = append(nodeIDs, pi.NodeID)
		}
	}
	if err := tx.Where("id IN ?", ids).Delete(&ProviderInstance{}).Error; err != nil {
		return nil, err
	}
	if len(nodeIDs) == 0 {
		return nil, nil
	}
	var nodes []Node
	if err := tx.Select("id", "uuid").Where("id IN ?", nodeIDs).Find(&nodes).Error; err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, nil
	}
	if err := tx.Model(&Node{}).Where("id IN ?", nodeIDs).Updates(map[string]any{
		"recovery_phase": RecoveryPhaseStopped, "last_recovery_error": RecoveryStoppedProviderInstanceGone,
	}).Error; err != nil {
		return nil, err
	}
	uuids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		uuids = append(uuids, n.UUID)
	}
	return uuids, nil
}

func (s *Store) publishRecoveryStopped(uuids []string) {
	for _, uuid := range uuids {
		s.publishNode(uuid, "recovery:"+RecoveryPhaseStopped)
	}
}

// ProviderInstances lists a provider's instances.
func (s *Store) ProviderInstances(providerID uint) ([]ProviderInstance, error) {
	var pis []ProviderInstance
	err := s.db.Where("provider_id = ?", providerID).Order("id asc").Find(&pis).Error
	return pis, err
}

// ProviderInstanceByExternalID is one stored instance of a provider, or nil
// when the last sync did not list it.
func (s *Store) ProviderInstanceByExternalID(providerID uint, externalID string) (*ProviderInstance, error) {
	var pi ProviderInstance
	err := s.db.Where("provider_id = ? AND external_id = ?", providerID, externalID).First(&pi).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pi, nil
}

// LinkProviderInstanceNode ties a provider instance to a gpuworks node.
func (s *Store) LinkProviderInstanceNode(providerID uint, externalID string, nodeID uint) error {
	return s.db.Model(&ProviderInstance{}).
		Where("provider_id = ? AND external_id = ?", providerID, externalID).
		Update("node_id", nodeID).Error
}

// CreateProviderBootstrapToken is deprecated. It is retained for old database
// tooling and must not be used for new agent installation flows.
func (s *Store) CreateProviderBootstrapToken(providerID uint, externalID, token string, ttl time.Duration) (*ProviderBootstrapToken, error) {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	tok := &ProviderBootstrapToken{
		TokenHash:  HashToken(token),
		ProviderID: providerID,
		ExternalID: externalID,
		ExpiresAt:  time.Now().UTC().Add(ttl),
	}
	if err := s.db.Create(tok).Error; err != nil {
		return nil, err
	}
	return tok, nil
}

// TakeProviderBootstrapToken is deprecated and is no longer part of hub auth.
func (s *Store) TakeProviderBootstrapToken(token string) (*ProviderBootstrapToken, error) {
	now := time.Now().UTC()
	var out ProviderBootstrapToken
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var tok ProviderBootstrapToken
		err := tx.Where("token_hash = ? AND used_at IS NULL AND expires_at > ?", HashToken(token), now).First(&tok).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !hmac.Equal([]byte(tok.TokenHash), []byte(HashToken(token))) {
			return ErrNotFound
		}
		if err := tx.Model(&tok).Update("used_at", now).Error; err != nil {
			return err
		}
		tok.UsedAt = &now
		out = tok
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SetProviderBootstrapTokenNode is deprecated legacy bookkeeping.
func (s *Store) SetProviderBootstrapTokenNode(tokenID, nodeID uint) error {
	return s.db.Model(&ProviderBootstrapToken{}).Where("id = ?", tokenID).Update("node_id", nodeID).Error
}

// ---- settings ----

// GetSetting returns a setting's JSON value ("" if absent).
func (s *Store) GetSetting(key SettingKey) string {
	value, _ := s.LookupSetting(key)
	return value
}

// LookupSetting distinguishes an absent setting from a storage failure. Network
// policy readers must not interpret a failed lookup as permission to go direct.
func (s *Store) LookupSetting(key SettingKey) (string, error) {
	var st Setting
	err := s.db.First(&st, "key = ?", string(key)).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return st.Value, nil
}

// SetSetting upserts a setting.
func (s *Store) SetSetting(key SettingKey, value string) error {
	st := Setting{Key: string(key), Value: value, UpdatedAt: time.Now().UTC()}
	err := s.db.Save(&st).Error
	if err == nil {
		s.Bus.PublishChange("setting", string(key), "updated")
	}
	return err
}

// DeleteSetting removes a setting. Deleting an absent key succeeds, so callers
// can clear a value without first checking whether it was ever set.
func (s *Store) DeleteSetting(key SettingKey) error {
	if err := s.db.Where("key = ?", string(key)).Delete(&Setting{}).Error; err != nil {
		return err
	}
	s.Bus.PublishChange("setting", string(key), "deleted")
	return nil
}

// AllSettings returns every setting as a map.
func (s *Store) AllSettings() (map[string]string, error) {
	var all []Setting
	if err := s.db.Find(&all).Error; err != nil {
		return nil, err
	}
	m := make(map[string]string, len(all))
	for _, st := range all {
		m[st.Key] = st.Value
	}
	return m, nil
}

// NodeProvider is where a node rented through a cloud account runs: the
// provider kind and the region the provider reported for the instance.
type NodeProvider struct {
	Kind   string
	Region string
}

// NodeProviders says, for each of nodeIDs that was rented through a provider
// account, where it runs. Nodes added by hand are absent.
func (s *Store) NodeProviders(nodeIDs []uint) (map[uint]NodeProvider, error) {
	out := map[uint]NodeProvider{}
	if len(nodeIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		NodeID uint
		Kind   string
		Region string
	}
	err := s.reader().Table("provider_instances").
		Select("provider_instances.node_id AS node_id, providers.kind AS kind, provider_instances.region AS region").
		Joins("JOIN providers ON providers.id = provider_instances.provider_id").
		Where("provider_instances.node_id IN ?", nodeIDs).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.NodeID] = NodeProvider{Kind: strings.TrimSpace(row.Kind), Region: strings.TrimSpace(row.Region)}
	}
	return out, nil
}
