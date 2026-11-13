package store

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
)

// networkSettingsDB is a database as the release before the network settings
// moved: rules and nodes, a global network setting and node overrides, and
// model_files with the columns a deployment's hand-picked endpoint used.
func networkSettingsDB(t *testing.T) (*gorm.DB, *Node, *Node) {
	t.Helper()
	db := openPullMigrationDB(t)
	if err := db.AutoMigrate(&PullSourceRule{}, &PullSourceRuleEndpoint{}, &PullSourceRuleRequirement{}, &ModelFile{}); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"Proxy", "Endpoint", "NetworkExplicit"} {
		if err := db.Migrator().AddColumn(&modelFileNetworkColumns{}, field); err != nil {
			t.Fatal(err)
		}
	}
	a, b := &Node{UUID: "node-a", Name: "a"}, &Node{UUID: "node-b", Name: "b"}
	for _, node := range []*Node{a, b} {
		if err := db.Create(node).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&PullSourceRule{Position: 0, Match: "docker.io", AllowUpstream: true}).Error; err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"network": `{"http_proxy":"http://alice:pw@global.corp:3128","https_proxy":"http://alice:pw@global.corp:3128","no_proxy":".corp",` +
			`"rewrites":[{"from":"https://huggingface.co","to":"https://hf-mirror.com"},{"from":"https://huggingface.co/","to":"https://xget.example/hf"},{"from":"https://huggingface.co/org","to":"https://x.example"},{"from":"ftp://x","to":"https://y.example"}]}`,
		fmt.Sprintf("network:%d", a.ID): `{"https_proxy":"http://idc.corp:8080","rewrites":[{"from":"https://huggingface.co","to":"http://hf-proxy.idc:8090"}]}`,
		fmt.Sprintf("network:%d", b.ID): `{"no_proxy":"files.corp"}`,
		"network:999":                   `{"http_proxy":"http://gone.corp:1","rewrites":[{"from":"https://modelscope.cn","to":"https://ms.corp"}]}`,
	} {
		if err := db.Create(&Setting{Key: key, Value: value}).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db, a, b
}

// The rewrites become site rules - the node's own first, then the global
// ones, each site's targets in order - and leave the settings, whose proxies
// stay for the carry-over. model_files loses its endpoint columns and keeps
// its indexes. Applying it again adds nothing.
func TestNetworkRewritesBecomeSiteRules(t *testing.T) {
	db, a, _ := networkSettingsDB(t)
	indexesBefore, err := indexDefinitions(db, "model_files")
	if err != nil {
		t.Fatal(err)
	}
	m := migrationByName(t, "network_rewrites_to_site_rules")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}
	rules, err := readPullSourceRules(db)
	if err != nil {
		t.Fatal(err)
	}
	type rule struct {
		Match        string
		Endpoints    []string
		Upstream     bool
		ControlPlane bool
		Reqs         nodelabel.Requirements
	}
	var got []rule
	for _, r := range rules {
		got = append(got, rule{r.Match, r.Endpoints, r.AllowUpstream, r.ControlPlane, r.Requirements})
	}
	want := []rule{
		{"docker.io", nil, true, false, nil},
		{"https://huggingface.co", []string{"http://hf-proxy.idc:8090"}, true, false, nodelabel.Requirements{nodelabel.KeyNode: {a.UUID}}},
		{"https://huggingface.co", []string{"https://hf-mirror.com", "https://xget.example/hf"}, true, true, nil},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rules = %+v\nwant %+v", got, want)
	}
	var settings []Setting
	db.Order("key").Find(&settings)
	if len(settings) != 4 {
		t.Fatalf("settings = %+v, want all four kept for their proxies", settings)
	}
	for _, setting := range settings {
		if strings.Contains(setting.Value, "rewrites") {
			t.Fatalf("%s still has rewrites: %s", setting.Key, setting.Value)
		}
	}
	for _, column := range []string{"endpoint", "proxy", "network_explicit"} {
		if db.Migrator().HasColumn("model_files", column) {
			t.Fatalf("model_files.%s survived", column)
		}
	}
	if after, err := indexDefinitions(db, "model_files"); err != nil || len(after) != len(indexesBefore) {
		t.Fatalf("model_files indexes = %d, want %d (%v)", len(after), len(indexesBefore), err)
	}
	if !db.Migrator().HasTable(&PullProxy{}) || !db.Migrator().HasTable(&PullProxyRequirement{}) {
		t.Fatal("the proxy tables were not created")
	}
}

// Schema, rules and marker are one transaction: a failure leaves none of
// them, and the retry completes.
func TestNetworkRewritesMigrationRollsBack(t *testing.T) {
	db, _, _ := networkSettingsDB(t)
	m := migrationByName(t, "network_rewrites_to_site_rules")
	if err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_rewrites_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`, m.Version)).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err == nil {
		t.Fatal("injected marker failure did not fail the migration")
	}
	var rules int64
	db.Model(&PullSourceRule{}).Count(&rules)
	if rules != 1 || !db.Migrator().HasColumn("model_files", "endpoint") || db.Migrator().HasTable(&PullProxy{}) {
		t.Fatalf("a failed migration left changes: rules=%d", rules)
	}
	var global Setting
	db.First(&global, "key = ?", "network")
	if !strings.Contains(global.Value, "rewrites") {
		t.Fatal("a failed migration removed the rewrites")
	}
	if err := db.Exec(`DROP TRIGGER fail_rewrites_marker`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

// The proxies move into entries that give every node the proxy it had: a node
// override's merged values for that node, first, then the global setting for
// everyone, chosen for the management node. A login written into a URL moves
// to the entry's user and encrypted password. The network settings go.
func TestNetworkProxiesCarryOverKeepingEachNodesProxy(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()
	box, err := auth.NewSecretbox("carry-over")
	if err != nil {
		t.Fatal(err)
	}
	st.SetSecretbox(box)
	a, err := st.RegisterNode("node-a", HashToken("a"), "a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.RegisterNode("node-b", HashToken("b"), "b")
	if err != nil {
		t.Fatal(err)
	}
	c, err := st.RegisterNode("node-c", HashToken("c"), "c")
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[SettingKey]string{
		SettingNetwork:              `{"http_proxy":"http://alice:pw@global.corp:3128","https_proxy":"http://alice:pw@global.corp:3128","no_proxy":".corp"}`,
		NodeNetworkSettingKey(a.ID): `{"https_proxy":"http://idc.corp:8080"}`,
		NodeNetworkSettingKey(b.ID): `{"no_proxy":"files.corp"}`,
		NodeNetworkSettingKey(c.ID): `{}`,
		"network:999":               `{"http_proxy":"http://gone.corp:1"}`,
	} {
		if err := st.SetSetting(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CarryOverNetworkProxies(); err != nil {
		t.Fatal(err)
	}
	proxies, err := st.PullProxies()
	if err != nil || len(proxies) != 3 {
		t.Fatalf("entries = %+v, %v", proxies, err)
	}
	type entry struct {
		HTTP, HTTPS, NoProxy, User, Password string
		ControlPlane                         bool
		Reqs                                 nodelabel.Requirements
	}
	var got []entry
	for _, p := range proxies {
		got = append(got, entry{p.HTTPProxy, p.HTTPSProxy, p.NoProxy, p.Username, p.Password, p.ControlPlane, p.Requirements})
	}
	want := []entry{
		{"http://global.corp:3128", "http://idc.corp:8080", ".corp", "alice", "pw", false, nodelabel.Requirements{nodelabel.KeyNode: {a.UUID}}},
		{"http://global.corp:3128", "http://global.corp:3128", "files.corp", "alice", "pw", false, nodelabel.Requirements{nodelabel.KeyNode: {b.UUID}}},
		{"http://global.corp:3128", "http://global.corp:3128", ".corp", "alice", "pw", true, nil},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %+v\nwant %+v", got, want)
	}
	var row PullProxy
	// The cipher is random base64, so a substring check for "pw" would match it
	// by chance now and then; assert it is sealed by opening it instead.
	if err := st.db.First(&row, proxies[2].ID).Error; err != nil || row.SecretCipher == "" || row.SecretCipher == "pw" || strings.Contains(row.HTTPProxy, "alice") {
		t.Fatalf("stored entry = %+v, err %v", row, err)
	}
	if opened, err := box.Decrypt(row.SecretCipher); err != nil || string(opened) != "pw" {
		t.Fatalf("stored cipher opens to %q, %v; want the password sealed", opened, err)
	}
	var left int64
	st.db.Model(&Setting{}).Where("key = ? OR key LIKE ?", "network", "network:%").Count(&left)
	if left != 0 {
		t.Fatalf("%d network settings are left", left)
	}
	// Nothing left to carry over: running again changes nothing.
	if err := st.CarryOverNetworkProxies(); err != nil {
		t.Fatal(err)
	}
	if again, _ := st.PullProxies(); len(again) != 3 {
		t.Fatalf("a second run added entries: %d", len(again))
	}
}

// Without the key a password cannot be sealed: nothing moves and the settings
// stay for the next start.
func TestNetworkProxiesWaitForTheKey(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()
	if err := st.SetSetting(SettingNetwork, `{"http_proxy":"http://alice:pw@global.corp:3128"}`); err != nil {
		t.Fatal(err)
	}
	if err := st.CarryOverNetworkProxies(); err == nil || strings.Contains(err.Error(), "pw") {
		t.Fatalf("err = %v, want a refusal that does not name the password", err)
	}
	if proxies, _ := st.PullProxies(); len(proxies) != 0 || st.GetSetting(SettingNetwork) == "" {
		t.Fatalf("a refused carry-over moved something: %+v", proxies)
	}
}
