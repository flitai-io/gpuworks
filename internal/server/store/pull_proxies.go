package store

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
)

// PullProxySpec is a proxy entry with its node requirements. Password is the
// decrypted password, for the nodes the entry applies to and the management
// node; it never leaves the server any other way (HasSecret says whether one
// is stored).
type PullProxySpec struct {
	ID           uint
	Position     int
	HTTPProxy    string
	HTTPSProxy   string
	NoProxy      string
	Username     string
	Password     string
	HasSecret    bool
	ControlPlane bool
	Note         string
	Requirements nodelabel.Requirements
}

// NetworkConfig is the entry as a client applies it: the proxy URLs with the
// login in them (a Docker daemon and a CLI take nothing else), and the hosts
// that bypass them.
func (p PullProxySpec) NetworkConfig() proto.NetworkConfig {
	return proto.NetworkConfig{
		HTTPProxy:  proxyURLWithLogin(p.HTTPProxy, p.Username, p.Password),
		HTTPSProxy: proxyURLWithLogin(p.HTTPSProxy, p.Username, p.Password),
		NoProxy:    p.NoProxy,
	}
}

func proxyURLWithLogin(raw, username, password string) string {
	if raw == "" || username == "" {
		return raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if password != "" {
		parsed.User = url.UserPassword(username, password)
	} else {
		parsed.User = url.User(username)
	}
	return parsed.String()
}

// PullProxies returns every proxy entry in the order they are matched, with
// their passwords decrypted. The slice is shared: callers must not modify it.
func (s *Store) PullProxies() ([]PullProxySpec, error) {
	s.pullCache.mu.RLock()
	if s.pullCache.proxiesLoaded {
		proxies := s.pullCache.proxies
		s.pullCache.mu.RUnlock()
		return proxies, nil
	}
	s.pullCache.mu.RUnlock()
	proxies, err := s.readPullProxies(s.db)
	if err != nil {
		return nil, err
	}
	s.pullCache.mu.Lock()
	s.pullCache.proxies, s.pullCache.proxiesLoaded = proxies, true
	s.pullCache.mu.Unlock()
	return proxies, nil
}

func (s *Store) readPullProxies(db *gorm.DB) ([]PullProxySpec, error) {
	var rows []PullProxy
	if err := db.Order("position asc, id asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return []PullProxySpec{}, nil
	}
	var reqs []PullProxyRequirement
	if err := db.Order("proxy_id asc, key asc, value asc").Find(&reqs).Error; err != nil {
		return nil, err
	}
	byProxy := make(map[uint]nodelabel.Requirements, len(rows))
	for _, req := range reqs {
		if byProxy[req.ProxyID] == nil {
			byProxy[req.ProxyID] = nodelabel.Requirements{}
		}
		byProxy[req.ProxyID][req.Key] = append(byProxy[req.ProxyID][req.Key], req.Value)
	}
	out := make([]PullProxySpec, 0, len(rows))
	for _, row := range rows {
		spec := PullProxySpec{
			ID: row.ID, Position: row.Position, HTTPProxy: row.HTTPProxy, HTTPSProxy: row.HTTPSProxy,
			NoProxy: row.NoProxy, Username: row.Username, HasSecret: row.SecretCipher != "",
			ControlPlane: row.ControlPlane, Note: row.Note, Requirements: byProxy[row.ID],
		}
		// A password the server's key no longer opens is sent as none: the
		// proxy then refuses the node, which says more than a wrong one.
		if row.SecretCipher != "" && s.secretbox != nil {
			if raw, err := s.secretbox.Decrypt(row.SecretCipher); err == nil {
				spec.Password = string(raw)
			}
		}
		out = append(out, spec)
	}
	return out, nil
}

// normalizeProxyURL spells a proxy address the way it is stored: a scheme
// (http:// when none is written), a host and an optional port, nothing else.
// A login written into the URL is refused: it has fields of its own, so it
// is never stored or shown in clear.
func normalizeProxyURL(field, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return "", pullSettingError(field, fmt.Errorf("the proxy address contains whitespace"))
	}
	withScheme := raw
	if !strings.Contains(withScheme, "://") {
		withScheme = "http://" + withScheme
	}
	parsed, err := url.Parse(withScheme)
	if err != nil {
		return "", pullSettingError(field, fmt.Errorf("the proxy address is not a URL"))
	}
	if parsed.User != nil {
		return "", pullSettingError(field, fmt.Errorf("put the proxy's user name and password in their own fields, not in the address"))
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", pullSettingError(field, fmt.Errorf("a proxy address starts with http://, https://, socks5:// or socks5h://"))
	}
	if parsed.Hostname() == "" || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", pullSettingError(field, fmt.Errorf("a proxy address is a scheme, a host and a port, nothing more"))
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host), nil
}

// normalizeNoProxy keeps a NO_PROXY list's entries trimmed and non-empty.
func normalizeNoProxy(raw string) (string, error) {
	var entries []string
	for _, entry := range strings.Split(raw, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			if strings.ContainsAny(entry, " \t") {
				return "", pullSettingError("no_proxy", fmt.Errorf("%q contains whitespace", entry))
			}
			entries = append(entries, entry)
		}
	}
	joined := strings.Join(entries, ",")
	if len(joined) > 2048 {
		return "", pullSettingError("no_proxy", fmt.Errorf("the list is longer than 2048 bytes"))
	}
	return joined, nil
}

func normalizePullProxy(spec PullProxySpec) (PullProxySpec, error) {
	var err error
	if spec.HTTPProxy, err = normalizeProxyURL("http_proxy", spec.HTTPProxy); err != nil {
		return spec, err
	}
	if spec.HTTPSProxy, err = normalizeProxyURL("https_proxy", spec.HTTPSProxy); err != nil {
		return spec, err
	}
	if spec.HTTPProxy == "" && spec.HTTPSProxy == "" {
		return spec, pullSettingError("http_proxy", fmt.Errorf("an entry names an HTTP or an HTTPS proxy, or both"))
	}
	if spec.NoProxy, err = normalizeNoProxy(spec.NoProxy); err != nil {
		return spec, err
	}
	spec.Username = strings.TrimSpace(spec.Username)
	if len(spec.Username) > 255 {
		return spec, pullSettingError("username", fmt.Errorf("user name is longer than 255 bytes"))
	}
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

// sealProxyPassword encrypts a proxy password for storage.
func (s *Store) sealProxyPassword(password string) (string, error) {
	if password == "" {
		return "", nil
	}
	if s.secretbox == nil {
		return "", fmt.Errorf("proxy passwords need the server's secret key")
	}
	cipher, err := s.secretbox.Encrypt([]byte(password))
	if err != nil {
		return "", err
	}
	if len(cipher) > 2048 {
		return "", pullSettingError("password", fmt.Errorf("the password is too long"))
	}
	return cipher, nil
}

func writeProxyRequirements(tx *gorm.DB, id uint, reqs nodelabel.Requirements) error {
	if err := tx.Where("proxy_id = ?", id).Delete(&PullProxyRequirement{}).Error; err != nil {
		return err
	}
	for _, key := range sortedRequirementKeys(reqs) {
		for _, value := range reqs[key] {
			if err := tx.Create(&PullProxyRequirement{ProxyID: id, Key: key, Value: value}).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// CreatePullProxy adds a proxy entry after every existing one; password is
// its password, empty for none.
func (s *Store) CreatePullProxy(spec PullProxySpec, password string) (PullProxySpec, error) {
	spec, err := normalizePullProxy(spec)
	if err != nil {
		return spec, err
	}
	if password != "" && spec.Username == "" {
		return spec, pullSettingError("username", fmt.Errorf("a password needs a user name"))
	}
	cipher, err := s.sealProxyPassword(password)
	if err != nil {
		return spec, err
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		var last struct{ Max *int }
		if err := tx.Model(&PullProxy{}).Select("MAX(position) AS max").Scan(&last).Error; err != nil {
			return err
		}
		spec.Position = 0
		if last.Max != nil {
			spec.Position = *last.Max + 1
		}
		row := PullProxy{
			Position: spec.Position, HTTPProxy: spec.HTTPProxy, HTTPSProxy: spec.HTTPSProxy, NoProxy: spec.NoProxy,
			Username: spec.Username, SecretCipher: cipher, ControlPlane: spec.ControlPlane, Note: spec.Note,
		}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		spec.ID = row.ID
		return writeProxyRequirements(tx, row.ID, spec.Requirements)
	})
	if err != nil {
		return spec, err
	}
	spec.HasSecret, spec.Password = cipher != "", password
	s.pullSettingsChanged(PullSettingsProxies)
	return spec, nil
}

// UpdatePullProxy replaces an entry's content; its position is kept. A nil
// password keeps the stored one, an empty one removes it.
func (s *Store) UpdatePullProxy(id uint, spec PullProxySpec, password *string) (PullProxySpec, error) {
	spec, err := normalizePullProxy(spec)
	if err != nil {
		return spec, err
	}
	var cipher string
	if password != nil {
		if cipher, err = s.sealProxyPassword(*password); err != nil {
			return spec, err
		}
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		var row PullProxy
		if err := tx.First(&row, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		row.HTTPProxy, row.HTTPSProxy, row.NoProxy = spec.HTTPProxy, spec.HTTPSProxy, spec.NoProxy
		row.Username, row.ControlPlane, row.Note = spec.Username, spec.ControlPlane, spec.Note
		if password != nil {
			row.SecretCipher = cipher
		}
		if row.SecretCipher != "" && row.Username == "" {
			return pullSettingError("username", fmt.Errorf("a password needs a user name"))
		}
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		spec.ID, spec.Position, spec.HasSecret = row.ID, row.Position, row.SecretCipher != ""
		return writeProxyRequirements(tx, row.ID, spec.Requirements)
	})
	if err != nil {
		return spec, err
	}
	s.pullSettingsChanged(PullSettingsProxies)
	return spec, nil
}

// DeletePullProxy removes a proxy entry.
func (s *Store) DeletePullProxy(id uint) error {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Delete(&PullProxy{}, id)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		return tx.Where("proxy_id = ?", id).Delete(&PullProxyRequirement{}).Error
	})
	if err != nil {
		return err
	}
	s.pullSettingsChanged(PullSettingsProxies)
	return nil
}

// ReorderPullProxies sets the order entries are matched in. ids must name
// every entry exactly once.
func (s *Store) ReorderPullProxies(ids []uint) error {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var existing []uint
		if err := tx.Model(&PullProxy{}).Pluck("id", &existing).Error; err != nil {
			return err
		}
		wrong := pullSettingError("order", fmt.Errorf("the order must name each of the %d proxies exactly once", len(existing)))
		if len(existing) != len(ids) {
			return wrong
		}
		known := make(map[uint]bool, len(existing))
		for _, id := range existing {
			known[id] = true
		}
		for _, id := range ids {
			if !known[id] {
				return wrong
			}
			delete(known, id)
		}
		for i, id := range ids {
			if err := tx.Model(&PullProxy{}).Where("id = ?", id).Update("position", i).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.pullSettingsChanged(PullSettingsProxies)
	return nil
}
