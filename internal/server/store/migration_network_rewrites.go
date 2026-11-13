package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/sourceurl"
)

// migrateNetworkRewritesToSiteRules moves where nodes download from out of
// the network settings and into the pull source rules, and makes room for the
// proxies to follow (store.CarryOverNetworkProxies, which needs the server's
// key and so runs after the migrations):
//
//   - each URL rewrite of the global network setting becomes a site rule for
//     every node that still reaches the site itself, chosen for the
//     management node too (it never read the rewrites, which left it
//     registering models it could not read);
//   - each rewrite of a node's override becomes the same rule limited to that
//     node, before the global ones. An override used to replace the global
//     rewrites; a rule adds to them, so the node tries its own address, then
//     the global one, then the site - it only gains a fallback;
//   - the rewrites leave the network settings, whose proxies stay until they
//     are carried over;
//   - the proxy tables are created, and model_files loses the endpoint and
//     proxy a deployment once picked for its download.
//
// A rewrite that is not of a site root to an address (one with a path in
// what it replaced, say) cannot be a site rule and is dropped.
func migrateNetworkRewritesToSiteRules(db *gorm.DB) error {
	if err := db.AutoMigrate(&PullProxy{}, &PullProxyRequirement{}); err != nil {
		return err
	}
	if db.Migrator().HasTable("model_files") {
		if err := dropColumns(db, "model_files", "endpoint", "proxy", "network_explicit"); err != nil {
			return err
		}
	}
	if !db.Migrator().HasTable(&Setting{}) {
		return nil
	}
	var settings []Setting
	if err := db.Where("key = ? OR key LIKE ?", string(SettingNetwork), string(SettingNetworkNodePrefix)+"%").Order("key asc").Find(&settings).Error; err != nil {
		return err
	}
	var global, perNode []PullSourceRuleSpec
	for _, setting := range settings {
		var cfg proto.NetworkConfig
		if strings.TrimSpace(setting.Value) == "" || json.Unmarshal([]byte(setting.Value), &cfg) != nil {
			continue
		}
		if len(cfg.Rewrites) == 0 {
			continue
		}
		var reqs nodelabel.Requirements
		if setting.Key != string(SettingNetwork) {
			nodeID, err := strconv.ParseUint(strings.TrimPrefix(setting.Key, string(SettingNetworkNodePrefix)), 10, 64)
			if err != nil {
				continue
			}
			var node Node
			err = db.Select("id", "uuid").First(&node, uint(nodeID)).Error
			if err == nil {
				reqs = nodelabel.Requirements{nodelabel.KeyNode: {node.UUID}}
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}
		// A deleted node's override leaves no rule, but its rewrites still
		// leave the setting.
		if setting.Key == string(SettingNetwork) || reqs != nil {
			for _, rule := range siteRulesFromRewrites(cfg.Rewrites, reqs) {
				if reqs == nil {
					rule.ControlPlane = true
					global = append(global, rule)
				} else {
					perNode = append(perNode, rule)
				}
			}
		}
		cfg.Rewrites = nil
		encoded, err := json.Marshal(cfg)
		if err != nil {
			return err
		}
		if err := db.Model(&Setting{}).Where("key = ?", setting.Key).Update("value", string(encoded)).Error; err != nil {
			return err
		}
	}
	existing, err := readPullSourceRules(db)
	if err != nil {
		return err
	}
	position := 0
	for _, rule := range existing {
		if rule.Position >= position {
			position = rule.Position + 1
		}
	}
	for _, rule := range append(perNode, global...) {
		if hasSameSiteRule(existing, rule) {
			continue
		}
		row := PullSourceRule{Position: position, Match: rule.Match, AllowUpstream: rule.AllowUpstream, ControlPlane: rule.ControlPlane, Note: rule.Note}
		if err := db.Create(&row).Error; err != nil {
			return err
		}
		if err := writeRuleChildren(db, row.ID, rule); err != nil {
			return err
		}
		existing = append(existing, rule)
		position++
	}
	return nil
}

// siteRulesFromRewrites turns one setting's rewrites into site rules, one
// per site in the order the sites first appear, each with the rewrites'
// targets as its endpoints in order.
func siteRulesFromRewrites(rewrites []proto.URLRewrite, reqs nodelabel.Requirements) []PullSourceRuleSpec {
	var out []PullSourceRuleSpec
	index := map[string]int{}
	for _, rewrite := range rewrites {
		if sourceurl.ValidateMatch(rewrite.From) != nil {
			continue
		}
		match, err := sourceurl.Normalize(rewrite.From)
		if err != nil {
			continue
		}
		endpoint, err := sourceurl.Normalize(rewrite.To)
		if err != nil || endpoint == match {
			continue
		}
		at, ok := index[match]
		if !ok {
			at = len(out)
			index[match] = at
			out = append(out, PullSourceRuleSpec{Match: match, AllowUpstream: true, Note: "network settings", Requirements: reqs})
		}
		duplicate := false
		for _, have := range out[at].Endpoints {
			duplicate = duplicate || have == endpoint
		}
		if !duplicate {
			out[at].Endpoints = append(out[at].Endpoints, endpoint)
		}
	}
	return out
}

func hasSameSiteRule(rules []PullSourceRuleSpec, rule PullSourceRuleSpec) bool {
	for _, have := range rules {
		if have.Match == rule.Match && reflect.DeepEqual(have.Endpoints, rule.Endpoints) && reflect.DeepEqual(nodelabel.Requirements(have.Requirements), rule.Requirements) {
			return true
		}
	}
	return false
}
