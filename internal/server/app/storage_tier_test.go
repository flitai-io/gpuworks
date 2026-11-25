package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

func TestEnrollmentInfoRecordsTheStorageTierOnlyWhenItIsNotStandard(t *testing.T) {
	standard, err := json.Marshal(EnrollmentInfo{ClusterID: "c", CAFingerprint: "f", Protocol: 2, ListenAddress: "10.0.0.1:9443", PublicURL: "https://10.0.0.1:9443", StorageTier: storagetier.Standard.Recorded()})
	if err != nil || string(standard) != `{"cluster_id":"c","ca_fingerprint":"f","protocol":2,"listen_address":"10.0.0.1:9443","public_url":"https://10.0.0.1:9443"}` {
		t.Fatalf("standard = %s (%v), want the bytes of a cluster installed before tiers", standard, err)
	}
	slow, err := json.Marshal(EnrollmentInfo{ClusterID: "c", CAFingerprint: "f", Protocol: 2, ListenAddress: "10.0.0.1:9443", PublicURL: "https://10.0.0.1:9443", StorageTier: storagetier.SlowStorage.Recorded()})
	if err != nil || string(slow) != `{"cluster_id":"c","ca_fingerprint":"f","protocol":2,"listen_address":"10.0.0.1:9443","public_url":"https://10.0.0.1:9443","storage_tier":"slow-storage"}` {
		t.Fatalf("slow-storage = %s (%v)", slow, err)
	}
	var decoded EnrollmentInfo
	if err := json.Unmarshal(standard, &decoded); err != nil || decoded.StorageTier != "" {
		t.Fatalf("absent field decoded as %q (%v), want none", decoded.StorageTier, err)
	}
}

func TestRecordedStorageTier(t *testing.T) {
	write := func(t *testing.T, dir, name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "config.json"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name             string
		enrollment, join string
		want             storagetier.Tier
		refused          string
	}{
		{"no cluster files", "", "", storagetier.Standard, ""},
		{"enrollment without a tier", `{"cluster_id":"c"}`, "", storagetier.Standard, ""},
		{"enrollment slow-storage", `{"cluster_id":"c","storage_tier":"slow-storage"}`, "", storagetier.SlowStorage, ""},
		{"joining member has no enrollment configuration yet", "", `{"cluster_id":"c"}`, storagetier.Standard, ""},
		{"unknown tier", `{"cluster_id":"c","storage_tier":"glacial"}`, "", "", `unknown storage tier "glacial"`},
		{"broken file", `{`, "", "", "invalid cluster configuration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.enrollment != "" {
				write(t, dir, "cluster-enrollment", tc.enrollment)
			}
			if tc.join != "" {
				write(t, dir, "gpuworks.db.join", tc.join)
			}
			got, err := RecordedStorageTier(dir)
			if tc.refused != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refused) {
					t.Fatalf("error %v, want %q", err, tc.refused)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("tier %q err %v, want %q", got, err, tc.want)
			}
		})
	}
}

// RecordStorageTier changes the tier a member records and nothing else in its
// enrollment configuration, returns the tier before, and leaves the file
// owner-only. What it writes is what RecordedStorageTier and the enrollment
// bundle's strict reader accept.
func TestRecordStorageTierChangesOnlyTheTier(t *testing.T) {
	dir := t.TempDir()
	enrollment := filepath.Join(dir, "cluster-enrollment")
	if err := os.MkdirAll(enrollment, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(enrollment, "config.json")
	original := `{
  "cluster_id": "a7d38634923abc036fb6274263e5cd2b",
  "ca_fingerprint": "sha256:0011",
  "protocol": 1,
  "listen_address": "10.0.0.1:9443",
  "public_url": "https://10.0.0.1:9443",
  "storage_tier": "slow-storage"
}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	read := func() string {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	previous, err := RecordStorageTier(dir, "slow-storage@100ms")
	if err != nil || previous != storagetier.SlowStorage {
		t.Fatalf("previous %q, %v", previous, err)
	}
	if want := strings.Replace(original, `"slow-storage"`, `"slow-storage@100ms"`, 1); read() != want {
		t.Fatalf("configuration after the change:\n%s\nwant:\n%s", read(), want)
	}
	if tier, err := RecordedStorageTier(dir); err != nil || tier != "slow-storage@100ms" || tier.ElectionTimeout() != 1500*time.Millisecond {
		t.Fatalf("recorded %q, %v", tier, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode %v, %v", info.Mode(), err)
	}
	// The same tier again changes nothing, not even the file.
	before, _ := os.Stat(path)
	if previous, err := RecordStorageTier(dir, "slow-storage@100ms"); err != nil || previous != "slow-storage@100ms" {
		t.Fatalf("same tier again: %q, %v", previous, err)
	}
	if after, _ := os.Stat(path); !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("recording the same tier rewrote the configuration")
	}
	// Standard is recorded as no tier at all, as a cluster installed before
	// tiers has it.
	if _, err := RecordStorageTier(dir, storagetier.Standard); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read(), "storage_tier") {
		t.Fatalf("standard left a tier in the configuration:\n%s", read())
	}
	if entries, _ := os.ReadDir(enrollment); len(entries) != 1 {
		t.Fatalf("left %d files beside the configuration", len(entries)-1)
	}
	// Refusals leave the record as it is.
	kept := read()
	for _, tier := range []storagetier.Tier{"slow-storage@500ms", "fast", "slow-storage@50ms", ""} {
		if _, err := RecordStorageTier(dir, tier); err == nil {
			t.Errorf("tier %q was recorded", tier)
		}
	}
	if read() != kept {
		t.Fatal("a refused tier changed the configuration")
	}
	if _, err := RecordStorageTier(t.TempDir(), storagetier.SlowStorage); err == nil || !strings.Contains(err.Error(), "not a cluster member") {
		t.Fatalf("an installation without the record: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"cluster_id":"x","surprise":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordStorageTier(dir, storagetier.SlowStorage); err == nil {
		t.Fatal("a configuration with an unknown field was rewritten")
	}
}
