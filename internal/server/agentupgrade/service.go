package agentupgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type Service struct {
	st            *store.Store
	cfg           Config
	mu            sync.Mutex
	maintenance   bool
	wake          chan struct{}
	events        chan Observation
	running       atomic.Bool
	lastRetention time.Time
}

func New(st *store.Store, cfg Config) (*Service, error) {
	if st == nil || cfg.Transport == nil || cfg.ArtifactDir == "" {
		return nil, errors.New("agent upgrade store, transport and artifact directory are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.TransferTimeout <= 0 {
		cfg.TransferTimeout = 10 * time.Minute
	}
	if cfg.ConfirmTimeout <= 0 {
		cfg.ConfirmTimeout = 7 * time.Minute
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if err := os.MkdirAll(cfg.ArtifactDir, 0o700); err != nil {
		return nil, err
	}
	if err := st.RecoverAgentUpgrades(cfg.TargetVersion); err != nil {
		return nil, err
	}
	svc := &Service{st: st, cfg: cfg, wake: make(chan struct{}, 1), events: make(chan Observation, 256)}
	if err := svc.recoverArtifacts(); err != nil {
		return nil, err
	}
	return svc, nil
}
func (s *Service) now() time.Time { return s.cfg.Now().UTC() }
func (s *Service) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) blocker() (*Blocker, error) {
	item, err := s.st.AgentUpgradeBlocker()
	if err != nil {
		return nil, err
	}
	if item != nil {
		return &Blocker{RunID: item.RunID, NodeUUID: item.NodeUUID, Name: item.Name, Reason: item.Phase}, nil
	}
	queued, err := s.st.AgentUpgradeQueuedBlocker()
	if err != nil {
		return nil, err
	}
	if queued != nil {
		return &Blocker{RunID: queued.RunID, NodeUUID: queued.NodeUUID, Name: queued.Name, Reason: "active_run"}, nil
	}
	return nil, nil
}

func (s *Service) Preview(ctx context.Context, uuids []string) (Preview, error) {
	p, _, err := s.preview(ctx, uuids, false)
	return p, err
}
func (s *Service) preview(ctx context.Context, uuids []string, seal bool) (Preview, manifest, error) {
	p := Preview{TargetVersion: s.cfg.TargetVersion, Counts: map[string]int{}, Nodes: []Candidate{}, NodeUUIDs: []string{}}
	m := manifest{}
	if strings.TrimSpace(s.cfg.TargetVersion) == "" || s.cfg.TargetVersion == "dev" {
		return p, m, &Error{Code: "AGENT_UPGRADE_UNAVAILABLE", Detail: "the server has no identifiable agent target version"}
	}
	nodes, err := s.st.ListNodes()
	if err != nil {
		return p, m, err
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].UUID < nodes[j].UUID })
	wanted := map[string]bool{}
	for _, uuid := range uuids {
		if strings.TrimSpace(uuid) == "" {
			return p, m, &Error{Code: "AGENT_UPGRADE_INVALID_REQUEST", Detail: "node UUID cannot be empty"}
		}
		wanted[uuid] = true
	}
	found := map[string]bool{}
	artifactErrors := map[string]error{}
	for _, n := range nodes {
		if err := ctx.Err(); err != nil {
			return p, m, err
		}
		if len(uuids) > 0 && !wanted[n.UUID] {
			continue
		}
		found[n.UUID] = true
		c := s.candidate(n)
		if n.Arch != "" && (n.OS == "" || n.OS == "linux" || n.OS == "darwin") {
			name := proto.AgentBinaryName(n.OS, n.Arch)
			if _, ok := m[name]; !ok && artifactErrors[name] == nil {
				if s.cfg.ReadBinary == nil {
					artifactErrors[name] = errors.New("agent artifact reader is not configured")
				} else {
					data, readErr := s.cfg.ReadBinary(name)
					if readErr != nil {
						artifactErrors[name] = readErr
					} else if len(data) == 0 {
						artifactErrors[name] = errors.New("agent artifact is empty")
					} else {
						a := artifact{Name: name, SHA256: digest(data), Size: int64(len(data))}
						if seal {
							a, readErr = s.seal(name, data)
						}
						if readErr != nil {
							artifactErrors[name] = readErr
						} else {
							m[name] = a
						}
					}
				}
			}
			if err := artifactErrors[name]; err != nil && c.Eligibility == "eligible" {
				c.Eligibility, c.Reason = "unsupported", err.Error()
			}
		}
		p.NodeUUIDs = append(p.NodeUUIDs, n.UUID)
		p.Nodes = append(p.Nodes, c)
		p.Counts[c.Eligibility]++
	}
	for uuid := range wanted {
		if !found[uuid] {
			return p, m, &Error{Code: "AGENT_UPGRADE_NOT_FOUND", Detail: "node no longer exists: " + uuid}
		}
	}
	sort.Strings(p.NodeUUIDs)
	p.ManifestFingerprint = fingerprint(s.cfg.TargetVersion, m)
	p.BlockedByItem, err = s.blocker()
	return p, m, err
}

func (s *Service) Create(ctx context.Context, req CreateRequest) (RunView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(req.NodeUUIDs) == 0 || strings.TrimSpace(req.IdempotencyKey) == "" || len(req.IdempotencyKey) > 128 {
		return RunView{}, &Error{Code: "AGENT_UPGRADE_INVALID_REQUEST", Detail: "a fixed node snapshot and idempotency key are required"}
	}
	if s.maintenance {
		return RunView{}, &Error{Code: "AGENT_UPGRADE_ACTIVE", Detail: "the server is upgrading"}
	}
	uuids := slices.Clone(req.NodeUUIDs)
	sort.Strings(uuids)
	uuids = slices.Compact(uuids)
	b, _ := json.Marshal([]any{uuids, req.ExpectedTargetVersion, req.ExpectedManifestFingerprint, req.SourceRunID, req.RequestedBy})
	requestFingerprint := digest(b)
	runs, err := s.st.AgentUpgradeRuns()
	if err != nil {
		return RunView{}, err
	}
	for _, r := range runs {
		if r.IdempotencyKey == req.IdempotencyKey {
			if r.RequestFingerprint != requestFingerprint {
				return RunView{}, &Error{Code: "AGENT_UPGRADE_REQUEST_CHANGED", Detail: store.ErrAgentUpgradeRequestChanged.Error()}
			}
			return s.Get(ctx, r.ID)
		}
	}
	p, m, err := s.preview(ctx, uuids, true)
	if err != nil {
		return RunView{}, err
	}
	if req.ExpectedTargetVersion != p.TargetVersion || req.ExpectedManifestFingerprint != p.ManifestFingerprint {
		return RunView{}, &Error{Code: "AGENT_UPGRADE_TARGET_CHANGED", Detail: "upgrade target or agent artifacts changed; preview again"}
	}
	if p.Counts["eligible"] == 0 {
		return RunView{}, &Error{Code: "AGENT_UPGRADE_INVALID_REQUEST", Detail: "no eligible nodes remain"}
	}
	run := &store.AgentUpgradeRun{RequestedBy: req.RequestedBy, CreatedAt: s.now(), IdempotencyKey: req.IdempotencyKey, RequestFingerprint: requestFingerprint, TargetVersion: p.TargetVersion, SourceRunID: req.SourceRunID}
	items := make([]store.AgentUpgradeItem, 0, len(p.Nodes))
	for _, c := range p.Nodes {
		item := store.AgentUpgradeItem{NodeUUID: c.NodeUUID, Name: c.Name, FromVersion: c.FromVersion, OS: c.OS, Arch: c.Arch, Transport: c.Transport, ArtifactName: proto.AgentBinaryName(c.OS, c.Arch), Phase: "queued"}
		if a, ok := m[item.ArtifactName]; ok {
			item.ArtifactSHA256, item.ArtifactSize = a.SHA256, a.Size
		}
		if c.Eligibility != "eligible" {
			now := s.now()
			item.Phase, item.Reason, item.Error, item.FinishedAt = "skipped", c.Eligibility, c.Reason, &now
		}
		items = append(items, item)
	}
	saved, err := s.st.CreateAgentUpgrade(run, items, s.now())
	if errors.Is(err, store.ErrAgentUpgradeRequestChanged) {
		return RunView{}, &Error{Code: "AGENT_UPGRADE_REQUEST_CHANGED", Detail: err.Error()}
	}
	if errors.Is(err, store.ErrAgentUpgradeConflict) {
		return RunView{}, &Error{Code: "AGENT_UPGRADE_ACTIVE", Detail: err.Error()}
	}
	if err != nil {
		return RunView{}, err
	}
	s.notify()
	return s.Get(ctx, saved.ID)
}

func (s *Service) UpgradeNode(ctx context.Context, uuid, actor string) (RunView, error) {
	p, err := s.Preview(ctx, []string{uuid})
	if err != nil {
		return RunView{}, err
	}
	return s.Create(ctx, CreateRequest{NodeUUIDs: p.NodeUUIDs, ExpectedTargetVersion: p.TargetVersion, ExpectedManifestFingerprint: p.ManifestFingerprint, IdempotencyKey: fmt.Sprintf("single-%s-%d", uuid, s.now().UnixNano()), RequestedBy: actor})
}

func (s *Service) BeginServerUpgrade() (func(bool), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	blocker, err := s.blocker()
	if err != nil {
		return nil, err
	}
	if blocker != nil || s.maintenance {
		return nil, &Error{Code: "AGENT_UPGRADE_ACTIVE", Detail: "finish or verify the outstanding Agent upgrade before upgrading the server"}
	}
	s.maintenance = true
	var once sync.Once
	return func(success bool) {
		once.Do(func() {
			if !success {
				s.mu.Lock()
				s.maintenance = false
				s.mu.Unlock()
			}
		})
	}, nil
}
