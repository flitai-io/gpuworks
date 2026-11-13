package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
)

func openPullMigrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.AutoMigrate(&SchemaMigration{}, &Setting{}, &Node{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func countMarkers(t *testing.T, db *gorm.DB, name string) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&SchemaMigration{}).Where("name = ?", name).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPullSettingsTablesMigrationSchemaMarkerAndIdempotence(t *testing.T) {
	db := openPullMigrationDB(t)
	m := migrationByName(t, "pull_settings_tables")
	for i := 0; i < 2; i++ {
		if err := migrateDB(db, []migration{m}); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []any{&PullSourceRule{}, &PullSourceRuleEndpoint{}, &PullSourceRuleRequirement{}, &SyncGroup{}, &SyncGroupRequirement{}, &registryCredentialTable{}} {
		if !db.Migrator().HasTable(table) {
			t.Fatalf("missing table for %T", table)
		}
	}
	if !db.Migrator().HasIndex(&registryCredentialTable{}, "Registry") {
		t.Fatal("registry index missing")
	}
	var groups []SyncGroup
	db.Find(&groups)
	if len(groups) != 1 || groups[0].Name != DefaultSyncGroupName || groups[0].RateLimitKiB != 0 {
		t.Fatalf("groups = %#v, want exactly the seeded all-nodes group", groups)
	}
	var reqs int64
	db.Model(&SyncGroupRequirement{}).Count(&reqs)
	if reqs != 0 {
		t.Fatalf("the seeded group selects every node, not %d requirements", reqs)
	}
	if countMarkers(t, db, m.Name) != 1 {
		t.Fatal("marker not recorded exactly once")
	}
	if err := db.Create(&registryCredentialTable{Registry: "a.example"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&registryCredentialTable{Registry: "a.example"}).Error; err == nil {
		t.Fatal("registry not unique")
	}
	// Re-running the body itself (a database restored to before the marker)
	// does not seed a second group.
	if err := migratePullSettingsTables(db); err != nil {
		t.Fatal(err)
	}
	var all int64
	db.Model(&SyncGroup{}).Count(&all)
	if all != 1 {
		t.Fatalf("groups after a re-run = %d", all)
	}
}

func TestPullSettingsTablesMigrationRollsBack(t *testing.T) {
	db := openPullMigrationDB(t)
	base := migrationByName(t, "pull_settings_tables")
	want := errors.New("injected pull settings failure")
	failure := migration{Version: base.Version, Name: base.Name, Apply: func(tx *gorm.DB) error {
		if err := base.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(db, failure); !errors.Is(err, want) {
		t.Fatalf("err = %v", err)
	}
	if db.Migrator().HasTable(&SyncGroup{}) || db.Migrator().HasTable(&PullSourceRule{}) {
		t.Fatal("failed migration left tables behind")
	}
	if countMarkers(t, db, base.Name) != 0 {
		t.Fatal("failed migration left its marker")
	}
	if err := migrateDB(db, []migration{base}); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

// shippedRewritesJSON is what image_registry_rewrites was seeded with before
// pull source rules replaced it.
const shippedRewritesJSON = `{"default":"quay","rules":[` +
	`{"prefix":"gpuworks/gpuworks-runner","mirrors":["quay.io/gpuworks/gpuworks-runner","swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-runner","registry.zstack.ai/gpuworks/gpuworks-runner"]},` +
	`{"prefix":"gpustack/","mirrors":["quay.io/gpustack","swr.cn-south-1.myhuaweicloud.com/gpustack"]}]}`

func setSetting(t *testing.T, db *gorm.DB, key SettingKey, value string) {
	t.Helper()
	if err := db.Save(&Setting{Key: string(key), Value: value}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestPullSourceRulesMigrationConvertsRewrites(t *testing.T) {
	db := openPullMigrationDB(t)
	if err := migrateDB(db, []migration{migrationByName(t, "pull_settings_tables")}); err != nil {
		t.Fatal(err)
	}
	node := Node{UUID: "node-a", TokenHash: "h", Hostname: "a"}
	if err := db.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	// The shipped default plus an operator's own rule.
	var cfg map[string]any
	_ = json.Unmarshal([]byte(shippedRewritesJSON), &cfg)
	cfg["rules"] = append(cfg["rules"].([]any), map[string]any{"prefix": "vllm/", "mirrors": []string{"mirror.corp.example/vllm"}})
	raw, _ := json.Marshal(cfg)
	setSetting(t, db, SettingImageRegistryRewrites, string(raw))
	setSetting(t, db, SettingImageRegistryMirrors, `{"default":"quay","sources":{"quay":{"prefix":"quay.io/gpustack"}}}`)
	global, _ := json.Marshal(proto.NetworkConfig{HTTPSProxy: "http://p:3128", Rewrites: []proto.URLRewrite{
		{From: "https://huggingface.co", To: "https://hf-mirror.com"},
		{From: "docker.io", To: "docker.m.daocloud.io"},
	}})
	setSetting(t, db, SettingNetwork, string(global))
	override, _ := json.Marshal(proto.NetworkConfig{Rewrites: []proto.URLRewrite{{From: "ghcr.io", To: "ghcr.nju.edu.cn"}}})
	setSetting(t, db, NodeNetworkSettingKey(node.ID), string(override))
	// An override for a node that no longer exists is not a rule for anyone.
	orphan, _ := json.Marshal(proto.NetworkConfig{Rewrites: []proto.URLRewrite{{From: "quay.io", To: "quay.example"}}})
	setSetting(t, db, NodeNetworkSettingKey(9999), string(orphan))

	m := migrationByName(t, "pull_source_rules_from_rewrites")
	for i := 0; i < 2; i++ {
		if err := migrateDB(db, []migration{m}); err != nil {
			t.Fatal(err)
		}
	}
	rules, err := readPullSourceRules(db)
	if err != nil {
		t.Fatal(err)
	}
	type got struct {
		Match     string
		Endpoints []string
		Upstream  bool
		Reqs      nodelabel.Requirements
	}
	var have []got
	for _, rule := range rules {
		have = append(have, got{rule.Match, rule.Endpoints, rule.AllowUpstream, rule.Requirements})
	}
	want := []got{
		{"ghcr.io", []string{"ghcr.nju.edu.cn"}, true, nodelabel.Requirements{nodelabel.KeyNode: {"node-a"}}},
		{"docker.io", []string{"docker.m.daocloud.io"}, true, nil},
		{"vllm/", []string{"mirror.corp.example/vllm"}, true, nil},
		{"gpuworks/", []string{"registry.zstack.ai/gpuworks/"}, true, nil},
	}
	if !reflect.DeepEqual(have, want) {
		t.Fatalf("rules =\n%#v\nwant\n%#v", have, want)
	}
	for _, key := range []SettingKey{SettingImageRegistryRewrites, SettingImageRegistryMirrors} {
		if v, _ := migrationSettingValue(db, key); v != "" {
			t.Fatalf("%s still set: %s", key, v)
		}
	}
	// The URL rewrite and the proxy stay in the network setting; the image
	// rewrite moved out of it.
	var after proto.NetworkConfig
	v, _ := migrationSettingValue(db, SettingNetwork)
	_ = json.Unmarshal([]byte(v), &after)
	if after.HTTPSProxy != "http://p:3128" || len(after.Rewrites) != 1 || after.Rewrites[0].From != "https://huggingface.co" {
		t.Fatalf("network after = %#v", after)
	}
	v, _ = migrationSettingValue(db, NodeNetworkSettingKey(node.ID))
	if strings.Contains(v, "ghcr") {
		t.Fatalf("node override still rewrites images: %s", v)
	}
	if countMarkers(t, db, m.Name) != 1 {
		t.Fatal("marker not recorded exactly once")
	}
}

func TestPullSourceRulesMigrationOnAFreshInstallationAddsNothing(t *testing.T) {
	db := openPullMigrationDB(t)
	if err := migrateDB(db, []migration{migrationByName(t, "pull_settings_tables"), migrationByName(t, "pull_source_rules_from_rewrites")}); err != nil {
		t.Fatal(err)
	}
	var n int64
	db.Model(&PullSourceRule{}).Count(&n)
	if n != 0 {
		t.Fatalf("fresh installation got %d rules", n)
	}
}

func TestPullSourceRulesMigrationWithoutTheInternalRegistry(t *testing.T) {
	db := openPullMigrationDB(t)
	if err := migrateDB(db, []migration{migrationByName(t, "pull_settings_tables")}); err != nil {
		t.Fatal(err)
	}
	// An operator who had removed the internal registry from the shipped rule
	// does not get it back.
	setSetting(t, db, SettingImageRegistryRewrites, `{"default":"quay","rules":[{"prefix":"gpuworks/gpuworks-runner","mirrors":["quay.io/gpuworks/gpuworks-runner"]}]}`)
	if err := migrateDB(db, []migration{migrationByName(t, "pull_source_rules_from_rewrites")}); err != nil {
		t.Fatal(err)
	}
	var n int64
	db.Model(&PullSourceRule{}).Count(&n)
	if n != 0 {
		t.Fatalf("got %d rules, want none", n)
	}
}

func TestPullSourceRulesMigrationRollsBack(t *testing.T) {
	db := openPullMigrationDB(t)
	if err := migrateDB(db, []migration{migrationByName(t, "pull_settings_tables")}); err != nil {
		t.Fatal(err)
	}
	setSetting(t, db, SettingImageRegistryRewrites, shippedRewritesJSON)
	base := migrationByName(t, "pull_source_rules_from_rewrites")
	want := errors.New("injected rules failure")
	failure := migration{Version: base.Version, Name: base.Name, Apply: func(tx *gorm.DB) error {
		if err := base.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(db, failure); !errors.Is(err, want) {
		t.Fatalf("err = %v", err)
	}
	var n int64
	db.Model(&PullSourceRule{}).Count(&n)
	if v, _ := migrationSettingValue(db, SettingImageRegistryRewrites); n != 0 || v == "" || countMarkers(t, db, base.Name) != 0 {
		t.Fatalf("rollback left rules=%d setting=%q", n, v)
	}
	if err := migrateDB(db, []migration{base}); err != nil {
		t.Fatal(err)
	}
	db.Model(&PullSourceRule{}).Count(&n)
	if n != 1 {
		t.Fatalf("retry produced %d rules, want the internal registry rule", n)
	}
}

func TestDropAcquisitionChoicesKeepsTheRestOfTheSnapshot(t *testing.T) {
	db := openPullMigrationDB(t)
	if err := db.AutoMigrate(&Deployment{}); err != nil {
		t.Fatal(err)
	}
	with := `{"acquisition":{"image_pull":{"kind":"public","provider":"quay.io/x:1"},"model_source":{"kind":"peer_sync","source_node":"n1"}},"image":"gpuworks/x:1","image_sources":["ghcr.io/x:1"],"spec_id":"s"}`
	without := `{"image":"gpuworks/y:1","spec_id":"t"}`
	broken := `{"acquisition":`
	for i, snapshot := range []string{with, without, broken} {
		if err := db.Create(&Deployment{Name: "d" + string(rune('a'+i)), CatalogSelectionSnapshot: snapshot}).Error; err != nil {
			t.Fatal(err)
		}
	}
	m := migrationByName(t, "deployment_acquisition_choices_dropped")
	for i := 0; i < 2; i++ {
		if err := migrateDB(db, []migration{m}); err != nil {
			t.Fatal(err)
		}
	}
	var rows []Deployment
	db.Order("id").Find(&rows)
	if rows[0].CatalogSelectionSnapshot != `{"image":"gpuworks/x:1","image_sources":["ghcr.io/x:1"],"spec_id":"s"}` {
		t.Fatalf("snapshot = %s", rows[0].CatalogSelectionSnapshot)
	}
	if rows[1].CatalogSelectionSnapshot != without || rows[2].CatalogSelectionSnapshot != broken {
		t.Fatalf("untouched snapshots changed: %q %q", rows[1].CatalogSelectionSnapshot, rows[2].CatalogSelectionSnapshot)
	}
	// Rollback: an injected failure leaves the choices in place.
	db2 := openPullMigrationDB(t)
	_ = db2.AutoMigrate(&Deployment{})
	db2.Create(&Deployment{Name: "x", CatalogSelectionSnapshot: with})
	want := errors.New("injected")
	failure := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(db2, failure); !errors.Is(err, want) {
		t.Fatalf("err = %v", err)
	}
	var row Deployment
	db2.First(&row)
	if row.CatalogSelectionSnapshot != with || countMarkers(t, db2, m.Name) != 0 {
		t.Fatalf("rollback left %s", row.CatalogSelectionSnapshot)
	}
}

// The registry logins move to source_credentials keyed by host, with their
// ids and ciphertexts, and the old table goes. Applying it again changes
// nothing; a failure leaves the old table as it was.
func TestSourceCredentialsMigrationMovesTheLogins(t *testing.T) {
	db := openPullMigrationDB(t)
	if err := db.AutoMigrate(&registryCredentialTable{}); err != nil {
		t.Fatal(err)
	}
	for _, row := range []registryCredentialTable{{ID: 3, Registry: "ghcr.io", Username: "bot", SecretCipher: "cipher-a"}, {ID: 7, Registry: "docker.io", Username: "me", SecretCipher: "cipher-b"}} {
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	m := migrationByName(t, "source_credentials")
	if err := db.Exec(`CREATE TRIGGER fail_credentials_marker BEFORE INSERT ON schema_migrations WHEN NEW.name='source_credentials' BEGIN SELECT RAISE(ABORT, 'injected'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err == nil {
		t.Fatal("injected failure did not fail the migration")
	}
	if !db.Migrator().HasTable(&registryCredentialTable{}) || db.Migrator().HasTable(&SourceCredential{}) {
		t.Fatal("a failed migration changed the tables")
	}
	if err := db.Exec(`DROP TRIGGER fail_credentials_marker`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}
	var rows []SourceCredential
	db.Order("id").Find(&rows)
	if len(rows) != 2 || rows[0].ID != 3 || rows[0].Host != "ghcr.io" || rows[0].Username != "bot" || rows[0].SecretCipher != "cipher-a" || rows[1].Host != "docker.io" {
		t.Fatalf("source credentials = %+v", rows)
	}
	if db.Migrator().HasTable("registry_credentials") {
		t.Fatal("the old table survived")
	}
	if !db.Migrator().HasIndex(&SourceCredential{}, "Host") {
		t.Fatal("the host index is missing")
	}
	if countMarkers(t, db, "source_credentials") != 1 {
		t.Fatal("marker missing or duplicated")
	}
}
