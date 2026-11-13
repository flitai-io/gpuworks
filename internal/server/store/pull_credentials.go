package store

import (
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/pullref"
)

// NormalizeCredentialHost spells a host the way logins are keyed: lower case,
// no scheme, no path, Docker Hub under one name. A registry and a model site
// are keyed alike ("ghcr.io", "huggingface.co", "mirror.corp:8443").
func NormalizeCredentialHost(host string) (string, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host = strings.TrimSuffix(host, "/")
	if host == "" {
		return "", fmt.Errorf("host is empty")
	}
	if strings.ContainsAny(host, "/ \t@") || !pullref.HostLike(host) {
		return "", fmt.Errorf("%q is not a host", host)
	}
	switch host {
	case "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		host = "docker.io"
	}
	return host, nil
}

// SourceCredentials lists the stored logins without their secrets.
func (s *Store) SourceCredentials() ([]SourceCredentialView, error) {
	var rows []SourceCredential
	if err := s.db.Order("host asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]SourceCredentialView, 0, len(rows))
	for _, row := range rows {
		out = append(out, credentialView(row))
	}
	return out, nil
}

func credentialView(row SourceCredential) SourceCredentialView {
	return SourceCredentialView{ID: row.ID, Host: row.Host, Username: row.Username, HasSecret: row.SecretCipher != ""}
}

// PutSourceCredential creates (id 0) or replaces a login. username is empty
// for a model site's token. An empty secret on a replace keeps the stored one,
// so an edit that only renames the user does not need the password again.
func (s *Store) PutSourceCredential(id uint, host, username, secret string) (SourceCredentialView, error) {
	host, err := NormalizeCredentialHost(host)
	if err != nil {
		return SourceCredentialView{}, pullSettingError("host", err)
	}
	username = strings.TrimSpace(username)
	if len(username) > 255 {
		return SourceCredentialView{}, pullSettingError("username", fmt.Errorf("username is longer than 255 bytes"))
	}
	if id == 0 && secret == "" {
		return SourceCredentialView{}, pullSettingError("secret", fmt.Errorf("password or token is empty"))
	}
	var cipher string
	if secret != "" {
		if s.secretbox == nil {
			return SourceCredentialView{}, fmt.Errorf("credentials need the server's secret key")
		}
		cipher, err = s.secretbox.Encrypt([]byte(secret))
		if err != nil {
			return SourceCredentialView{}, err
		}
		if len(cipher) > 2048 {
			return SourceCredentialView{}, pullSettingError("secret", fmt.Errorf("password or token is too long"))
		}
	}
	var row SourceCredential
	err = s.db.Transaction(func(tx *gorm.DB) error {
		var clash SourceCredential
		q := tx.Where("host = ?", host)
		if id != 0 {
			q = q.Where("id <> ?", id)
		}
		if err := q.First(&clash).Error; err == nil {
			return pullSettingError("host", fmt.Errorf("a login for %s already exists", host))
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if id != 0 {
			if err := tx.First(&row, id).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrNotFound
				}
				return err
			}
		}
		row.Host, row.Username = host, username
		if cipher != "" {
			row.SecretCipher = cipher
		}
		return tx.Save(&row).Error
	})
	if err != nil {
		return SourceCredentialView{}, err
	}
	s.pullSettingsChanged(PullSettingsCredentials)
	return credentialView(row), nil
}

// DeleteSourceCredential removes a login.
func (s *Store) DeleteSourceCredential(id uint) error {
	result := s.db.Delete(&SourceCredential{}, id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	s.pullSettingsChanged(PullSettingsCredentials)
	return nil
}

// SourceAuths returns every stored login with its secret decrypted, for the
// desired state a node is sent and the management node's own requests. A
// login whose secret cannot be decrypted (the server's key changed) is left
// out rather than sent wrong.
func (s *Store) SourceAuths() ([]proto.SourceAuth, error) {
	var rows []SourceCredential
	if err := s.db.Order("host asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 || s.secretbox == nil {
		return nil, nil
	}
	out := make([]proto.SourceAuth, 0, len(rows))
	for _, row := range rows {
		raw, err := s.secretbox.Decrypt(row.SecretCipher)
		if err != nil {
			continue
		}
		out = append(out, proto.SourceAuth{Host: row.Host, Username: row.Username, Secret: string(raw)})
	}
	return out, nil
}

// SourceAuthFor returns the decrypted login for one credential, for testing
// it against its host.
func (s *Store) SourceAuthFor(id uint) (proto.SourceAuth, error) {
	var row SourceCredential
	if err := s.db.First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return proto.SourceAuth{}, ErrNotFound
		}
		return proto.SourceAuth{}, err
	}
	if s.secretbox == nil {
		return proto.SourceAuth{}, fmt.Errorf("credentials need the server's secret key")
	}
	raw, err := s.secretbox.Decrypt(row.SecretCipher)
	if err != nil {
		return proto.SourceAuth{}, fmt.Errorf("decrypt the login for %s: %w", row.Host, err)
	}
	return proto.SourceAuth{Host: row.Host, Username: row.Username, Secret: string(raw)}, nil
}
