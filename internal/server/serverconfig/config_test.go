package serverconfig

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadReadsEveryKeyAndRejectsUnknownOnes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	content := `# operator note
listen:
  console: [192.0.2.156:8800]
  gateway: ["10.20.0.5:443"]
data_dir: /opt/gpuworks/data
log:
  file: /opt/gpuworks/logs/gpuworks-server.log
  max_size: 10MB
  keep: 20
  level: debug
  stall_report_after: 5s
trusted_proxies: [127.0.0.1/32]
metrics_retention: 168h
session_idle_timeout: 2h
session_max_lifetime: 72h
`
	if err := os.WriteFile(path, []byte(content), 0o664); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := File{
		Listen:         Listen{Console: []string{"192.0.2.156:8800"}, Gateway: []string{"10.20.0.5:443"}},
		DataDir:        "/opt/gpuworks/data",
		Log:            Log{File: "/opt/gpuworks/logs/gpuworks-server.log", MaxSize: "10MB", Keep: 20, Level: "debug", StallReportAfter: "5s"},
		TrustedProxies: []string{"127.0.0.1/32"}, MetricsRetention: "168h",
		SessionIdleTimeout: "2h", SessionMaxLifetime: "72h",
	}
	if !reflect.DeepEqual(f, want) {
		t.Fatalf("loaded %+v", f)
	}

	if err := os.WriteFile(path, []byte("listen:\n  consle: [1.2.3.4:80]\n"), 0o664); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "consle") {
		t.Fatalf("typo accepted: %v", err)
	}
	// The server keeps no external address and dials serving networks from
	// whatever the routing table picks; the keys that used to say otherwise
	// are gone.
	for _, removed := range []string{"public_url: http://198.51.100.152:8808", "serving_source: 10.40.0.1"} {
		if err := os.WriteFile(path, []byte("listen:\n  console: [1.2.3.4:80]\n"+removed+"\n"), 0o664); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("removed key accepted: %s", removed)
		}
	}
	if err := os.WriteFile(path, []byte("listen:\n  console: [example.com:80]\n"), 0o664); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "not an IP") {
		t.Fatalf("hostname accepted: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o664); err != nil {
		t.Fatal(err)
	}
	if f, err := Load(path); err != nil || !f.Listen.Empty() {
		t.Fatalf("empty file = %+v, %v", f, err)
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file err = %v", err)
	}
}

func TestSetListenKeepsOtherKeysAndComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	content := "# keep me\nlisten:\n  console: [10.0.0.1:8800]\ndata_dir: /data # the data\nlog:\n  keep: 5\n"
	if err := os.WriteFile(path, []byte(content), 0o660); err != nil {
		t.Fatal(err)
	}
	// WriteFile's mode passes through the umask; set it outright so the test
	// knows the mode SetListen has to preserve.
	if err := os.Chmod(path, 0o660); err != nil {
		t.Fatal(err)
	}
	if err := SetListen(path, Listen{Console: []string{"10.0.0.2:8800"}, Gateway: []string{"10.0.0.3:8800"}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	text := string(raw)
	for _, want := range []string{"# keep me", "# the data", "data_dir: /data", "keep: 5", "10.0.0.2:8800", "10.0.0.3:8800"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, "10.0.0.1") {
		t.Fatalf("old address kept:\n%s", text)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o660 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	if err := SetListen(path, Listen{Console: []string{"10.0.0.2:8800"}}); err != nil {
		t.Fatal(err)
	}
	if f, err := Load(path); err != nil || f.Listen.Gateway != nil || f.DataDir != "/data" {
		t.Fatalf("after dropping the gateway address: %+v, %v", f, err)
	}
	if err := SetListen(path, Listen{Agent: []string{"10.0.0.2:8800"}}); err == nil {
		t.Fatal("listen without console accepted")
	}
}

func TestWriteRendersAFileLoadReadsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	f := File{Listen: Listen{Console: []string{"10.0.0.1:8800"}}, DataDir: "/d", Log: Log{File: "/l/s.log", MaxSize: "10MB", Keep: 20}, TrustedProxies: []string{"127.0.0.1/32", "::1/128"}, SessionIdleTimeout: "2h", SessionMaxLifetime: "72h"}
	if err := Write(path, f, 0o664); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || !reflect.DeepEqual(got, f) {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
}

func TestBindingsMergeAddressesAndAddLoopback(t *testing.T) {
	l := Listen{
		Console: []string{"192.0.2.156:8800"},
		Agent:   []string{"192.0.2.156:8800", "10.100.10.1:8800"},
		Gateway: []string{"10.20.0.5:8800", "10.20.0.5:443"},
	}
	if err := l.Validate(); err != nil {
		t.Fatal(err)
	}
	got := Bindings(l)
	want := []Binding{
		{Addr: "192.0.2.156:8800", Functions: []string{FunctionConsole, FunctionAgent}},
		{Addr: "10.100.10.1:8800", Functions: []string{FunctionAgent}},
		{Addr: "10.20.0.5:8800", Functions: []string{FunctionGateway}},
		{Addr: "10.20.0.5:443", Functions: []string{FunctionGateway}},
		{Addr: "127.0.0.1:443", Functions: Functions},
		{Addr: "127.0.0.1:8800", Functions: Functions},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bindings = %+v", got)
	}

	single := Bindings(Listen{Console: []string{"10.0.0.1:8800"}})
	if len(single) != 2 || !reflect.DeepEqual(single[0].Functions, Functions) || single[1].Addr != "127.0.0.1:8800" {
		t.Fatalf("single address inherits every function: %+v", single)
	}
	wild := Bindings(Listen{Console: []string{"0.0.0.0:8800"}})
	if len(wild) != 1 || wild[0].Addr != "0.0.0.0:8800" {
		t.Fatalf("a wildcard covers loopback: %+v", wild)
	}
	if err := (Listen{Console: []string{"0.0.0.0:8800"}, Gateway: []string{"10.0.0.1:8800"}}).Validate(); err == nil {
		t.Fatal("wildcard sharing a port with a specific address accepted")
	}
	both := Listen{Console: []string{"0.0.0.0:8800", "127.0.0.1:8800"}}
	if err := both.Validate(); err != nil {
		t.Fatalf("explicit loopback beside a wildcard: %v", err)
	}
	if got := Bindings(both); len(got) != 1 || got[0].Addr != "0.0.0.0:8800" {
		t.Fatalf("loopback beside a wildcard is bound twice: %+v", got)
	}
	other := Bindings(Listen{Console: []string{"127.0.0.2:8800"}})
	if len(other) != 2 || !reflect.DeepEqual(other[0].Functions, Functions) || other[1].Addr != "127.0.0.1:8800" {
		t.Fatalf("127.0.0.2 is an ordinary address: %+v", other)
	}
}

func TestFunctionOfRoutesPaths(t *testing.T) {
	for path, want := range map[string]string{
		"/healthz":                     "",
		"/api/v1/agent/connect":        FunctionAgent,
		"/install.sh":                  FunctionAgent,
		"/dl/gpuworks-agent-linux":     FunctionAgent,
		"/v1/chat/completions":         FunctionGateway,
		"/v1/models":                   FunctionGateway,
		"/v1":                          FunctionGateway,
		"/api/v1/nodes":                FunctionConsole,
		"/api/v1/agent/upgrade-errors": FunctionConsole,
		"/":                            FunctionConsole,
		"/v1x":                         FunctionConsole,
	} {
		if got := FunctionOf(path); got != want {
			t.Errorf("FunctionOf(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestDetectIPPrefersTheDefaultRouteThenAPhysicalInterface(t *testing.T) {
	ifaces := func() ([]Interface, error) {
		return []Interface{
			{Name: "lo", Up: true, Addrs: []net.IP{net.ParseIP("127.0.0.1")}},
			{Name: "docker0", Up: true, Addrs: []net.IP{net.ParseIP("172.17.0.1")}},
			{Name: "eth1", Up: false, Addrs: []net.IP{net.ParseIP("10.9.9.9")}},
			{Name: "eth0", Up: true, Addrs: []net.IP{net.ParseIP("fe80::1"), net.ParseIP("10.0.0.7")}},
		}, nil
	}
	ip, err := DetectIP(Host{RouteIP: func() (net.IP, error) { return net.ParseIP("192.168.1.5"), nil }, Interfaces: ifaces})
	if err != nil || ip.String() != "192.168.1.5" {
		t.Fatalf("route ip = %v, %v", ip, err)
	}
	noRoute := func() (net.IP, error) { return nil, errors.New("network is unreachable") }
	ip, err = DetectIP(Host{RouteIP: noRoute, Interfaces: ifaces})
	if err != nil || ip.String() != "10.0.0.7" {
		t.Fatalf("fallback ip = %v, %v", ip, err)
	}
	_, err = DetectIP(Host{RouteIP: noRoute, Interfaces: func() ([]Interface, error) {
		return []Interface{{Name: "lo", Up: true, Addrs: []net.IP{net.ParseIP("127.0.0.1")}}}, nil
	}})
	if !errors.Is(err, ErrNoAddress) {
		t.Fatalf("nothing to bind err = %v", err)
	}
	local, err := LocalIPs(Host{Interfaces: ifaces})
	if err != nil || !local["10.0.0.7"] || !local["127.0.0.1"] || local["10.0.0.8"] {
		t.Fatalf("local ips = %v, %v", local, err)
	}
}

func TestUpdateChannelRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	original := "# operator note\nlisten:\n  console: [\"127.0.0.1:8800\"]\nlog:\n  level: info\n"
	if err := os.WriteFile(path, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := SetUpdateChannel(path, "release"); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil || f.UpdateChannel != "release" || f.Log.Level != "info" || len(f.Listen.Console) != 1 {
		t.Fatalf("Load = %+v, %v", f, err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "# operator note") || !strings.Contains(string(raw), "update_channel: release") {
		t.Fatalf("file = %s", raw)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	if err := SetUpdateChannel(path, ""); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "update_channel") {
		t.Fatalf("an empty channel must remove the key: %s", raw)
	}
	if err := SetUpdateChannel(path, "beta"); err == nil || !strings.Contains(err.Error(), "nightly, release") {
		t.Fatalf("an unknown channel = %v", err)
	}
	if err := os.WriteFile(path, []byte("listen:\n  console: [\"127.0.0.1:8800\"]\nupdate_channel: preview\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "update_channel") {
		t.Fatalf("Load accepted an unknown channel: %v", err)
	}
}

func TestAutoCheckUpdatesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	original := "# operator note\nlisten:\n  console: [\"127.0.0.1:8800\"]\nupdate_channel: release\n"
	if err := os.WriteFile(path, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	if f, err := Load(path); err != nil || f.AutoCheckUpdates != nil {
		t.Fatalf("an absent key must stay absent (it means on): %+v, %v", f, err)
	}
	if err := SetAutoCheckUpdates(path, false); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil || f.AutoCheckUpdates == nil || *f.AutoCheckUpdates || f.UpdateChannel != "release" {
		t.Fatalf("Load = %+v, %v", f, err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "# operator note") || !strings.Contains(string(raw), "auto_check_updates: false") {
		t.Fatalf("file = %s", raw)
	}
	if err := SetAutoCheckUpdates(path, true); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "auto_check_updates") || !strings.Contains(string(raw), "update_channel: release") {
		t.Fatalf("switching it on must remove the key and keep the rest: %s", raw)
	}
}

func TestUsageReportRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	original := "# operator note\nlisten:\n  console: [\"127.0.0.1:8800\"]\nauto_check_updates: false\n"
	if err := os.WriteFile(path, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	if f, err := Load(path); err != nil || f.UsageReport != nil {
		t.Fatalf("an absent key must stay absent (it means on): %+v, %v", f, err)
	}
	if err := SetUsageReport(path, false); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil || f.UsageReport == nil || *f.UsageReport || f.AutoCheckUpdates == nil {
		t.Fatalf("Load = %+v, %v", f, err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "# operator note") || !strings.Contains(string(raw), "usage_report: false") {
		t.Fatalf("file = %s", raw)
	}
	if err := SetUsageReport(path, true); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "usage_report") || !strings.Contains(string(raw), "auto_check_updates: false") {
		t.Fatalf("switching it on must remove the key and keep the rest: %s", raw)
	}
	// A private delivery writes the key by hand before the first start.
	if err := os.WriteFile(path, []byte("listen:\n  console: [\"127.0.0.1:8800\"]\nusage_report: false\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if f, err := Load(path); err != nil || f.UsageReport == nil || *f.UsageReport {
		t.Fatalf("hand-written key = %+v, %v", f, err)
	}
}

// auth.local_password accepts the two documented values and empty, and a file
// carrying anything else is refused at load rather than silently ignored.
func TestAuthLocalPasswordValidation(t *testing.T) {
	for _, tc := range []struct {
		value string
		ok    bool
	}{
		{"", true}, {LocalPasswordAll, true}, {LocalPasswordLocalAdmin, true},
		{"admin", false}, {"LOCAL_ADMIN", false}, {"true", false},
	} {
		if err := (Auth{LocalPassword: tc.value}).Validate(); (err == nil) != tc.ok {
			t.Fatalf("auth.local_password %q: err=%v, want ok=%t", tc.value, err, tc.ok)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("auth:\n  local_password: local_admin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil || f.Auth.LocalPassword != LocalPasswordLocalAdmin {
		t.Fatalf("load = %+v %v", f.Auth, err)
	}
	if err := os.WriteFile(path, []byte("auth:\n  local_password: nobody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "auth.local_password") {
		t.Fatalf("load with an invalid value = %v, want an auth.local_password error", err)
	}
}

// auth.external.provision accepts auto, existing and empty; anything else is
// refused at load.
func TestAuthExternalProvisionValidation(t *testing.T) {
	for _, tc := range []struct {
		value string
		ok    bool
	}{
		{"", true}, {ProvisionAuto, true}, {ProvisionExisting, true},
		{"none", false}, {"AUTO", false}, {"true", false},
	} {
		if err := (Auth{External: AuthExternal{Provision: tc.value}}).Validate(); (err == nil) != tc.ok {
			t.Fatalf("auth.external.provision %q: err=%v, want ok=%t", tc.value, err, tc.ok)
		}
	}
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("auth:\n  external:\n    provision: existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if f, err := Load(path); err != nil || f.Auth.External.Provision != ProvisionExisting {
		t.Fatalf("load = %+v %v", f.Auth, err)
	}
	if err := os.WriteFile(path, []byte("auth:\n  external:\n    provision: everyone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "auth.external.provision") {
		t.Fatalf("load with an invalid value = %v, want an auth.external.provision error", err)
	}
}
