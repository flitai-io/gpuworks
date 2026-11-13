package serverconfig

import (
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/prepobs"
)

func TestLoadPeerRelayConfiguration(t *testing.T) {
	_, err := decodeFile("server.yaml", []byte("listen:\n  console: [127.0.0.1:8800]\npeer_relay:\n  rate_limit_kib_s: 32768\n  burst_kib: 32\n  no_progress_timeout: 3m\n  confirmation_timeout: 8m\n"))
	if err != nil {
		t.Fatalf("valid peer relay host configuration rejected: %v", err)
	}
}

func TestPeerRelayYAMLRejectsNonIntegerBudget(t *testing.T) {
	for _, field := range []string{"rate_limit_kib_s", "burst_kib"} {
		for _, value := range []string{"0.5", "1.9", "1e3", "\"64\"", "9223372036854775808", "9007199254740992"} {
			t.Run(field+"/"+value, func(t *testing.T) {
				_, err := decodeFile("server.yaml", []byte("peer_relay:\n  "+field+": "+value+"\n"))
				if err == nil {
					t.Fatalf("non-integer or overflowing %s=%s accepted", field, value)
				}
			})
		}
	}
	for _, raw := range []string{"peer_relay:\n  unknown_limit: 3\n", "peer_relay:\n  rate_limit_kib_s: 1\n  rate_limit_kib_s: 2\n"} {
		if _, err := decodeFile("server.yaml", []byte(raw)); err == nil {
			t.Fatalf("invalid nested mapping accepted: %s", strings.TrimSpace(raw))
		}
	}
}

func TestPeerRelayPolicyDefaultsAndCustomValues(t *testing.T) {
	defaults := prepobs.DefaultPeerRelayPolicy()
	for _, raw := range []PeerRelay{{}, {NoProgressTimeout: "0", ConfirmationTimeout: "0s"}} {
		got, err := raw.Policy()
		if err != nil || !reflect.DeepEqual(got, defaults) {
			t.Fatalf("zero must retain safe defaults: %+v %v", got, err)
		}
	}
	raw := PeerRelay{RateLimitKiBPerSecond: 32768, BurstKiB: 32, NoProgressTimeout: "3m", ConfirmationTimeout: "8m"}
	got, err := raw.Policy()
	want := defaults
	want.RateLimitBytesPerSecond = 32 << 20
	want.BurstBytes = 32 << 10
	want.QuantumBytes = 8 << 10
	want.NoProgressTimeout = 3 * time.Minute
	want.ConfirmationTimeout = 8 * time.Minute
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("custom policy=%+v err=%v want=%+v", got, err, want)
	}
	tiny, err := (PeerRelay{RateLimitKiBPerSecond: 1, BurstKiB: 1}).Policy()
	if err != nil || tiny.QuantumBytes != 256 || tiny.BurstBytes != 1024 || tiny.RateLimitBytesPerSecond != 1024 {
		t.Fatalf("small valid policy=%+v %v", tiny, err)
	}
}

func TestPeerRelayPolicyRejectsInvalidDirectConfiguration(t *testing.T) {
	for _, raw := range []PeerRelay{
		{RateLimitKiBPerSecond: -1}, {BurstKiB: -1},
		{RateLimitKiBPerSecond: math.MaxInt64/1024 + 1}, {BurstKiB: math.MaxInt64/1024 + 1},
		{NoProgressTimeout: "-1s"}, {ConfirmationTimeout: "-1s"},
		{NoProgressTimeout: "soon"}, {ConfirmationTimeout: "1e99h"},
	} {
		if _, err := raw.Policy(); err == nil {
			t.Fatalf("invalid direct policy accepted: %+v", raw)
		}
	}
}

func TestPeerRelayYAMLAndWriteRoundTrip(t *testing.T) {
	for _, raw := range []string{
		"", "peer_relay: {}\n", "peer_relay: null\n",
		"peer_relay:\n  rate_limit_kib_s: 0\n  burst_kib: 0\n  no_progress_timeout: 0s\n  confirmation_timeout: 0\n",
	} {
		file, err := decodeFile("server.yaml", []byte(raw))
		if err != nil {
			t.Fatalf("default YAML %q: %v", raw, err)
		}
		got, err := file.PeerRelay.Policy()
		if err != nil || !reflect.DeepEqual(got, prepobs.DefaultPeerRelayPolicy()) {
			t.Fatalf("default YAML policy %+v: %v", got, err)
		}
	}
	f := File{Listen: Listen{Console: []string{"127.0.0.1:8800"}}, PeerRelay: PeerRelay{RateLimitKiBPerSecond: 32768, BurstKiB: 32, NoProgressTimeout: "500ms", ConfirmationTimeout: "2m"}}
	path := filepath.Join(t.TempDir(), FileName)
	if err := Write(path, f, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || !reflect.DeepEqual(got, f) {
		t.Fatalf("peer relay roundtrip=%+v err=%v", got, err)
	}
	for _, raw := range []string{"peer_relay: []\n", "peer_relay: 0\n", "peer_relay:\n  no_progress_timeout: -1s\n", "peer_relay:\n  confirmation_timeout: tomorrow\n"} {
		if _, err := decodeFile("server.yaml", []byte(raw)); err == nil {
			t.Fatalf("invalid YAML accepted: %s", raw)
		}
	}
}
