//go:build linux

package extplugin

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
)

// A plugin outside a cgroup that grows past 1.5 × its mem_mib, counting
// the child it started, is killed with its whole process group and
// restarted.
func TestRSSWatchdogRestartsAGrowingPlugin(t *testing.T) {
	tm := timing()
	tm.MemoryEvery = 50 * time.Millisecond
	tm.BackoffMin, tm.BackoffMax = 3*time.Second, 3*time.Second // long enough to see the backoff
	h := newHost(t, nil, tm)
	spec := fakeSpec(t, "acme_fake", "--grow-rss", "40")
	// The watchdog's bound is 72 MiB. The plugin holds 40 MiB besides its
	// runtime, below the bound; only with its child's 40 MiB is the group
	// above it.
	spec.MemMiB = 48
	if err := h.Start(spec); err != nil {
		t.Fatal(err)
	}
	st := waitStatus(t, h, "acme_fake", "killed for its memory", func(s pluginhost.Status) bool {
		return s.State == pluginhost.StateBackoff && s.Code == messages.ErrorPluginMemoryExceeded
	})
	if st.Params["id"] != "acme_fake" || st.Params["limit_mib"] != 48 {
		t.Fatalf("params = %#v", st.Params)
	}
	// The watchdog kills at its first sample above the bound, while the
	// plugin and its child are still touching their pages, so the sample is
	// anywhere from the bound (rss_mib truncates, so 72 MiB and a bit reads
	// 72) up to the 80 MiB they end at. At the bound is what shows the child
	// counted: the plugin alone holds less.
	if rss, _ := st.Params["rss_mib"].(int64); rss < 72 {
		t.Fatalf("rss_mib = %v, want the group's, at least the 72 MiB bound only the plugin and its child together exceed", st.Params["rss_mib"])
	}
	if st.Cgroup != "" {
		t.Fatalf("Cgroup = %q, the test host places no cgroups", st.Cgroup)
	}
	child := growChild(t, h, spec.DataDir)
	deadline := time.Now().Add(10 * time.Second)
	for alive(child) {
		if time.Now().After(deadline) {
			t.Fatalf("the plugin's child %d outlived the group kill", child)
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitStatus(t, h, "acme_fake", "restarted", func(s pluginhost.Status) bool { return s.Starts >= 2 })
}

// growChild reads the pid of the child --grow-rss started.
func growChild(t *testing.T, h *pluginhost.Host, dataDir string) int {
	t.Helper()
	f, err := os.Open(filepath.Join(dataDir, "results.jsonl"))
	if err != nil {
		t.Fatalf("%v; stderr:\n%s", err, h.Stderr("acme_fake"))
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec struct {
			Probe string
			Child int
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.Probe == "grow" && rec.Child > 0 {
			return rec.Child
		}
	}
	t.Fatalf("the plugin recorded no growing child; stderr:\n%s", h.Stderr("acme_fake"))
	return 0
}
