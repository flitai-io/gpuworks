package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"
)

// A database with services stored before the proxy choice gains the columns,
// and each existing service reads management, no custom URL and no image input.
func TestAIConfigProxyAndImageInputMigration(t *testing.T) {
	m := migrationByName(t, "ai_config_proxy_and_image_input")
	rows := `INSERT INTO ai_configs (id, name, preset, base_url, api_key, model, is_default) VALUES (1, 'old', 'deepseek', 'https://api.deepseek.com', 'enc:v1:x', 'deepseek-v4-flash', 1), (2, 'other', 'custom', 'http://10.0.0.5:8000/v1', '', 'qwen', 0)`
	db := openBeforeSingleDefault(t, "ai-proxy", rows)
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"ProxyMode", "ProxyURL", "ImageInput"} {
		if !db.Migrator().HasColumn(&AIConfig{}, column) {
			t.Fatalf("ai_configs.%s missing", column)
		}
	}
	var got []AIConfig
	if err := db.Order("id").Find(&got).Error; err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("rows = %+v", got)
	}
	for _, row := range got {
		if row.ProxyMode != "management" || row.ProxyURL != "" || row.ImageInput {
			t.Fatalf("migrated row = %+v", row)
		}
	}
	if got[0].Model != "deepseek-v4-flash" || got[0].APIKey != "enc:v1:x" {
		t.Fatalf("migration touched other columns: %+v", got[0])
	}
	var marker SchemaMigration
	if err := db.First(&marker, m.Version).Error; err != nil {
		t.Fatalf("marker: %v", err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = ?", m.Version).Error; err != nil {
		t.Fatal(err)
	}
	// A service changed after the first run keeps its choice on a re-run.
	if err := db.Exec("UPDATE ai_configs SET proxy_mode = 'direct' WHERE id = 2").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}
	var mode string
	if err := db.Raw("SELECT proxy_mode FROM ai_configs WHERE id = 2").Scan(&mode).Error; err != nil || mode != "direct" {
		t.Fatalf("re-run reset a chosen mode: %q err=%v", mode, err)
	}

	rollback := openBeforeSingleDefault(t, "ai-proxy-rollback", rows)
	want := errors.New("injected failure")
	if err := applyMigration(rollback, migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasColumn(&AIConfig{}, "ProxyMode") {
		t.Fatal("the column survived the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil {
		t.Fatalf("restart after rollback: %v", err)
	}
}

// An update writes the proxy choice and the image switch, and a patch that
// names neither keeps them.
func TestUpdateAIConfigProxyAndImageInput(t *testing.T) {
	st := newTestStore(t)
	c, err := st.CreateAIConfig(&AIConfig{Name: "a", BaseURL: "https://api.deepseek.com", Model: "deepseek-flash", ProxyMode: "management"})
	if err != nil {
		t.Fatal(err)
	}
	mode, url, image := "custom", "enc:v1:proxy", true
	got, err := st.UpdateAIConfig(c.ID, AIConfigPatch{ProxyMode: &mode, ProxyURL: &url, ImageInput: &image})
	if err != nil || got.ProxyMode != "custom" || got.ProxyURL != "enc:v1:proxy" || !got.ImageInput {
		t.Fatalf("update = %+v err=%v", got, err)
	}
	name := "b"
	got, err = st.UpdateAIConfig(c.ID, AIConfigPatch{Name: &name})
	if err != nil || got.ProxyMode != "custom" || got.ProxyURL != "enc:v1:proxy" || !got.ImageInput {
		t.Fatalf("name-only update = %+v err=%v", got, err)
	}
	off := false
	got, err = st.UpdateAIConfig(c.ID, AIConfigPatch{ImageInput: &off})
	if err != nil || got.ImageInput {
		t.Fatalf("switch off = %+v err=%v", got, err)
	}
}
