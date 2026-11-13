package store

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
)

// DefaultSyncGroupName names the group an installation starts with, which
// selects every node. It is an ordinary group: renamed, narrowed or deleted
// like any other.
const DefaultSyncGroupName = "default-all-nodes"

// migratePullSettingsTables creates the pull source rule, sync group and
// registry credential tables and seeds a sync group that selects every node:
// before sync groups existed every node copied from any other, and a
// single-site cluster keeps doing so without being configured.
func migratePullSettingsTables(db *gorm.DB) error {
	if err := db.AutoMigrate(
		&PullSourceRule{}, &PullSourceRuleEndpoint{}, &PullSourceRuleRequirement{},
		&SyncGroup{}, &SyncGroupRequirement{}, &registryCredentialTable{},
	); err != nil {
		return err
	}
	var count int64
	if err := db.Model(&SyncGroup{}).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	return db.Create(&SyncGroup{Name: DefaultSyncGroupName}).Error
}

// shippedRewriteRules are the rules the image_registry_rewrites setting was
// seeded with. They described where gpuworks publishes its own images, which
// the image catalog now records as the images' registry sources, and where
// gpustack publishes its; neither is an operator's choice, so neither becomes a
// pull source rule.
var shippedRewriteRules = map[string]bool{
	"gpuworks/gpuworks-runner": true,
	"gpustack/":                true,
	"gpustack":                 true,
}

// internalRegistryHost is the registry the shipped rule for gpuworks images
// listed beside the public ones. It is not public; an installation that had it
// keeps reaching it through a rule of its own.
const internalRegistryHost = "registry.zstack.ai"

type legacyRewriteConfig struct {
	Default string `json:"default"`
	Rules   []struct {
		Prefix  string   `json:"prefix"`
		Mirrors []string `json:"mirrors"`
	} `json:"rules"`
}

// migratePullSourceRules turns where image pull sources used to be configured
// into pull source rules, and removes those settings:
//
//   - rules an operator added to image_registry_rewrites become rules for
//     every node; the shipped ones are dropped (see shippedRewriteRules), and
//     an installation whose setting named the internal registry gets the rule
//     gpuworks/ → registry.zstack.ai/gpuworks/ so it keeps reaching it;
//   - image rewrites in the global network setting become rules for every
//     node, those in a node's network override rules for that node
//     (gpuworks.ai/node). URL rewrites stay where they are: they redirect
//     model and archive downloads.
//
// A fresh installation has none of these settings and gets no rule.
func migratePullSourceRules(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Setting{}) {
		return nil
	}
	var rules, nodeRules, networkRules []PullSourceRuleSpec
	internal := false

	raw, err := migrationSettingValue(db, SettingImageRegistryRewrites)
	if err != nil {
		return err
	}
	if strings.TrimSpace(raw) == "" {
		legacy, err := migrationSettingValue(db, SettingImageRegistryMirrors)
		if err != nil {
			return err
		}
		raw = migrateImageRegistryMirrors(legacy)
	}
	if strings.TrimSpace(raw) != "" {
		var cfg legacyRewriteConfig
		if json.Unmarshal([]byte(raw), &cfg) == nil {
			for _, rule := range cfg.Rules {
				for _, mirror := range rule.Mirrors {
					if strings.Contains(strings.ToLower(mirror), internalRegistryHost) {
						internal = true
					}
				}
				if shippedRewriteRules[strings.TrimSpace(rule.Prefix)] {
					continue
				}
				if spec, ok := legacyRule(rule.Prefix, rule.Mirrors, nil); ok {
					rules = append(rules, spec)
				}
			}
		}
	}

	var settings []Setting
	if err := db.Where("key = ? OR key LIKE ?", string(SettingNetwork), string(SettingNetworkNodePrefix)+"%").Order("key asc").Find(&settings).Error; err != nil {
		return err
	}
	for _, setting := range settings {
		var cfg proto.NetworkConfig
		if strings.TrimSpace(setting.Value) == "" || json.Unmarshal([]byte(setting.Value), &cfg) != nil {
			continue
		}
		var reqs nodelabel.Requirements
		if setting.Key != string(SettingNetwork) {
			nodeID, err := strconv.ParseUint(strings.TrimPrefix(setting.Key, string(SettingNetworkNodePrefix)), 10, 64)
			if err != nil {
				continue
			}
			var node Node
			if err := db.Select("id", "uuid").First(&node, uint(nodeID)).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					continue
				}
				return err
			}
			reqs = nodelabel.Requirements{nodelabel.KeyNode: {node.UUID}}
		}
		kept := cfg.Rewrites[:0]
		changed := false
		for _, rewrite := range cfg.Rewrites {
			if isURLRewrite(rewrite.From) {
				kept = append(kept, rewrite)
				continue
			}
			spec, ok := legacyRule(rewrite.From, []string{rewrite.To}, reqs)
			if !ok {
				kept = append(kept, rewrite)
				continue
			}
			changed = true
			if reqs != nil {
				nodeRules = append(nodeRules, spec)
			} else {
				networkRules = append(networkRules, spec)
			}
		}
		if !changed {
			continue
		}
		cfg.Rewrites = kept
		if len(cfg.Rewrites) == 0 {
			cfg.Rewrites = nil
		}
		encoded, err := json.Marshal(cfg)
		if err != nil {
			return err
		}
		if err := db.Model(&Setting{}).Where("key = ?", setting.Key).Update("value", string(encoded)).Error; err != nil {
			return err
		}
	}

	// A node's own rules are the most specific, so they are tried first; the
	// internal registry, which may not be reachable at all, last.
	ordered := append(append(append([]PullSourceRuleSpec{}, nodeRules...), networkRules...), rules...)
	if internal {
		ordered = append(ordered, PullSourceRuleSpec{
			Match: "gpuworks/", Endpoints: []string{internalRegistryHost + "/gpuworks/"}, AllowUpstream: true,
		})
	}
	var position int
	var last struct{ Max *int }
	if err := db.Model(&PullSourceRule{}).Select("MAX(position) AS max").Scan(&last).Error; err != nil {
		return err
	}
	if last.Max != nil {
		position = *last.Max + 1
	}
	for _, rule := range ordered {
		row := PullSourceRule{Position: position, Match: rule.Match, AllowUpstream: rule.AllowUpstream}
		if err := db.Create(&row).Error; err != nil {
			return err
		}
		if err := writeRuleChildren(db, row.ID, rule); err != nil {
			return err
		}
		position++
	}
	return db.Where("key IN ?", []string{string(SettingImageRegistryRewrites), string(SettingImageRegistryMirrors)}).Delete(&Setting{}).Error
}

// legacyRule is a rewrite (prefix → mirrors) as a pull source rule, or false
// when it is not a valid one. A rewrite always fell back to the image's own
// address, so the rule allows it.
func legacyRule(prefix string, mirrors []string, reqs nodelabel.Requirements) (PullSourceRuleSpec, bool) {
	spec, err := normalizePullSourceRule(PullSourceRuleSpec{
		Match: strings.TrimSpace(prefix), Endpoints: mirrors, AllowUpstream: true, Requirements: reqs,
	})
	if err != nil || len(spec.Endpoints) == 0 {
		return PullSourceRuleSpec{}, false
	}
	return spec, true
}

func isURLRewrite(from string) bool {
	from = strings.ToLower(strings.TrimSpace(from))
	return strings.HasPrefix(from, "http://") || strings.HasPrefix(from, "https://")
}

func migrationSettingValue(db *gorm.DB, key SettingKey) (string, error) {
	var setting Setting
	err := db.Where("key = ?", string(key)).First(&setting).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	return setting.Value, err
}

// migrateDropAcquisitionChoices removes the acquisition record (model_source,
// image_pull) from every deployment's catalog selection snapshot. Where a
// deployment's model and image come from is no longer a deployment's choice:
// each node gets them from a node of its sync group or through the pull source
// rules, so a recorded choice would only be read as one. The snapshot's
// top-level keys are stored sorted, so removing one key and re-encoding keeps
// every other entry byte for byte.
func migrateDropAcquisitionChoices(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Deployment{}) {
		return nil
	}
	var rows []struct {
		ID               uint
		TemplateSnapshot string
	}
	if err := db.Model(&Deployment{}).Select("id", "template_snapshot").
		Where("template_snapshot LIKE ?", `%"acquisition"%`).Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		doc := map[string]json.RawMessage{}
		if json.Unmarshal([]byte(row.TemplateSnapshot), &doc) != nil {
			continue
		}
		if _, ok := doc["acquisition"]; !ok {
			continue
		}
		delete(doc, "acquisition")
		encoded, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		if err := db.Model(&Deployment{}).Where("id = ?", row.ID).Update("template_snapshot", string(encoded)).Error; err != nil {
			return err
		}
	}
	return nil
}

// addPullSourceRuleControlPlane adds the choice of whether the control plane
// reads a site through a rule. No rule chose it before, so every existing
// rule leaves it off.
func addPullSourceRuleControlPlane(db *gorm.DB) error {
	if !db.Migrator().HasTable(&PullSourceRule{}) || db.Migrator().HasColumn(&PullSourceRule{}, "ControlPlane") {
		return nil
	}
	return db.Migrator().AddColumn(&PullSourceRule{}, "ControlPlane")
}

// migrateSourceCredentials moves the registry logins into source_credentials,
// keyed by host: a model site's token is a login of the same kind, with no
// user name. Ids and ciphertexts are kept.
func migrateSourceCredentials(db *gorm.DB) error {
	if err := db.AutoMigrate(&SourceCredential{}); err != nil {
		return err
	}
	if !db.Migrator().HasTable(&registryCredentialTable{}) {
		return nil
	}
	if err := db.Exec(`INSERT INTO source_credentials (id, host, username, secret_cipher, created_at, updated_at)
		SELECT id, registry, username, secret_cipher, created_at, updated_at FROM registry_credentials`).Error; err != nil {
		return err
	}
	return db.Migrator().DropTable(&registryCredentialTable{})
}
