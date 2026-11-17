package store

import (
	"errors"
	"strings"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

const encryptedAIKeyPrefix = "enc:v1:"

// ErrAIDefaultRequired answers a patch that would clear the mark on the only
// service the assistant uses. The platform is fully usable without AI, so a
// service may be deleted - but while any service is stored, exactly one of them
// is the one the assistant sends to, and clearing that mark would leave the
// console showing services the assistant no longer uses with nothing saying
// which one it does.
var ErrAIDefaultRequired = errors.New("the only default AI service cannot stop being the default")

// EncryptAIKey seals a secret of an AI service for storage: its API key, and
// the custom proxy URL, which carries the proxy's login.
func EncryptAIKey(box *auth.Secretbox, key string) (string, error) {
	if key == "" {
		return "", nil
	}
	if box == nil {
		return "", errors.New("AI key encryption is not configured")
	}
	encoded, err := box.Encrypt([]byte(key))
	if err != nil {
		return "", err
	}
	return encryptedAIKeyPrefix + encoded, nil
}

func DecryptAIKey(box *auth.Secretbox, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !strings.HasPrefix(value, encryptedAIKeyPrefix) {
		// Early releases stored the key in plaintext. Keep compatibility for
		// old rows; new writes are always encrypted by the API before
		// persistence.
		return value, nil
	}
	if box == nil {
		return "", errors.New("AI key decryption is not configured")
	}
	plain, err := box.Decrypt(strings.TrimPrefix(value, encryptedAIKeyPrefix))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// CreateAIConfig stores an LLM endpoint config. If it's the first or marked
// default, it becomes the default (unsetting others).
func (s *Store) CreateAIConfig(c *AIConfig) (*AIConfig, error) {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&AIConfig{}).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			c.IsDefault = true
		}
		if c.IsDefault {
			if err := tx.Model(&AIConfig{}).Where("is_default = ?", true).Update("is_default", false).Error; err != nil {
				return err
			}
		}
		return tx.Create(c).Error
	})
	if err != nil {
		return nil, err
	}
	s.Bus.PublishChange("ai", uintKey(c.ID), "created")
	return c, nil
}

// ListAIConfigs returns all AI configs, newest first. The order is part of the
// contract: the console lists what this returns, and the default that takes over
// when one is removed is the newest remaining service, so the promoted row is
// the one the operator sees on top.
func (s *Store) ListAIConfigs() ([]AIConfig, error) {
	var cs []AIConfig
	err := s.db.Order("id desc").Find(&cs).Error
	return cs, err
}

// DefaultAIConfig returns the default AI config, or ErrNotFound if none.
//
// There is no fallback to an arbitrary row: one is the default or the assistant
// has nothing to send to. A fallback that picks a row the console does not mark
// is how an operator ends up with every question, and every seed of platform
// context, going to a provider they thought they had replaced - the row it used
// to pick was the OLDEST (id ascending) while the console lists the NEWEST
// first, so the two disagreed about which service was in use. The invariant -
// exactly one default while at least one service is stored - is kept by create,
// update and delete, by the ai_config_single_default migration that repairs a
// database written by an older build, and by that migration's partial unique
// index.
func (s *Store) DefaultAIConfig() (*AIConfig, error) {
	var c AIConfig
	err := s.db.Where("is_default = ?", true).First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// AIConfigByID reads one config, or ErrNotFound.
func (s *Store) AIConfigByID(id uint) (*AIConfig, error) {
	var c AIConfig
	if id == 0 {
		return nil, ErrNotFound
	}
	if err := s.db.First(&c, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &c, nil
}

// AIConfigPatch is a partial change to an AI config: a nil field keeps the
// stored value. APIKey is the ciphertext to store, not a plaintext key.
type AIConfigPatch struct {
	Name      *string
	Preset    *string
	BaseURL   *string
	APIKey    *string
	Model     *string
	IsDefault *bool
	// ProxyMode and ProxyURL change together: the caller clears the URL when
	// the mode leaves custom. ProxyURL is the ciphertext to store.
	ProxyMode  *string
	ProxyURL   *string
	ImageInput *bool
}

// UpdateAIConfig applies a patch and answers the row as stored. Marking one
// config the default clears the mark on the others in the same transaction, so
// at most one is ever marked; clearing the mark on the one default is refused
// with ErrAIDefaultRequired rather than leaving every stored service unused,
// because a second default is the only way to move the assistant elsewhere and
// removing a service is the way to stop using the assistant.
func (s *Store) UpdateAIConfig(id uint, patch AIConfigPatch) (*AIConfig, error) {
	var out AIConfig
	if id == 0 {
		return nil, ErrNotFound
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&out, id).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		} else if err != nil {
			return err
		}

		if patch.IsDefault != nil && !*patch.IsDefault && out.IsDefault {
			return ErrAIDefaultRequired
		}
		updates := map[string]any{}
		if patch.Name != nil {
			updates["name"] = *patch.Name
		}
		if patch.Preset != nil {
			updates["preset"] = *patch.Preset
		}
		if patch.BaseURL != nil {
			updates["base_url"] = *patch.BaseURL
		}
		if patch.APIKey != nil {
			updates["api_key"] = *patch.APIKey
		}
		if patch.Model != nil {
			updates["model"] = *patch.Model
		}
		if patch.ProxyMode != nil {
			updates["proxy_mode"] = *patch.ProxyMode
		}
		if patch.ProxyURL != nil {
			updates["proxy_url"] = *patch.ProxyURL
		}
		if patch.ImageInput != nil {
			updates["image_input"] = *patch.ImageInput
		}
		if patch.IsDefault != nil {
			if *patch.IsDefault {
				if err := tx.Model(&AIConfig{}).Where("is_default = ? AND id <> ?", true, id).Update("is_default", false).Error; err != nil {
					return err
				}
			}
			updates["is_default"] = *patch.IsDefault
		}
		if len(updates) > 0 {
			if err := tx.Model(&AIConfig{}).Where("id = ?", id).Updates(updates).Error; err != nil {
				return err
			}
		}
		return tx.First(&out, id).Error
	})
	if err != nil {
		return nil, err
	}
	s.Bus.PublishChange("ai", uintKey(id), "updated")
	return &out, nil
}

// DeleteAIConfig removes a config and answers the id that took over as the
// default, 0 when none did. A missing id is ErrNotFound: answering success for
// a row that was never there hides a stale console from the operator.
//
// Removing the default hands the mark to the newest remaining service in the
// same transaction, so the assistant is never left pointing at nothing while
// services are stored; that is the row the console lists first, and the id comes
// back so the console can say which service the assistant moved to instead of
// letting the privacy notice change host unannounced.
func (s *Store) DeleteAIConfig(id uint) (uint, error) {
	var promoted uint
	if id == 0 {
		return 0, ErrNotFound
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var row AIConfig
		if err := tx.First(&row, id).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		deleted := tx.Delete(&AIConfig{}, id)
		if deleted.Error != nil {
			return deleted.Error
		}
		if deleted.RowsAffected == 0 {
			return ErrNotFound
		}
		if !row.IsDefault {
			return nil
		}
		var survivor AIConfig
		err := tx.Order("id desc").First(&survivor).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// The last service is gone: nothing is configured, which is a
			// state the product supports.
			return nil
		}
		if err != nil {
			return err
		}
		if err := tx.Model(&AIConfig{}).Where("id = ?", survivor.ID).Update("is_default", true).Error; err != nil {
			return err
		}
		promoted = survivor.ID
		return nil
	})
	if err != nil {
		return 0, err
	}
	s.Bus.PublishChange("ai", uintKey(id), "deleted")
	if promoted != 0 {
		s.Bus.PublishChange("ai", uintKey(promoted), "updated")
	}
	return promoted, nil
}

// HasAIConfig reports whether any LLM endpoint is configured.
func (s *Store) HasAIConfig() bool {
	var count int64
	s.db.Model(&AIConfig{}).Count(&count)
	return count > 0
}
