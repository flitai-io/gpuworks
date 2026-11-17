package store

import "gorm.io/gorm"

// addAIConfigProxyAndImageInput adds how an AI service's calls leave the server
// and whether its model accepts images. Existing rows read management, the
// default: until now every service went out through the server process's proxy
// environment, which is the management route while no management node proxy is
// chosen; where one is chosen the assistant now uses it, as image catalog and
// metadata requests already do. No service was ever sent an image, so none is
// marked as accepting one.
func addAIConfigProxyAndImageInput(db *gorm.DB) error {
	if !db.Migrator().HasTable(&AIConfig{}) {
		return nil
	}
	for _, column := range []string{"ProxyMode", "ProxyURL", "ImageInput"} {
		if db.Migrator().HasColumn(&AIConfig{}, column) {
			continue
		}
		if err := db.Migrator().AddColumn(&AIConfig{}, column); err != nil {
			return err
		}
	}
	return db.Exec("UPDATE ai_configs SET proxy_mode = 'management' WHERE proxy_mode IS NULL OR proxy_mode = ''").Error
}
