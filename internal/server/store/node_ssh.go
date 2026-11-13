package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Recovery phases are orthogonal to Node.State. Node.State remains the
// scheduler-facing connection state; these values describe out-of-band work.
//
// The first eight are steps of an attempt in flight; the others describe
// where an attempt ended. RecoveryUnderway is the one reader of that
// division — the recovery loop's own "try this node again" rule is a
// different question and lives with the query that asks it.
const (
	RecoveryPhaseProbing       = "probing"
	RecoveryPhaseArtifactCheck = "artifact_check"
	RecoveryPhaseUploading     = "uploading"
	RecoveryPhaseVerifying     = "verifying"
	RecoveryPhaseReplacing     = "replacing"
	RecoveryPhaseRestarting    = "restarting"
	RecoveryPhaseReinstalling  = "reinstalling"
	RecoveryPhaseReconnecting  = "reconnecting"
	RecoveryPhaseUnreachable   = "unreachable"
	RecoveryPhaseBlocked       = "recovery_blocked"
	RecoveryPhaseStopped       = "stopped"
	RecoveryPhaseProbeFailed   = "probe_failed"
)

// RecoveryUnderway is whether the platform is in the middle of restoring
// this node's agent. The phases an attempt ends in are not underway, even
// the ones the loop will retry later: a node that has been unreachable for
// a day reads as offline, not as forever connecting.
func RecoveryUnderway(phase string) bool {
	switch phase {
	case RecoveryPhaseProbing, RecoveryPhaseArtifactCheck, RecoveryPhaseUploading,
		RecoveryPhaseVerifying, RecoveryPhaseReplacing, RecoveryPhaseRestarting,
		RecoveryPhaseReinstalling, RecoveryPhaseReconnecting:
		return true
	default:
		return false
	}
}

// RecoveryStoppedProviderInstanceGone is the recovery error recorded when a
// node's vendor instance has been released.
const RecoveryStoppedProviderInstanceGone = "provider instance no longer exists"

// NodeSSHCredential stores only the SSH endpoint in clear text. Authentication
// material is an AES-GCM ciphertext encrypted with the server master key.
type NodeSSHCredential struct {
	NodeID      uint   `gorm:"primaryKey"`
	Host        string `gorm:"size:255"`
	Port        int
	User        string `gorm:"size:64"`
	CredsCipher string `gorm:"type:text"` // ciphertext of JSON store.NodeSSHSecrets
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (NodeSSHCredential) TableName() string { return "node_ssh_credential" }

// NodeSSHSecrets is deliberately store-local and never used as an API DTO.
// Both fields empty means the server authenticates with the default keys in
// its own ~/.ssh.
type NodeSSHSecrets struct {
	Password      string `json:"password,omitempty"`
	PrivateKeyPEM string `json:"private_key_pem,omitempty"`
}

func (s NodeSSHSecrets) Empty() bool {
	return s.Password == "" && strings.TrimSpace(s.PrivateKeyPEM) == ""
}

// SetNodeSSHCredential replaces a node's write-only SSH authentication record.
func (s *Store) SetNodeSSHCredential(uuid, host string, port int, user string, secrets NodeSSHSecrets) (*NodeSSHCredential, error) {
	host = strings.TrimSpace(host)
	user = strings.TrimSpace(user)
	if strings.Contains(host, ":") {
		host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
		if net.ParseIP(host) == nil {
			return nil, errors.New("ssh host must not include a port")
		}
	}
	if host == "" || user == "" || port < 1 || port > 65535 {
		return nil, errors.New("ssh host, user, and a valid port are required")
	}
	if s.secretbox == nil {
		return nil, errors.New("ssh credential encryption is not configured")
	}
	node, err := s.NodeByUUID(uuid)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(secrets)
	if err != nil {
		return nil, err
	}
	ciphertext, err := s.secretbox.Encrypt(raw)
	if err != nil {
		return nil, fmt.Errorf("encrypt ssh credential: %w", err)
	}
	row := NodeSSHCredential{NodeID: node.ID, Host: host, Port: port, User: user, CredsCipher: ciphertext}
	var existing NodeSSHCredential
	err = s.db.Where("node_id = ?", node.ID).First(&existing).Error
	switch {
	case err == nil:
		if err := s.db.Model(&existing).Updates(map[string]any{
			"host": host, "port": port, "user": user, "creds_cipher": ciphertext,
		}).Error; err != nil {
			return nil, err
		}
		row.CreatedAt = existing.CreatedAt
		row.UpdatedAt = time.Now().UTC()
	case errors.Is(err, gorm.ErrRecordNotFound):
		if err := s.db.Create(&row).Error; err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	s.publishNode(uuid, "ssh_credential:updated")
	return &row, nil
}

// NodesWithSSHHost returns the nodes whose saved SSH endpoint is host, so the
// access wizard can say a host is already a node here. It runs once per
// precheck or install, not per node or event.
func (s *Store) NodesWithSSHHost(host string) ([]Node, error) {
	host = strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(host), "]"), "[")
	var nodes []Node
	if host == "" {
		return nodes, nil
	}
	err := s.db.Joins("JOIN node_ssh_credential ON node_ssh_credential.node_id = nodes.id").
		Where("node_ssh_credential.host = ?", host).Order("nodes.id").Find(&nodes).Error
	return nodes, err
}

// GetNodeSSHCredential decrypts authentication material for internal recovery
// use. API handlers must map it to a sanitized response instead of returning it.
func (s *Store) GetNodeSSHCredential(uuid string) (*NodeSSHCredential, NodeSSHSecrets, error) {
	var secrets NodeSSHSecrets
	if s.secretbox == nil {
		return nil, secrets, errors.New("ssh credential encryption is not configured")
	}
	node, err := s.NodeByUUID(uuid)
	if err != nil {
		return nil, secrets, err
	}
	var row NodeSSHCredential
	if err := s.db.Where("node_id = ?", node.ID).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, secrets, ErrNotFound
		}
		return nil, secrets, err
	}
	raw, err := s.secretbox.Decrypt(row.CredsCipher)
	if err != nil {
		return nil, secrets, fmt.Errorf("decrypt ssh credential: %w", err)
	}
	if err := json.Unmarshal(raw, &secrets); err != nil {
		return nil, secrets, fmt.Errorf("decode ssh credential: %w", err)
	}
	return &row, secrets, nil
}

func (s *Store) DeleteNodeSSHCredential(uuid string) error {
	node, err := s.NodeByUUID(uuid)
	if err != nil {
		return err
	}
	res := s.db.Where("node_id = ?", node.ID).Delete(&NodeSSHCredential{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	s.publishNode(uuid, "ssh_credential:deleted")
	return nil
}

// HasNodeSSHCredential is a pure read (a presence flag folded onto node reads
// by NodeByUUID/NodeByID/NodeWithGPUs/markNodeCredentialFlags), so it goes
// through the read pool like the rest of those callers' queries — see
// Store.reader.
func (s *Store) HasNodeSSHCredential(nodeID uint) (bool, error) {
	var count int64
	err := s.reader().Model(&NodeSSHCredential{}).Where("node_id = ?", nodeID).Count(&count).Error
	return count > 0, err
}

func (s *Store) markNodeCredentialFlags(nodes []Node) error {
	if len(nodes) == 0 {
		return nil
	}
	ids := make([]uint, 0, len(nodes))
	for i := range nodes {
		ids = append(ids, nodes[i].ID)
	}
	var rows []NodeSSHCredential
	if err := s.reader().Select("node_id").Where("node_id IN ?", ids).Find(&rows).Error; err != nil {
		return err
	}
	has := make(map[uint]bool, len(rows))
	for _, row := range rows {
		has[row.NodeID] = true
	}
	for i := range nodes {
		nodes[i].HasSSHCredential = has[nodes[i].ID]
	}
	return nil
}

// SetNodeRecoveryPhase persists one state-machine transition and emits a node
// event after the update commits.
func (s *Store) SetNodeRecoveryPhase(uuid, phase string, attempts int, lastProbeAt *time.Time, lastErr string) error {
	updates := map[string]any{
		"recovery_phase": phase, "recovery_attempts": attempts,
		"last_probe_at": lastProbeAt, "last_recovery_error": strings.TrimSpace(lastErr),
	}
	res := s.db.Model(&Node{}).Where("uuid = ?", uuid).Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	reason := "recovery:cleared"
	if phase != "" {
		reason = "recovery:" + phase
	}
	s.publishNode(uuid, reason)
	return nil
}

func (s *Store) ClearNodeRecovery(uuid string) error {
	return s.SetNodeRecoveryPhase(uuid, "", 0, nil, "")
}

// NodesNeedingRecovery returns only previously connected offline nodes with a
// configured SSH side channel. Pending and explicitly stopped nodes are absent.
func (s *Store) NodesNeedingRecovery() ([]Node, error) {
	var nodes []Node
	err := s.db.Model(&Node{}).
		Joins("JOIN node_ssh_credential ON node_ssh_credential.node_id = nodes.id").
		// Which nodes are worth another attempt, which is not the same
		// question as RecoveryUnderway: a node left unreachable is tried
		// again, one that is blocked or stopped is not.
		Where("nodes.state = ? AND nodes.last_seen_at IS NOT NULL AND (nodes.recovery_phase IS NULL OR nodes.recovery_phase NOT IN ?)", NodeStateOffline, []string{RecoveryPhaseStopped, RecoveryPhaseBlocked}).
		Order("nodes.id").Find(&nodes).Error
	if err != nil {
		return nil, err
	}
	for i := range nodes {
		nodes[i].HasSSHCredential = true
	}
	return nodes, nil
}

func (s *Store) ProviderInstanceForNode(nodeID uint) (*ProviderInstance, error) {
	var row ProviderInstance
	err := s.db.Where("node_id = ?", nodeID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &row, err
}
