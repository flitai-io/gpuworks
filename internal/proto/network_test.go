package proto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNetworkConfigApply(t *testing.T) {
	net := NetworkConfig{Rewrites: []URLRewrite{
		{From: "https://huggingface.co", To: "https://hf-mirror.com"},
		{From: "https://huggingface.co/api", To: "https://hf-mirror.com/api2"}, // longer prefix wins
	}}
	cases := map[string]string{
		"https://huggingface.co/Qwen/Qwen3-8B/resolve/main/config.json": "https://hf-mirror.com/Qwen/Qwen3-8B/resolve/main/config.json",
		"https://huggingface.co/api/models/x":                           "https://hf-mirror.com/api2/models/x",
		"https://modelscope.cn/models/x":                                "https://modelscope.cn/models/x", // no rule
	}
	for in, want := range cases {
		if got := net.Apply(in); got != want {
			t.Errorf("Apply(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNetworkConfigNoRules(t *testing.T) {
	var net NetworkConfig
	u := "https://huggingface.co/x"
	if got := net.Apply(u); got != u {
		t.Errorf("empty config changed url: %q", got)
	}
}

func TestEnginePortsWireShape(t *testing.T) {
	b, err := json.Marshal(NetworkConfig{EnginePorts: &PortRange{Start: 30000, End: 32767}})
	if err != nil || string(b) != `{"engine_ports":{"start":30000,"end":32767}}` {
		t.Fatalf("network config = %s, %v", b, err)
	}
	if b, _ := json.Marshal(NetworkConfig{}); string(b) != `{}` {
		t.Fatalf("network config without a range = %s, want {}", b)
	}
	b, err = json.Marshal(NetInventory{Interfaces: []NetInterface{}, RDMAPorts: []RDMAPort{}, EnginePortListeners: []int{30001}})
	if err != nil || string(b) != `{"interfaces":[],"rdma_ports":[],"engine_port_listeners":[30001]}` {
		t.Fatalf("inventory = %s, %v", b, err)
	}
	var back NetInventory
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back.EnginePortListeners, []int{30001}) {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
	if b, _ := json.Marshal(NetInventory{Interfaces: []NetInterface{}, RDMAPorts: []RDMAPort{}, EnginePortListeners: []int{}}); strings.Contains(string(b), "engine_port_listeners") {
		t.Fatalf("inventory without listeners = %s, want the key omitted", b)
	}
}

func TestPortRangeContainsItsBounds(t *testing.T) {
	r := PortRange{Start: 30000, End: 30002}
	for port, want := range map[int]bool{29999: false, 30000: true, 30001: true, 30002: true, 30003: false} {
		if got := r.Contains(port); got != want {
			t.Errorf("Contains(%d) = %v, want %v", port, got, want)
		}
	}
}

func TestParsePortRange(t *testing.T) {
	if r, err := ParsePortRange(" 40000-42767 "); err != nil || r != (PortRange{Start: 40000, End: 42767}) {
		t.Fatalf("ParsePortRange = %+v, %v", r, err)
	}
	for _, bad := range []string{"", "40000", "42767-40000", "0-10", "1-70000", "a-b"} {
		if _, err := ParsePortRange(bad); err == nil {
			t.Fatalf("ParsePortRange(%q) accepted", bad)
		}
	}
}
