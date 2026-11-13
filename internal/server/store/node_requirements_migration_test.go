package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/nodelabel"
)

// Migration 120 moves where a deployment may run out of its placement
// document into the requirement table, and every deployment keeps exactly the
// nodes it could run on before.
func TestMoveNodeRequirementsOutOfPlacementMigration(t *testing.T) {
	type row struct {
		nodeID    uint
		nodeIDs   any
		nnodes    int
		placement any
	}
	deployments := map[int]row{
		// A manual single-node deployment from an old client, with a selector.
		1: {1, `[1]`, 1, `{"mode":"manual","node_uuid":"n-1","node_labels":{"pool":"a","team":""},"tp":2,"gpus":[0,1]}`},
		// An automatic one: nothing recorded that, so it is pinned too.
		2: {2, `[2]`, 1, `{"mode":"auto","tp":1}`},
		// A multi-node replica list keeps its nested node_uuid and its rank
		// order; it gets no node requirement.
		3: {1, `[1,2]`, 2, `{"tp":2,"pp":1,"node_labels":{"pool":"a"},"units":[{"node_uuid":"n-1","followers":[{"node_uuid":"n-2"}]}]}`},
		// PD names its nodes per role.
		4: {2, `[2,1]`, 1, `{"pd":{"mode":"mooncake","roles":{"prefill":{"units":[{"node_uuid":"n-2"}]}}}}`},
		// A selector naming a key migration 119 deleted, and one that is not
		// a string map, stay in the document, which then no longer parses.
		5: {1, `[1]`, 1, `{"node_labels":{"gpuworks.ai/pool":"a"}}`},
		6: {1, `[1]`, 1, `{"node_labels":["pool"]}`},
		// A node that has since been deleted, and an unreadable node list
		// falling back to the primary node.
		7: {9, `[9]`, 1, `{}`},
		8: {2, `not json`, 1, nil},
		// A document that does not decode is left alone entirely.
		9: {1, `[1]`, 1, `not json`},
		// A node that reports no vendor gives no vendor requirement.
		10: {3, `[3]`, 1, `null`},
	}
	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		for _, stmt := range []string{
			// The deployments table as it stood before this migration: no
			// placement_strategy column.
			"CREATE TABLE deployments (id INTEGER PRIMARY KEY, node_id INTEGER, node_ids TEXT, n_nodes INTEGER, placement TEXT)",
			"CREATE TABLE nodes (id INTEGER PRIMARY KEY, uuid TEXT, gpu_vendor TEXT)",
			"INSERT INTO nodes (id, uuid, gpu_vendor) VALUES (1, 'n-1', 'NVIDIA'), (2, 'n-2', 'ascend'), (3, 'n-3', '')",
		} {
			if err := db.Exec(stmt).Error; err != nil {
				t.Fatal(err)
			}
		}
		for id, d := range deployments {
			if err := db.Exec("INSERT INTO deployments (id, node_id, node_ids, n_nodes, placement) VALUES (?, ?, ?, ?, ?)",
				id, d.nodeID, d.nodeIDs, d.nnodes, d.placement).Error; err != nil {
				t.Fatal(err)
			}
		}
		return db
	}
	placement := func(db *gorm.DB, id int) string {
		t.Helper()
		var raw *string
		if err := db.Raw("SELECT placement FROM deployments WHERE id = ?", id).Scan(&raw).Error; err != nil {
			t.Fatal(err)
		}
		if raw == nil {
			return "<null>"
		}
		return *raw
	}
	requirements := func(db *gorm.DB) map[uint]nodelabel.Requirements {
		t.Helper()
		var rows []DeploymentNodeRequirement
		if err := db.Order("deployment_id, key, value").Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		out := map[uint]nodelabel.Requirements{}
		for _, r := range rows {
			if out[r.DeploymentID] == nil {
				out[r.DeploymentID] = nodelabel.Requirements{}
			}
			out[r.DeploymentID][r.Key] = append(out[r.DeploymentID][r.Key], r.Value)
		}
		return out
	}
	sameJSON := func(got, want string) bool {
		var g, w any
		if err := json.Unmarshal([]byte(want), &w); err != nil {
			t.Fatalf("want %s: %v", want, err)
		}
		return json.Unmarshal([]byte(got), &g) == nil && reflect.DeepEqual(g, w)
	}
	check := func(t *testing.T, db *gorm.DB) {
		t.Helper()
		vendor := func(v string) []string { return []string{v} }
		want := map[uint]nodelabel.Requirements{
			1:  {"pool": {"a"}, "team": {""}, nodelabel.KeyVendor: vendor("nvidia"), nodelabel.KeyNode: {"n-1"}},
			2:  {nodelabel.KeyVendor: vendor("ascend"), nodelabel.KeyNode: {"n-2"}},
			3:  {"pool": {"a"}, nodelabel.KeyVendor: vendor("nvidia")},
			4:  {nodelabel.KeyVendor: vendor("ascend")},
			5:  {nodelabel.KeyVendor: vendor("nvidia"), nodelabel.KeyNode: {"n-1"}},
			6:  {nodelabel.KeyVendor: vendor("nvidia"), nodelabel.KeyNode: {"n-1"}},
			7:  {nodelabel.KeyNode: {"#9"}},
			8:  {nodelabel.KeyVendor: vendor("ascend"), nodelabel.KeyNode: {"n-2"}},
			9:  {nodelabel.KeyVendor: vendor("nvidia")},
			10: {nodelabel.KeyNode: {"n-3"}},
		}
		if got := requirements(db); !reflect.DeepEqual(got, want) {
			t.Errorf("requirements =\n%v\nwant\n%v", got, want)
		}
		for id, want := range map[int]string{
			1: `{"tp":2,"gpus":[0,1]}`,
			2: `{"tp":1}`,
			3: `{"tp":2,"pp":1,"units":[{"node_uuid":"n-1","followers":[{"node_uuid":"n-2"}]}]}`,
		} {
			if got := placement(db, id); !sameJSON(got, want) {
				t.Errorf("deployment %d placement = %s\nwant %s", id, got, want)
			}
		}
		for id, want := range map[int]string{
			4:  `{"pd":{"mode":"mooncake","roles":{"prefill":{"units":[{"node_uuid":"n-2"}]}}}}`,
			5:  `{"node_labels":{"gpuworks.ai/pool":"a"}}`,
			6:  `{"node_labels":["pool"]}`,
			7:  `{}`,
			8:  "<null>",
			9:  "not json",
			10: "null",
		} {
			if got := placement(db, id); got != want {
				t.Errorf("deployment %d placement = %s, want it unchanged: %s", id, got, want)
			}
		}
		var strategies []string
		if err := db.Raw("SELECT DISTINCT placement_strategy FROM deployments").Scan(&strategies).Error; err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(strategies, []string{"spread"}) {
			t.Errorf("strategies = %v, want every existing deployment spread", strategies)
		}
	}

	m := migrationByVersion(t, 120)
	if m.Name != "deployment_node_requirements" {
		t.Fatalf("migration 120 = %q", m.Name)
	}
	db := open("node-requirements")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	check(t, db)
	var marker SchemaMigration
	if err := db.First(&marker, 120).Error; err != nil || marker.Name != m.Name {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	check(t, db)

	// A failure after the rewrites rolls back the new table, the new column,
	// the moved rows and the marker together.
	rollback := open("node-requirements-rollback")
	injected := errors.New("injected requirement migration failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return injected
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, injected) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasTable(&DeploymentNodeRequirement{}) {
		t.Fatal("requirement table survived the rollback")
	}
	if rollback.Migrator().HasColumn(&Deployment{}, "PlacementStrategy") {
		t.Fatal("placement_strategy survived the rollback")
	}
	if got := placement(rollback, 1); got != deployments[1].placement {
		t.Fatalf("placement after the failed migration = %s", got)
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 120).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil {
		t.Fatalf("restart after rollback: %v", err)
	}
	check(t, rollback)
}

// A database that has no deployments table yet still gets the requirement
// table, and the migration does not fail.
func TestMoveNodeRequirementsWithoutDeployments(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:node-requirements-empty?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := moveNodeRequirementsOutOfPlacement(db); err != nil {
		t.Fatalf("empty database: %v", err)
	}
	if !db.Migrator().HasTable(&DeploymentNodeRequirement{}) {
		t.Fatal("no requirement table")
	}
}

// The same move against the real schema every earlier migration leaves
// behind, so a column this migration reads by name cannot drift from the one
// the entity maps - a hand-written table above would not notice.
func TestMoveNodeRequirementsOnTheRealSchema(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:node-requirements-real?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var before []migration
	var m migration
	for _, candidate := range migrations {
		switch {
		case candidate.Version < 120:
			before = append(before, candidate)
		case candidate.Version == 120:
			m = candidate
		}
	}
	if err := migrateDB(db, before); err != nil {
		t.Fatal(err)
	}
	if db.Migrator().HasTable(&DeploymentNodeRequirement{}) {
		t.Fatal("requirement table exists before migration 120")
	}
	for _, stmt := range []string{
		"INSERT INTO nodes (id, uuid, gpu_vendor) VALUES (1, 'n-1', 'nvidia'), (2, 'n-2', 'nvidia')",
		`INSERT INTO deployments (id, name, node_id, node_ids, n_nodes, placement) VALUES
			(1, 'single', 1, '[1]', 1, '{"mode":"manual","node_labels":{"pool":"a"},"tp":1}'),
			(2, 'multi', 1, '[1,2]', 2, '{"tp":2}')`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	st := &Store{db: db}
	got, err := st.DeploymentRequirementsFor([]uint{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint]nodelabel.Requirements{
		1: {"pool": {"a"}, nodelabel.KeyVendor: {"nvidia"}, nodelabel.KeyNode: {"n-1"}},
		2: {nodelabel.KeyVendor: {"nvidia"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requirements = %v, want %v", got, want)
	}
	var dep Deployment
	if err := db.First(&dep, 1).Error; err != nil {
		t.Fatal(err)
	}
	if dep.PlacementStrategy != PlacementSpread || dep.Placement != `{"tp":1}` {
		t.Fatalf("deployment = strategy %q placement %s", dep.PlacementStrategy, dep.Placement)
	}
}
