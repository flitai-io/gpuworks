package store

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/imagecatalog/pullref"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
)

// PullSettingsChange is the bus topic a write to the pull source rules, the
// sync groups or the registry credentials publishes; the key names which.
const (
	PullSettingsTopic       = "pull_settings"
	PullSettingsRules       = "source_rules"
	PullSettingsSyncGroups  = "sync_groups"
	PullSettingsCredentials = "credentials"
	PullSettingsProxies     = "proxies"
)

// PullSourceRuleSpec is a pull source rule with its endpoints and node
// requirements, the shape every reader and writer works with.
type PullSourceRuleSpec struct {
	ID            uint
	Position      int
	Match         string
	Endpoints     []string
	AllowUpstream bool
	// ControlPlane: a site rule the control plane reads through too
	// (PullSourceRule.ControlPlane).
	ControlPlane bool
	Note         string
	Requirements nodelabel.Requirements
}

// SourceRule is the rule as the candidate planner reads it.
func (r PullSourceRuleSpec) SourceRule() pullref.SourceRule {
	return pullref.SourceRule{Match: r.Match, Endpoints: append([]string(nil), r.Endpoints...), AllowUpstream: r.AllowUpstream}
}

// SyncGroupSpec is a sync group with its node requirements.
type SyncGroupSpec struct {
	ID           uint
	Name         string
	RateLimitKiB int64
	Requirements nodelabel.Requirements
}

// SourceCredentialView is a login without its secret.
type SourceCredentialView struct {
	ID        uint
	Host      string
	Username  string
	HasSecret bool
}

// pullSettingsCache holds the rules and groups between writes: they are read
// for every launch, every desired-state replay and every automatic copy, and
// change only when an operator edits them.
type pullSettingsCache struct {
	mu     sync.RWMutex
	rules  []PullSourceRuleSpec
	groups []SyncGroupSpec
	loaded bool
	// proxies are read for every replay and every network sync too, with
	// their passwords decrypted once per write rather than once per node.
	proxies       []PullProxySpec
	proxiesLoaded bool
}

func (c *pullSettingsCache) invalidate() {
	c.mu.Lock()
	c.loaded, c.proxiesLoaded = false, false
	c.rules, c.groups, c.proxies = nil, nil, nil
	c.mu.Unlock()
}

func (s *Store) loadPullSettings() ([]PullSourceRuleSpec, []SyncGroupSpec, error) {
	s.pullCache.mu.RLock()
	if s.pullCache.loaded {
		rules, groups := s.pullCache.rules, s.pullCache.groups
		s.pullCache.mu.RUnlock()
		return rules, groups, nil
	}
	s.pullCache.mu.RUnlock()
	rules, err := readPullSourceRules(s.db)
	if err != nil {
		return nil, nil, err
	}
	groups, err := readSyncGroups(s.db)
	if err != nil {
		return nil, nil, err
	}
	s.pullCache.mu.Lock()
	s.pullCache.rules, s.pullCache.groups, s.pullCache.loaded = rules, groups, true
	s.pullCache.mu.Unlock()
	return rules, groups, nil
}

// PullSourceRules returns every rule in the order they are tried. The slice
// is shared: callers must not modify it.
func (s *Store) PullSourceRules() ([]PullSourceRuleSpec, error) {
	rules, _, err := s.loadPullSettings()
	return rules, err
}

// SyncGroups returns every sync group, the default first. The slice is
// shared: callers must not modify it.
func (s *Store) SyncGroups() ([]SyncGroupSpec, error) {
	_, groups, err := s.loadPullSettings()
	return groups, err
}

func readPullSourceRules(db *gorm.DB) ([]PullSourceRuleSpec, error) {
	var rows []PullSourceRule
	if err := db.Order("position asc, id asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return []PullSourceRuleSpec{}, nil
	}
	var endpoints []PullSourceRuleEndpoint
	if err := db.Order("rule_id asc, position asc").Find(&endpoints).Error; err != nil {
		return nil, err
	}
	var reqs []PullSourceRuleRequirement
	if err := db.Order("rule_id asc, key asc, value asc").Find(&reqs).Error; err != nil {
		return nil, err
	}
	byRule := make(map[uint][]string, len(rows))
	for _, endpoint := range endpoints {
		byRule[endpoint.RuleID] = append(byRule[endpoint.RuleID], endpoint.Endpoint)
	}
	reqsByRule := make(map[uint]nodelabel.Requirements, len(rows))
	for _, req := range reqs {
		if reqsByRule[req.RuleID] == nil {
			reqsByRule[req.RuleID] = nodelabel.Requirements{}
		}
		reqsByRule[req.RuleID][req.Key] = append(reqsByRule[req.RuleID][req.Key], req.Value)
	}
	out := make([]PullSourceRuleSpec, 0, len(rows))
	for _, row := range rows {
		out = append(out, PullSourceRuleSpec{
			ID: row.ID, Position: row.Position, Match: row.Match, AllowUpstream: row.AllowUpstream, ControlPlane: row.ControlPlane, Note: row.Note,
			Endpoints: byRule[row.ID], Requirements: reqsByRule[row.ID],
		})
	}
	return out, nil
}

func readSyncGroups(db *gorm.DB) ([]SyncGroupSpec, error) {
	var rows []SyncGroup
	if err := db.Order("id asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return []SyncGroupSpec{}, nil
	}
	var reqs []SyncGroupRequirement
	if err := db.Order("group_id asc, key asc, value asc").Find(&reqs).Error; err != nil {
		return nil, err
	}
	byGroup := make(map[uint]nodelabel.Requirements, len(rows))
	for _, req := range reqs {
		if byGroup[req.GroupID] == nil {
			byGroup[req.GroupID] = nodelabel.Requirements{}
		}
		byGroup[req.GroupID][req.Key] = append(byGroup[req.GroupID][req.Key], req.Value)
	}
	out := make([]SyncGroupSpec, 0, len(rows))
	for _, row := range rows {
		out = append(out, SyncGroupSpec{ID: row.ID, Name: row.Name, RateLimitKiB: row.RateLimitKiB, Requirements: byGroup[row.ID]})
	}
	return out, nil
}

// normalizePullSourceRule validates a rule and spells it the way it is
// stored: match and endpoints as their kind spells them (pull_rule_kind.go),
// normalized requirements.
func normalizePullSourceRule(spec PullSourceRuleSpec) (PullSourceRuleSpec, error) {
	match, endpoints, err := normalizeRuleAddresses(spec.Match, spec.Endpoints)
	if err != nil {
		return spec, err
	}
	spec.Match = match
	if spec.ControlPlane && spec.Kind() != PullRuleURL {
		return spec, pullSettingError("control_plane", fmt.Errorf("only a rule for a site is read by the control plane; it pulls no images"))
	}
	if len(endpoints) > 16 {
		return spec, pullSettingError("endpoints", fmt.Errorf("a rule lists at most 16 endpoints"))
	}
	if len(endpoints) == 0 && spec.AllowUpstream {
		return spec, pullSettingError("endpoints", fmt.Errorf("a rule that allows its own registry needs at least one endpoint"))
	}
	spec.Endpoints = endpoints
	spec.Note = strings.TrimSpace(spec.Note)
	if len(spec.Note) > 255 {
		return spec, pullSettingError("note", fmt.Errorf("note is longer than 255 bytes"))
	}
	reqs, err := nodelabel.NormalizeRequirements(spec.Requirements)
	if err != nil {
		return spec, pullSettingError("requirements", err)
	}
	spec.Requirements = reqs
	return spec, nil
}

func writeRuleChildren(tx *gorm.DB, id uint, spec PullSourceRuleSpec) error {
	if err := tx.Where("rule_id = ?", id).Delete(&PullSourceRuleEndpoint{}).Error; err != nil {
		return err
	}
	if err := tx.Where("rule_id = ?", id).Delete(&PullSourceRuleRequirement{}).Error; err != nil {
		return err
	}
	for i, endpoint := range spec.Endpoints {
		if err := tx.Create(&PullSourceRuleEndpoint{RuleID: id, Position: i, Endpoint: endpoint}).Error; err != nil {
			return err
		}
	}
	for _, key := range sortedRequirementKeys(spec.Requirements) {
		for _, value := range spec.Requirements[key] {
			if err := tx.Create(&PullSourceRuleRequirement{RuleID: id, Key: key, Value: value}).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func sortedRequirementKeys(reqs nodelabel.Requirements) []string {
	keys := make([]string, 0, len(reqs))
	for key := range reqs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// CreatePullSourceRule adds a rule after every existing one.
func (s *Store) CreatePullSourceRule(spec PullSourceRuleSpec) (PullSourceRuleSpec, error) {
	spec, err := normalizePullSourceRule(spec)
	if err != nil {
		return spec, err
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		var last struct{ Max *int }
		if err := tx.Model(&PullSourceRule{}).Select("MAX(position) AS max").Scan(&last).Error; err != nil {
			return err
		}
		spec.Position = 0
		if last.Max != nil {
			spec.Position = *last.Max + 1
		}
		row := PullSourceRule{Position: spec.Position, Match: spec.Match, AllowUpstream: spec.AllowUpstream, ControlPlane: spec.ControlPlane, Note: spec.Note}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		spec.ID = row.ID
		return writeRuleChildren(tx, row.ID, spec)
	})
	if err != nil {
		return spec, err
	}
	s.pullSettingsChanged(PullSettingsRules)
	return spec, nil
}

// UpdatePullSourceRule replaces a rule's content; its position is kept.
func (s *Store) UpdatePullSourceRule(id uint, spec PullSourceRuleSpec) (PullSourceRuleSpec, error) {
	spec, err := normalizePullSourceRule(spec)
	if err != nil {
		return spec, err
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		var row PullSourceRule
		if err := tx.First(&row, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		row.Match, row.AllowUpstream, row.ControlPlane, row.Note = spec.Match, spec.AllowUpstream, spec.ControlPlane, spec.Note
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		spec.ID, spec.Position = row.ID, row.Position
		return writeRuleChildren(tx, row.ID, spec)
	})
	if err != nil {
		return spec, err
	}
	s.pullSettingsChanged(PullSettingsRules)
	return spec, nil
}

// DeletePullSourceRule removes a rule.
func (s *Store) DeletePullSourceRule(id uint) error {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Delete(&PullSourceRule{}, id)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		if err := tx.Where("rule_id = ?", id).Delete(&PullSourceRuleEndpoint{}).Error; err != nil {
			return err
		}
		return tx.Where("rule_id = ?", id).Delete(&PullSourceRuleRequirement{}).Error
	})
	if err != nil {
		return err
	}
	s.pullSettingsChanged(PullSettingsRules)
	return nil
}

// ReorderPullSourceRules sets the order rules are tried in. ids must name every
// rule exactly once.
func (s *Store) ReorderPullSourceRules(ids []uint) error {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var existing []uint
		if err := tx.Model(&PullSourceRule{}).Pluck("id", &existing).Error; err != nil {
			return err
		}
		if len(existing) != len(ids) {
			return pullSettingError("order", fmt.Errorf("the order must name each of the %d rules exactly once", len(existing)))
		}
		known := make(map[uint]bool, len(existing))
		for _, id := range existing {
			known[id] = true
		}
		for _, id := range ids {
			if !known[id] {
				return pullSettingError("order", fmt.Errorf("the order must name each of the %d rules exactly once", len(existing)))
			}
			delete(known, id)
		}
		for i, id := range ids {
			if err := tx.Model(&PullSourceRule{}).Where("id = ?", id).Update("position", i).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.pullSettingsChanged(PullSettingsRules)
	return nil
}

func normalizeSyncGroup(spec SyncGroupSpec) (SyncGroupSpec, error) {
	if !utf8.ValidString(spec.Name) || strings.ContainsFunc(spec.Name, unicode.IsControl) {
		return spec, pullSettingReasonError("name", "invalid_text", fmt.Errorf("name must contain valid text without control characters"), nil)
	}
	spec.Name = strings.TrimSpace(spec.Name)
	if spec.Name == "" {
		return spec, pullSettingReasonError("name", "name_required", fmt.Errorf("a sync group needs a name"), nil)
	}
	if len(spec.Name) > syncGroupNameMaxBytes {
		return spec, pullSettingReasonError("name", "max_bytes", fmt.Errorf("name is longer than %d bytes", syncGroupNameMaxBytes), map[string]any{"max": syncGroupNameMaxBytes})
	}
	if spec.RateLimitKiB < 0 {
		return spec, pullSettingReasonError("rate_limit", "nonnegative", fmt.Errorf("rate limit must not be negative"), nil)
	}
	reqs, err := nodelabel.NormalizeRequirements(spec.Requirements)
	if err != nil {
		return spec, pullSettingReasonError("requirements", "requirements_invalid", err, nil)
	}
	spec.Requirements = reqs
	return spec, nil
}

func writeGroupRequirements(tx *gorm.DB, id uint, reqs nodelabel.Requirements) error {
	if err := tx.Where("group_id = ?", id).Delete(&SyncGroupRequirement{}).Error; err != nil {
		return err
	}
	for _, key := range sortedRequirementKeys(reqs) {
		for _, value := range reqs[key] {
			if err := tx.Create(&SyncGroupRequirement{GroupID: id, Key: key, Value: value}).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// CreateSyncGroup adds a sync group.
func (s *Store) CreateSyncGroup(spec SyncGroupSpec) (SyncGroupSpec, error) {
	spec, err := normalizeSyncGroup(spec)
	if err != nil {
		return spec, err
	}
	if err := s.checkSyncGroupName(spec.Name, 0); err != nil {
		return spec, err
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		row := SyncGroup{Name: spec.Name, RateLimitKiB: spec.RateLimitKiB}
		if err := tx.Create(&row).Error; err != nil {
			return syncGroupWriteError(err)
		}
		spec.ID = row.ID
		return writeGroupRequirements(tx, row.ID, spec.Requirements)
	})
	if err != nil {
		return spec, err
	}
	s.pullSettingsChanged(PullSettingsSyncGroups)
	return spec, nil
}

// UpdateSyncGroup replaces a group's name, requirements and rate limit.
func (s *Store) UpdateSyncGroup(id uint, spec SyncGroupSpec) (SyncGroupSpec, error) {
	// Check existence before validation, as other update paths do. This read
	// completes before the write transaction, avoiding a read-to-write upgrade
	// when two independent SQLite connections rename groups concurrently.
	var row SyncGroup
	if err := s.db.Select("id").First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return spec, ErrNotFound
		}
		return spec, err
	}
	spec, err := normalizeSyncGroup(spec)
	if err != nil {
		return spec, err
	}
	if err := s.checkSyncGroupName(spec.Name, id); err != nil {
		return spec, err
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&SyncGroup{}).Where("id = ?", id).Updates(map[string]any{"name": spec.Name, "rate_limit_ki_b": spec.RateLimitKiB})
		if result.Error != nil {
			return syncGroupWriteError(result.Error)
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		spec.ID = id
		return writeGroupRequirements(tx, id, spec.Requirements)
	})
	if err != nil {
		return spec, err
	}
	s.pullSettingsChanged(PullSettingsSyncGroups)
	return spec, nil
}

// DeleteSyncGroup removes a sync group, the default one included.
func (s *Store) DeleteSyncGroup(id uint) error {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Delete(&SyncGroup{}, id)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		return tx.Where("group_id = ?", id).Delete(&SyncGroupRequirement{}).Error
	})
	if err != nil {
		return err
	}
	s.pullSettingsChanged(PullSettingsSyncGroups)
	return nil
}

func (s *Store) pullSettingsChanged(which string) {
	s.pullCache.invalidate()
	if s.Bus != nil {
		s.Bus.PublishChange(PullSettingsTopic, which, "changed")
	}
}
