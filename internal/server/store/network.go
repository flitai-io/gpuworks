package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
)

// NodeNetworkSettingKey is the settings key that held one node's network
// override before the proxies moved into the pull settings
// (CarryOverNetworkProxies).
func NodeNetworkSettingKey(nodeID uint) SettingKey {
	return SettingNetworkNodePrefix + SettingKey(uintKey(nodeID))
}

// CarryOverNetworkProxies moves the proxies of the network settings into
// proxy entries, then deletes the network settings, in one transaction. It
// needs the server's key for the passwords, so it runs at startup once the
// key is set rather than as a migration; until it succeeds the settings stay
// where they are and the next start tries again.
//
// Each node override becomes an entry for that node (gpuworks.ai/node=<uuid>)
// holding the values the node used: the override's fields over the global
// ones, as they were merged. They come first, in node order, so a node keeps
// exactly the proxy it had. The global setting becomes an entry for every node
// after them, also chosen for the management node, whose outbound requests
// used it. A login written into a proxy URL becomes the entry's user name and
// encrypted password.
func (s *Store) CarryOverNetworkProxies() error {
	moved := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var settings []Setting
		if err := tx.Where("key = ? OR key LIKE ?", string(SettingNetwork), string(SettingNetworkNodePrefix)+"%").Find(&settings).Error; err != nil {
			return err
		}
		if len(settings) == 0 {
			return nil
		}
		var global proto.NetworkConfig
		type override struct {
			nodeID uint
			cfg    proto.NetworkConfig
		}
		var overrides []override
		for _, setting := range settings {
			var cfg proto.NetworkConfig
			if strings.TrimSpace(setting.Value) != "" && json.Unmarshal([]byte(setting.Value), &cfg) != nil {
				continue
			}
			if setting.Key == string(SettingNetwork) {
				global = cfg
				continue
			}
			nodeID, err := strconv.ParseUint(strings.TrimPrefix(setting.Key, string(SettingNetworkNodePrefix)), 10, 64)
			if err != nil {
				continue
			}
			overrides = append(overrides, override{nodeID: uint(nodeID), cfg: cfg})
		}
		sort.Slice(overrides, func(i, j int) bool { return overrides[i].nodeID < overrides[j].nodeID })
		var entries []carriedProxy
		for _, o := range overrides {
			var node Node
			if err := tx.Select("id", "uuid").First(&node, o.nodeID).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					continue
				}
				return err
			}
			merged := mergedNodeNetwork(global, o.cfg)
			same := merged.HTTPProxy == global.HTTPProxy && merged.HTTPSProxy == global.HTTPSProxy && merged.NoProxy == global.NoProxy
			if same || (merged.HTTPProxy == "" && merged.HTTPSProxy == "") {
				// Nothing of its own: the entry for every node covers it.
				continue
			}
			entries = append(entries, carriedProxy{cfg: merged, reqs: nodelabel.Requirements{nodelabel.KeyNode: {node.UUID}}, note: "node network override"})
		}
		if global.HTTPProxy != "" || global.HTTPSProxy != "" {
			entries = append(entries, carriedProxy{cfg: global, controlPlane: true, note: "network settings"})
		}
		var last struct{ Max *int }
		if err := tx.Model(&PullProxy{}).Select("MAX(position) AS max").Scan(&last).Error; err != nil {
			return err
		}
		position := 0
		if last.Max != nil {
			position = *last.Max + 1
		}
		for _, entry := range entries {
			row, ok, err := s.carriedProxyRow(entry)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			row.Position = position
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			if err := writeProxyRequirements(tx, row.ID, entry.reqs); err != nil {
				return err
			}
			position++
		}
		moved = true
		return tx.Where("key = ? OR key LIKE ?", string(SettingNetwork), string(SettingNetworkNodePrefix)+"%").Delete(&Setting{}).Error
	})
	if err != nil {
		return err
	}
	if moved {
		s.pullSettingsChanged(PullSettingsProxies)
	}
	return nil
}

type carriedProxy struct {
	cfg          proto.NetworkConfig
	reqs         nodelabel.Requirements
	controlPlane bool
	note         string
}

// carriedProxyRow is one carried-over entry as it is stored; ok is false for
// an entry whose addresses no longer pass validation, which is dropped
// rather than blocking the rest.
func (s *Store) carriedProxyRow(entry carriedProxy) (PullProxy, bool, error) {
	username, password := "", ""
	strip := func(raw string) string {
		parsed, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || parsed.User == nil {
			return raw
		}
		if username == "" {
			username = parsed.User.Username()
			password, _ = parsed.User.Password()
		}
		parsed.User = nil
		return parsed.String()
	}
	spec, err := normalizePullProxy(PullProxySpec{
		HTTPProxy: strip(entry.cfg.HTTPProxy), HTTPSProxy: strip(entry.cfg.HTTPSProxy), NoProxy: entry.cfg.NoProxy,
		Username: username, ControlPlane: entry.controlPlane, Note: entry.note, Requirements: entry.reqs,
	})
	if err != nil {
		return PullProxy{}, false, nil //nolint:nilerr // Invalid legacy proxy addresses are deliberately dropped during carry-over.
	}
	cipher, err := s.sealProxyPassword(password)
	if err != nil {
		return PullProxy{}, false, fmt.Errorf("carry over a proxy password: %w", err)
	}
	return PullProxy{
		HTTPProxy: spec.HTTPProxy, HTTPSProxy: spec.HTTPSProxy, NoProxy: spec.NoProxy,
		Username: spec.Username, SecretCipher: cipher, ControlPlane: spec.ControlPlane, Note: spec.Note,
	}, true, nil
}

// mergedNodeNetwork is a node override applied over the global setting, field
// by field, as the network settings were merged.
func mergedNodeNetwork(cfg, override proto.NetworkConfig) proto.NetworkConfig {
	if override.HTTPProxy != "" {
		cfg.HTTPProxy = override.HTTPProxy
	}
	if override.HTTPSProxy != "" {
		cfg.HTTPSProxy = override.HTTPSProxy
	}
	if override.NoProxy != "" {
		cfg.NoProxy = override.NoProxy
	}
	cfg.Rewrites = nil
	return cfg
}
