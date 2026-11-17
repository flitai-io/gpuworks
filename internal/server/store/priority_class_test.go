package store

import (
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/priority"
)

func TestPriorityClassesMigration(t *testing.T) {
	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("CREATE TABLE api_keys (id INTEGER PRIMARY KEY, name TEXT, prefix TEXT, revoked_at DATETIME)").Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("INSERT INTO api_keys (id, name, prefix) VALUES (1, 'old', 'gw-old')").Error; err != nil {
			t.Fatal(err)
		}
		return db
	}
	m := migrationByVersion(t, 214)
	db := open("priority-classes")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	var rows []PriorityClass
	if err := db.Order("value desc").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(priority.Builtins) {
		t.Fatalf("seeded = %+v", rows)
	}
	for i, b := range priority.Builtins {
		if rows[i].Name != b.Name || rows[i].Value != b.Value || !rows[i].Builtin || rows[i].IsDefault != (b.Name == priority.Normal) {
			t.Fatalf("row %d = %+v", i, rows[i])
		}
	}
	// A key from before stays unbound, with the default policy.
	var key struct {
		PriorityClassID        *uint
		PriorityCeilingClassID *uint
		PriorityPolicy         string
	}
	if err := db.Raw("SELECT priority_class_id, priority_ceiling_class_id, priority_policy FROM api_keys WHERE id = 1").Scan(&key).Error; err != nil ||
		key.PriorityClassID != nil || key.PriorityCeilingClassID != nil || key.PriorityPolicy != "" {
		t.Fatalf("old key = %+v err=%v", key, err)
	}
	for _, col := range []string{"PriorityClassID", "PriorityCeilingClassID"} {
		if !db.Migrator().HasIndex(&APIKey{}, col) {
			t.Fatalf("index on %s missing", col)
		}
	}
	var marker SchemaMigration
	if err := db.First(&marker, 214).Error; err != nil || marker.Name != "priority_classes" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	// Running again (without the marker) seeds nothing twice.
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 214").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	var count int64
	if db.Model(&PriorityClass{}).Count(&count); count != int64(len(priority.Builtins)) {
		t.Fatalf("rerun seeded %d rows", count)
	}

	rollback := open("priority-classes-rollback")
	want := errors.New("injected priority class failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasTable(&PriorityClass{}) || rollback.Migrator().HasColumn(&APIKey{}, "PriorityClassID") {
		t.Fatal("the failed migration left schema behind")
	}
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 214).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil || !rollback.Migrator().HasTable(&PriorityClass{}) {
		t.Fatalf("restart after rollback: err=%v", err)
	}
}

func TestPriorityClassLifecycle(t *testing.T) {
	st := newTestStore(t)
	sub := st.Bus.Subscribe(16, priorityClassEvent)
	defer sub.Close()

	gold := &PriorityClass{Name: "gold", DisplayName: " Gold ", Value: 30}
	if err := st.CreatePriorityClass(gold); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePriorityClass(&PriorityClass{Name: "gold", Value: 1}); !errors.Is(err, ErrPriorityClassExists) {
		t.Fatalf("duplicate = %v", err)
	}
	for _, bad := range []PriorityClass{{Name: "Gold", Value: 1}, {Name: "x", Value: 101}, {Name: "", Value: 0}} {
		if err := st.CreatePriorityClass(&bad); !errors.Is(err, ErrPriorityClassInvalid) {
			t.Fatalf("%+v = %v", bad, err)
		}
	}
	select {
	case <-sub.C():
	default:
		t.Fatal("creating a class published nothing")
	}

	// Built-in classes keep their values; display names may change.
	v := 5
	if _, err := st.UpdatePriorityClass(priority.High, PriorityClassUpdate{Value: &v}); !errors.Is(err, ErrPriorityClassBuiltin) {
		t.Fatalf("builtin value = %v", err)
	}
	name := "高"
	if got, err := st.UpdatePriorityClass(priority.High, PriorityClassUpdate{DisplayName: &name}); err != nil || got.DisplayName != "高" || got.Value != 20 {
		t.Fatalf("builtin rename = %+v %v", got, err)
	}
	v = 35
	if got, err := st.UpdatePriorityClass("gold", PriorityClassUpdate{Value: &v}); err != nil || got.Value != 35 || got.DisplayName != "Gold" {
		t.Fatalf("custom edit = %+v %v", got, err)
	}
	if _, err := st.UpdatePriorityClass("nope", PriorityClassUpdate{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown = %v", err)
	}

	// A key bound to gold reads its binding back through the gateway's
	// resolution, and the binding takes effect without waiting out a cache.
	full, err := st.CreateAPIKey("k", 0)
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := st.ResolveAPIKey(full)
	if !ok || identity.Priority.ClassID != 0 || identity.Priority.Policy != priority.DefaultPolicy {
		t.Fatalf("unbound identity = %+v", identity)
	}
	keys, _ := st.ListAPIKeys()
	prefix := keys[0].Prefix
	if err := st.SetAPIKeyPriority(prefix, APIKeyPriority{Class: priority.Normal, Ceiling: "gold", Policy: priority.PolicyWithinCeiling}); err != nil {
		t.Fatal(err)
	}
	identity, _ = st.ResolveAPIKey(full)
	table, err := st.PriorityClassTable()
	if err != nil {
		t.Fatal(err)
	}
	normal, _ := table.ByName(priority.Normal)
	goldClass, _ := table.ByName("gold")
	if identity.Priority.ClassID != normal.ID || identity.Priority.CeilingID != goldClass.ID || identity.Priority.Policy != priority.PolicyWithinCeiling {
		t.Fatalf("bound identity = %+v", identity.Priority)
	}
	// Only the within-ceiling policy keeps a ceiling.
	if err := st.SetAPIKeyPriority(prefix, APIKeyPriority{Class: priority.Normal, Ceiling: "gold", Policy: priority.PolicyFixed}); err != nil {
		t.Fatal(err)
	}
	if identity, _ = st.ResolveAPIKey(full); identity.Priority.CeilingID != 0 || identity.Priority.Policy != priority.PolicyFixed {
		t.Fatalf("fixed binding = %+v", identity.Priority)
	}
	// A ceiling below the key's own class (or the default's) is refused.
	if err := st.SetAPIKeyPriority(prefix, APIKeyPriority{Class: priority.High, Ceiling: priority.Low}); !errors.Is(err, ErrPriorityClassInvalid) {
		t.Fatalf("low ceiling = %v", err)
	}
	if err := st.SetAPIKeyPriority(prefix, APIKeyPriority{Ceiling: priority.Low}); !errors.Is(err, ErrPriorityClassInvalid) {
		t.Fatalf("ceiling below the default = %v", err)
	}
	if err := st.SetAPIKeyPriority(prefix, APIKeyPriority{Class: priority.Normal, Ceiling: "gold", Policy: priority.PolicyWithinCeiling}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAPIKeyPriority(prefix, APIKeyPriority{Class: "platinum"}); !errors.Is(err, ErrPriorityClassInvalid) {
		t.Fatalf("unknown class = %v", err)
	}
	if err := st.SetAPIKeyPriority("gw-none", APIKeyPriority{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown key = %v", err)
	}

	// A class a live key is bound to, the default and built-ins stay.
	if err := st.DeletePriorityClass("gold"); !errors.Is(err, ErrPriorityClassInUse) {
		t.Fatalf("bound class delete = %v", err)
	}
	if err := st.DeletePriorityClass(priority.Low); !errors.Is(err, ErrPriorityClassBuiltin) {
		t.Fatalf("builtin delete = %v", err)
	}
	if err := st.SetDefaultPriorityClass("gold"); err != nil {
		t.Fatal(err)
	}
	if table, _ = st.PriorityClassTable(); table.Default.Name != "gold" {
		t.Fatalf("default = %+v", table.Default)
	}
	if err := st.SetAPIKeyPriority(prefix, APIKeyPriority{}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeletePriorityClass("gold"); !errors.Is(err, ErrPriorityClassInUse) {
		t.Fatalf("default class delete = %v", err)
	}
	if err := st.SetDefaultPriorityClass(priority.Normal); err != nil {
		t.Fatal(err)
	}
	if err := st.DeletePriorityClass("gold"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDefaultPriorityClass("gold"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("default to a deleted class = %v", err)
	}
	rows, _ := st.PriorityClasses()
	defaults := 0
	for _, r := range rows {
		if r.IsDefault {
			defaults++
		}
	}
	if len(rows) != len(priority.Builtins) || defaults != 1 {
		t.Fatalf("classes = %+v", rows)
	}
}

// A class a live key names, as its class or its ceiling, is bound; a revoked
// key binds nothing.
func TestPriorityClassesBound(t *testing.T) {
	st := newTestStore(t)
	for _, c := range []*PriorityClass{{Name: "gold", Value: 30}, {Name: "silver", Value: 10}, {Name: "bronze", Value: 5}} {
		if err := st.CreatePriorityClass(c); err != nil {
			t.Fatal(err)
		}
	}
	prefixOf := func(name string) string {
		t.Helper()
		if _, err := st.CreateAPIKey(name, 0); err != nil {
			t.Fatal(err)
		}
		keys, err := st.ListAPIKeys()
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range keys {
			if k.Name == name {
				return k.Prefix
			}
		}
		t.Fatalf("no key %s", name)
		return ""
	}
	classed, ceilinged, revoked := prefixOf("classed"), prefixOf("ceilinged"), prefixOf("revoked")
	if err := st.SetAPIKeyPriority(classed, APIKeyPriority{Class: "silver", Policy: priority.PolicyFixed}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAPIKeyPriority(ceilinged, APIKeyPriority{Ceiling: "gold", Policy: priority.PolicyWithinCeiling}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAPIKeyPriority(revoked, APIKeyPriority{Class: "bronze", Policy: priority.PolicyFixed}); err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeAPIKey(revoked); err != nil {
		t.Fatal(err)
	}
	bound, err := st.PriorityClassesBound()
	if err != nil {
		t.Fatal(err)
	}
	table, err := st.PriorityClassTable()
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"gold": true, "silver": true, "bronze": false, priority.High: false} {
		c, _ := table.ByName(name)
		if bound[c.ID] != want {
			t.Fatalf("%s bound = %v, want %v (%v)", name, bound[c.ID], want, bound)
		}
	}
}

// A batch still sending requests, or an evaluation run still to send them,
// holds the class it runs at; a finished one does not.
func TestPriorityClassesHeldByBatchesAndRuns(t *testing.T) {
	st := newTestStore(t)
	for _, name := range []string{"copper", "tin", "iron", "zinc"} {
		if err := st.CreatePriorityClass(&PriorityClass{Name: name, Value: 5}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	for _, b := range []*Batch{
		{ID: "batch_running", Endpoint: "/v1/chat/completions", InputFileID: "file-a", CompletionWindow: "24h", Status: BatchInProgress, PriorityClass: "copper", CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
		{ID: "batch_done", Endpoint: "/v1/chat/completions", InputFileID: "file-b", CompletionWindow: "24h", Status: BatchCompleted, PriorityClass: "tin", CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
	} {
		if err := st.CreateBatch(b, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, run := range []*BenchmarkRun{{Name: "active", State: "running", PriorityClass: "iron"}, {Name: "done", State: "succeeded", PriorityClass: "zinc"}} {
		if _, err := st.CreateBenchmarkRun(run); err != nil {
			t.Fatal(err)
		}
	}
	bound, err := st.PriorityClassesBound()
	if err != nil {
		t.Fatal(err)
	}
	table, err := st.PriorityClassTable()
	if err != nil {
		t.Fatal(err)
	}
	for name, held := range map[string]bool{"copper": true, "iron": true, "tin": false, "zinc": false} {
		c, _ := table.ByName(name)
		if bound[c.ID] != held {
			t.Fatalf("%s bound = %v, want %v", name, bound[c.ID], held)
		}
		err := st.DeletePriorityClass(name)
		if held && !errors.Is(err, ErrPriorityClassInUse) {
			t.Fatalf("delete %s = %v, want in use", name, err)
		}
		if !held && err != nil {
			t.Fatalf("delete %s = %v", name, err)
		}
	}
}

// A batch or run naming a class that is gone by the time it is written is
// refused in the write's own transaction, so a delete cannot slip between a
// caller's check and the insert.
func TestRowsNamingAGoneClassAreRefused(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	err := st.CreateBatch(&Batch{ID: "batch_gone", Endpoint: "/v1/chat/completions", InputFileID: "f", CompletionWindow: "24h", Status: BatchValidating, PriorityClass: "gone", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}, nil)
	if !errors.Is(err, ErrPriorityClassGone) {
		t.Fatalf("batch err = %v", err)
	}
	if _, err := st.GetBatch("batch_gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused batch was written: %v", err)
	}
	if _, err := st.CreateBenchmarkRun(&BenchmarkRun{Name: "gone", PriorityClass: "gone"}); !errors.Is(err, ErrPriorityClassGone) {
		t.Fatalf("run err = %v", err)
	}
	// A class that exists, and none at all, are written.
	if err := st.CreateBatch(&Batch{ID: "batch_high", Endpoint: "/v1/chat/completions", InputFileID: "f", CompletionWindow: "24h", Status: BatchValidating, PriorityClass: priority.High, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateBenchmarkRun(&BenchmarkRun{Name: "none"}); err != nil {
		t.Fatal(err)
	}
}
