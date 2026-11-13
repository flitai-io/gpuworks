// Package store owns all persistence: GORM entities, migrations, and the
// repository methods that write them. Per the project rules, event publication
// happens here (after the transaction commits), never via ORM hooks, and DTOs
// live in the api layer — never leak an entity past this package's methods.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

// Store wraps the database handle and the event bus.
type Store struct {
	db  *gorm.DB
	Bus *bus.Bus
	// sharing keeps SharingDeployments' rows between deployment changes.
	sharing sharingCache
	// readDB is a second connection pool (same database file) dedicated to
	// hot, pure-read paths (ListNodes, ListDeployments, ListInstances, the
	// timeline reads, InstanceByUUID — see reader()): under WAL, readers do
	// not block the writer or each other, but sharing db's single connection
	// (SetMaxOpenConns(1), the one SQLite allows a writer) forced every read
	// to queue behind whatever write transaction was in flight — at fleet
	// scale (hundreds of status-update writes/sec) that queuing was measured
	// as the dominant cost of a browser's list-page poll (scale-review.md §3).
	//
	// Nil for an in-memory database (see Open) and on a Store value built by
	// hand outside Open (a handful of test fixtures); reader() falls back to
	// db in both cases. In-memory is deliberately excluded: it is SQLite's
	// shared-cache mode, not WAL (WAL needs a file to put the log in), and
	// shared-cache's own cross-connection locking can hand a second
	// connection SQLITE_LOCKED against an in-flight writer transaction —
	// unlike SQLITE_BUSY, busy_timeout does not retry that, and this driver
	// does not implement sqlite3_unlock_notify, so it surfaces as an
	// immediate query error instead of a wait. That surfaced in practice as
	// spurious "no running instance" 503s from a real integration harness
	// (all of which run in-memory): a second pool query and the writer's
	// commit collided mid-flight and the pool's read simply failed, which
	// several read call sites (proxy.selectDeploymentInstances among them)
	// treat the same as "not found yet" because they discard the error.
	readDB    *gorm.DB
	secretbox *auth.Secretbox
	// apiKeys remembers verified gateway keys so /v1 does not pay argon2id
	// and a last_used write on every request (apikey_cache.go).
	apiKeys apiKeyCache
	// platformTokens remembers verified platform tokens and spaces out their
	// last_used_at writes (platform_tokens.go).
	platformTokens platformTokenCache
	// weightRepos caches the catalog's repository index
	// (inference_catalog_index.go).
	weightRepos weightRepoCache
	// pullCache holds the pull source rules and sync groups between writes
	// (pull_settings.go).
	pullCache pullSettingsCache
	// cacheLifecycle bridges model readiness checks and persisted instance
	// references with cache deletion on the same node.
	cacheLifecycle cacheLifecycleGuard
	// observer receives the events the scheduling activity counts
	// (scheduling_activity.go).
	observer atomic.Pointer[EventObserver]
	// eventRetentionBatchHook, when set, is called after each batch of
	// events the retention trim deletes, with the write connection free
	// again (event_retention.go). Same-package tests set it; nil in
	// production.
	eventRetentionBatchHook func(deleted int64)
}

func (s *Store) SetSecretbox(box *auth.Secretbox) { s.secretbox = box }

// reader returns the read-only connection pool for hot, pure-read query
// paths, falling back to the writer connection when a Store was constructed
// without one (a handful of test fixtures build &Store{db: ...} directly) or
// when the database is in-memory (see the readDB field doc).
func (s *Store) reader() *gorm.DB {
	if s.readDB != nil {
		return s.readDB
	}
	return s.db
}

// isInMemoryDSN reports whether dsn (in any form Open accepts) names an
// in-memory database rather than a real file: the empty string, literal
// ":memory:", or a file: URI naming :memory: or mode=memory. Ordinary
// disk filenames merely containing these words remain on disk. A second connection
// pool is never opened for these — see the readDB field doc for why.
func isInMemoryDSN(dsn string) bool {
	path, err := DatabaseFilePath(dsn)
	return err == nil && path == ""
}

// ---- token helpers ----

// HashToken returns the sha256 hex of a token for storage/comparison.
func HashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

var ErrNotFound = errors.New("not found")
var ErrReferenced = errors.New("resource is referenced")

// ---- node queries ----

// NodeByUUID loads a node (without GPUs) by its UUID.
func (s *Store) NodeByUUID(uuid string) (*Node, error) {
	var n Node
	err := s.db.Where("uuid = ?", uuid).First(&n).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err == nil {
		n.HasSSHCredential, err = s.HasNodeSSHCredential(n.ID)
	}
	return &n, err
}

// NodeByTokenHash is the primary lookup used by the agent handshake.
func (s *Store) NodeByTokenHash(hash string) (*Node, error) {
	var n Node
	err := s.db.Where("token_hash = ?", strings.TrimSpace(hash)).First(&n).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &n, err
}

// ListNodes returns all nodes with their GPUs preloaded, newest first.
func (s *Store) ListNodes() ([]Node, error) {
	var nodes []Node
	err := s.withCards(s.reader(), func(q *gorm.DB) error { return q.Order("id desc").Find(&nodes).Error })
	if err == nil {
		err = s.markNodeCredentialFlags(nodes)
	}
	return nodes, err
}

// ServerDialNodes returns nodes whose listening endpoint can be redialed by
// the control plane after a restart or transient connection loss.
func (s *Store) ServerDialNodes() ([]Node, error) {
	var nodes []Node
	err := s.db.Where("connect_mode = ? AND server_dial_address <> '' AND state IN (?, ?)", "server_dial", NodeStateConnecting, NodeStateOffline).Order("id").Find(&nodes).Error
	return nodes, err
}

// NodeByID loads a node with GPUs by primary key.
func (s *Store) NodeByID(id uint) (*Node, error) {
	var n Node
	err := s.withCards(s.db, func(q *gorm.DB) error { return q.First(&n, id).Error })
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err == nil {
		n.HasSSHCredential, err = s.HasNodeSSHCredential(n.ID)
	}
	return &n, err
}

// NodeWithGPUs loads a single node with GPUs by UUID.
func (s *Store) NodeWithGPUs(uuid string) (*Node, error) {
	var n Node
	err := s.withCards(s.db, func(q *gorm.DB) error { return q.Where("uuid = ?", uuid).First(&n).Error })
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err == nil {
		n.HasSSHCredential, err = s.HasNodeSSHCredential(n.ID)
	}
	return &n, err
}

// ---- node writes (event-publishing repository methods) ----

// NodeLabels decodes a node's JSON labels map. Empty labels are returned as an
// empty map so callers can update the result and persist it directly.
func NodeLabels(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]string{}, nil
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(raw), &labels); err != nil {
		return nil, fmt.Errorf("decode node labels: %w", err)
	}
	if labels == nil {
		labels = map[string]string{}
	}
	return labels, nil
}

// RegisterNode creates a node during bootstrap and stores the token hash.
// Returns the created node. Publishes a node.changed event after commit.
// legacyTokenPrefix marks a Token column that holds no plaintext token: rows
// registered with only a token hash.
const legacyTokenPrefix = "legacy-"

// PlaintextToken is the node's token as its agent holds it, false for a row
// registered with only a hash, whose Token column is a placeholder.
func (n Node) PlaintextToken() (string, bool) {
	token := strings.TrimSpace(n.Token)
	if token == "" || strings.HasPrefix(token, legacyTokenPrefix) {
		return "", false
	}
	return token, true
}

func (s *Store) RegisterNode(uuid, tokenHash, hostname string, connectModeOpt ...string) (*Node, error) {
	now := time.Now().UTC()
	connectMode := "agent_dial"
	if len(connectModeOpt) > 0 && connectModeOpt[0] != "" {
		connectMode = connectModeOpt[0]
	}
	n := &Node{
		UUID: uuid,
		// Legacy/provider callers only supplied a hash. Keep the new plaintext
		// column non-empty and unique for those rows; token authentication still
		// uses TokenHash until those nodes are re-enrolled.
		Token:       legacyTokenPrefix + uuid,
		TokenHash:   tokenHash,
		State:       NodeStateConnecting,
		Hostname:    hostname,
		ConnectMode: connectMode,
		LastSeenAt:  &now,
	}
	if err := s.db.Create(n).Error; err != nil {
		return nil, err
	}
	s.publishNode(n.UUID, "registered")
	return n, nil
}

// CreateNodePending creates a node before an agent has connected and returns
// the one-time generated display token. The token is also kept in plaintext by
// design so an administrator can copy it again from the management UI.
func (s *Store) CreateNodePending(name string, connectModeOpt ...string) (*Node, string, error) {
	name = strings.TrimSpace(name)
	if len(name) > 255 {
		return nil, "", errors.New("node name is too long")
	}
	connectMode := "agent_dial"
	if len(connectModeOpt) > 0 && strings.TrimSpace(connectModeOpt[0]) == "server_dial" {
		connectMode = "server_dial"
	}
	token := auth.NewToken(24)
	if token == "" {
		return nil, "", errors.New("generate node token")
	}
	n := &Node{
		UUID:        auth.NewUUID(),
		Name:        name,
		Token:       token,
		TokenHash:   HashToken(token),
		State:       NodeStateConnecting,
		ConnectMode: connectMode,
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		return tx.Create(n).Error
	}); err != nil {
		return nil, "", err
	}
	s.publishNode(n.UUID, "pending")
	return n, token, nil
}

// SetNodeAgentInfo records values observed during a successful handshake.
func (s *Store) SetNodeAgentInfo(uuid, version, remoteAddr string, connectModeOpt ...string) error {
	updates := map[string]any{
		"agent_version": strings.TrimSpace(version), "last_remote_addr": strings.TrimSpace(remoteAddr),
		"last_reject_reason": "", "last_reject_at": nil,
	}
	if len(connectModeOpt) > 0 {
		mode := "agent_dial"
		if strings.TrimSpace(connectModeOpt[0]) == "server_dial" {
			mode = "server_dial"
		}
		updates["connect_mode"] = mode
	}
	res := s.db.Model(&Node{}).Where("uuid = ?", uuid).Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	s.publishNode(uuid, "agent_info")
	return nil
}

// SetNodeRuntimePreference persists the user's runtime policy. An empty value
// is normalized to auto so older nodes can opt back into detection.
func (s *Store) SetNodeRuntimePreference(uuid, preference string) error {
	preference = strings.ToLower(strings.TrimSpace(preference))
	if preference == "" {
		preference = proto.RuntimePreferenceAuto
	}
	if !proto.ValidRuntimePreference(preference) {
		return fmt.Errorf("invalid runtime preference %q", preference)
	}
	result := s.db.Model(&Node{}).Where("uuid = ?", uuid).Update("runtime_preference", preference)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	s.publishNode(uuid, "runtime_preference")
	return nil
}

// SetNodeReject records the latest authentication/protocol rejection.
func (s *Store) SetNodeReject(uuid, reason string) error {
	now := time.Now().UTC()
	res := s.db.Model(&Node{}).Where("uuid = ?", uuid).Updates(map[string]any{
		"last_reject_reason": strings.TrimSpace(reason), "last_reject_at": now,
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	s.publishNode(uuid, "reject:"+strings.TrimSpace(reason))
	return nil
}

// RenameNode changes only the user-managed display name; hostname remains an
// independent observation from the agent.
func (s *Store) RenameNode(uuid, name string) (*Node, error) {
	name = strings.TrimSpace(name)
	if len(name) > 255 {
		return nil, errors.New("node name is too long")
	}
	res := s.db.Model(&Node{}).Where("uuid = ?", uuid).Update("name", name)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, ErrNotFound
	}
	s.publishNode(uuid, "name")
	return s.NodeWithGPUs(uuid)
}

// SetNodeNameIfEmpty fills the display name once from the first observed
// hostname. It intentionally does nothing after a user has named the node.
func (s *Store) SetNodeNameIfEmpty(uuid, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	res := s.db.Model(&Node{}).Where("uuid = ? AND (name = '' OR name IS NULL)", uuid).Update("name", name)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		s.publishNode(uuid, "name")
	}
	return nil
}

// RegenerateNodeToken replaces the authentication token and returns the
// plaintext value for display/copying by an administrator.
func (s *Store) RegenerateNodeToken(uuid string) (*Node, string, error) {
	token := auth.NewToken(24)
	if token == "" {
		return nil, "", errors.New("generate node token")
	}
	res := s.db.Model(&Node{}).Where("uuid = ?", uuid).Updates(map[string]any{
		"token": token, "token_hash": HashToken(token),
	})
	if res.Error != nil {
		return nil, "", res.Error
	}
	if res.RowsAffected == 0 {
		return nil, "", ErrNotFound
	}
	s.publishNode(uuid, "token")
	n, err := s.NodeWithGPUs(uuid)
	return n, token, err
}

// SetNodeState updates the connection state and last-seen time.
func (s *Store) SetNodeState(uuid, state string) error {
	now := time.Now().UTC()
	res := s.db.Model(&Node{}).Where("uuid = ?", uuid).
		Updates(map[string]any{"state": state, "last_seen_at": now})
	if res.Error != nil {
		return res.Error
	}
	s.publishNode(uuid, "state:"+state)
	return nil
}

// SetNodeServerDialAddress persists the endpoint learned from a successful
// reverse connection so the server can reconnect without repeating SSH setup.
func (s *Store) SetNodeServerDialAddress(uuid, addr string) error {
	res := s.db.Model(&Node{}).Where("uuid = ?", uuid).Update("server_dial_address", addr)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	s.publishNode(uuid, "server_dial_address")
	return nil
}

func (s *Store) SetNodeServerDialListen(uuid, listen string) error {
	res := s.db.Model(&Node{}).Where("uuid = ?", uuid).Update("server_dial_listen", strings.TrimSpace(listen))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	s.publishNode(uuid, "server_dial_listen")
	return nil
}

// SetNodeInstallDir records where an SSH install put the node's agent.
func (s *Store) SetNodeInstallDir(uuid, dir string) error {
	res := s.db.Model(&Node{}).Where("uuid = ?", uuid).Update("install_dir", strings.TrimRight(strings.TrimSpace(dir), "/"))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	s.publishNode(uuid, "install_dir")
	return nil
}

// SetNodeServerURL persists the control-plane address used to enroll a node.
func (s *Store) SetNodeServerURL(uuid, serverURL string) error {
	res := s.db.Model(&Node{}).Where("uuid = ?", uuid).Update("server_url", strings.TrimRight(strings.TrimSpace(serverURL), "/"))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	s.publishNode(uuid, "server_url")
	return nil
}

// DeleteNode removes an idle node and clears records that point at it; its
// name stays behind in DeletedNode for the statistics it leaves. Runtime
// instances must be removed through the deployment/controller path first.
func (s *Store) DeleteNode(uuid string) error {
	var nodeID uint
	var removals []InstanceEvent
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var n Node
		err := tx.Where("uuid = ?", uuid).First(&n).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		// Rows only waiting to be removed from the node (a replica moved
		// away while it was offline keeps its row until the node confirms)
		// go with it; any other instance means a deployment still runs here.
		leaving := []string{proto.InstRemoving, proto.InstPendingRemoval}
		var instanceCount int64
		if err := tx.Model(&Instance{}).Where("node_id = ? AND state NOT IN ?", n.ID, leaving).Count(&instanceCount).Error; err != nil {
			return err
		}
		if instanceCount > 0 {
			return fmt.Errorf("node %s has %d instance(s); stop deployments before deleting the node", uuid, instanceCount)
		}
		var gone []Instance
		if err := tx.Where("node_id = ? AND state IN ?", n.ID, leaving).Find(&gone).Error; err != nil {
			return err
		}
		for i := range gone {
			removed, err := deleteInstanceRecords(tx, &gone[i])
			if err != nil {
				return err
			}
			removals = append(removals, removed...)
		}
		if err := checkNoCacheDeletion(tx.Where("node_id = ?", n.ID)); err != nil {
			return err
		}
		if err := deleteNodeStorageRecords(tx, n.ID); err != nil {
			return err
		}
		if err := tx.Where("node_id = ?", n.ID).Delete(&ModelFile{}).Error; err != nil {
			return err
		}
		if err := tx.Where("node_id = ?", n.ID).Delete(&ImageNode{}).Error; err != nil {
			return err
		}
		if err := tx.Where("node_id = ?", n.ID).Delete(&ProviderBootstrapToken{}).Error; err != nil {
			return err
		}
		if err := tx.Where("node_id = ?", n.ID).Delete(&NodeSSHCredential{}).Error; err != nil {
			return err
		}
		if err := tx.Model(&ProviderInstance{}).Where("node_id = ?", n.ID).Update("node_id", 0).Error; err != nil {
			return err
		}
		if err := tx.Where("node_id = ?", n.ID).Delete(&NetworkAttachment{}).Error; err != nil {
			return err
		}
		if err := tx.Where("node_id = ?", n.ID).Delete(&GPU{}).Error; err != nil {
			return err
		}
		if err := tx.Where("node_id = ?", n.ID).Delete(&CardHealthEpisode{}).Error; err != nil {
			return err
		}
		if err := rememberDeletedNode(tx, n, time.Now().UTC()); err != nil {
			return err
		}
		if err := tx.Delete(&Node{}, n.ID).Error; err != nil {
			return err
		}
		nodeID = n.ID
		return nil
	})
	if err != nil {
		return err
	}
	for _, ev := range removals {
		s.observe(ev, nil)
	}
	s.Bus.PublishChange("node", uuid, fmt.Sprintf("deleted:%d", nodeID))
	return nil
}
