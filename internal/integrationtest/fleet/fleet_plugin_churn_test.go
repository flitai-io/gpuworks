//go:build linux

package fleet

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	serverapp "gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/testenv"
	"gpuworks.ai/gpuworks/internal/testenv/extplugin"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// The test binary is the plugin-exec trampoline the server starts plugins
// through (extplugin.RunTrampolineIfAsked).
func TestMain(m *testing.M) {
	extplugin.RunTrampolineIfAsked()
	os.Exit(m.Run())
}

const (
	churnPluginID = "acme_churn"
	// churnPushEvery is how often the plugin refreshes its table.
	churnPushEvery = 2 * time.Second
	// churnCPUBudget is how much more CPU the server may spend over the
	// window with the pushing plugin than without it.
	churnCPUBudget = 1.05
)

// TestFleetPluginPushChurn measures what a plugin that keeps pushing costs
// the control plane of a connected fleet: the process's CPU-seconds over a
// window with a plugin installed that pushes a node_holds table over every
// node every two seconds, each push changing 1% of it, against the mean of
// a window before it was installed and one after it was disabled. The plugin runs as
// its own process, so the measurement is what the server (and, in this
// one-process shape, the in-process agents, the same in both windows)
// spends taking the pushes, writing them and applying the holds.
//
// It is a load measurement, gated like the other fleet cases. 200 nodes is
// the churn gate every change to a per-node path runs; 1000 is the fleet
// target:
//
//	GPUWORKS_TEST_PERF=1 GPUWORKS_FLEET_NODES=200 go test -count=1 -v -timeout 60m ./internal/integrationtest/fleet/ -run '^TestFleetPluginPushChurn$'
//	GPUWORKS_TEST_PERF=1 GPUWORKS_FLEET_NODES=1000 go test -count=1 -v -timeout 60m ./internal/integrationtest/fleet/ -run '^TestFleetPluginPushChurn$'
//
// GPUWORKS_FLEET_WINDOW_SECONDS sets the window (default 60).
func TestFleetPluginPushChurn(t *testing.T) {
	skipUnlessPerf(t)
	nodeCount := EnvInt("GPUWORKS_FLEET_NODES", 200)
	window := time.Duration(EnvInt("GPUWORKS_FLEET_WINDOW_SECONDS", 60)) * time.Second
	requireFileDescriptors(t, 6*nodeCount+2048)

	bin, err := extplugin.BuildFakePlugin(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := extsign.NewRoot(t, "1")
	pub := extsign.NewPublisher(t, "acme", nil, root)
	// Installed packages are read-only; registered before the server, so
	// it runs after the server stopped and before the temporary directory
	// is removed.
	var dataRoot string
	t.Cleanup(func() {
		if dataRoot != "" {
			layout.RemoveAll(dataRoot)
		}
	})
	env := testenv.NewOnDiskConfigured(t, func(c *serverapp.Config) {
		c.PluginRoots = []extapi.PublicKey{root.Public}
		c.PluginRuntime = serverapp.PluginRuntime{Trampoline: self}
		// The fixture package runs without Landlock where the host has none;
		// isolation is not what this case measures.
		c.Plugins = serverconfig.Plugins{AllowUnsandboxed: []string{churnPluginID}}
	})

	dataRoot = env.DataDir()

	start := time.Now()
	for i := range nodeCount {
		node := env.AddNode(fmt.Sprintf("churn-%04d", i), testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 81920}), testenv.WithDriverName("docker"))
		node.Driver.ShareEnginePorts()
	}
	client, err := Login(env.URL(), "admin", testenv.AdminPassword)
	if err != nil {
		t.Fatalf("log in to the control plane: %v", err)
	}
	if online, err := WaitFleetOnline(client, nodeCount, 10*time.Minute); err != nil {
		t.Fatal(err)
	} else {
		t.Logf("%d nodes online in %s", online, time.Since(start).Round(time.Millisecond))
	}
	// Let the registration burst (first status reports, inventories) end
	// before the baseline window.
	time.Sleep(min(window/2, 30*time.Second))

	baseline := cpuOver(window)

	dataDir := layout.New(env.DataDir()).Data(churnPluginID)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	args := []string{"--requires", "table.node_holds@1-1", "--push-every", churnPushEvery.String(), "--push-size", fmt.Sprint(nodeCount)}
	if err := os.WriteFile(filepath.Join(dataDir, "fakeplugin.args"), []byte(strings.Join(args, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	installPlugin(t, env.API(t), churnPackage(t, pub, bin))
	env.Eventually(t, time.Minute, func() bool { return pushesAccepted(t, dataDir) > 0 })
	env.Eventually(t, time.Minute, func() bool { return heldByPlugins(t, env) == nodeCount })
	before := pushesAccepted(t, dataDir)

	loaded := cpuOver(window)
	pushed := pushesAccepted(t, dataDir) - before

	if online, err := WaitFleetOnline(client, nodeCount, time.Minute); err != nil {
		t.Fatalf("nodes went offline while the plugin pushed: %v (%d online)", err, online)
	}
	// The fleet's own cost drifts over a run (metrics accumulate), so the
	// windows are idle, pushing, idle again, and the pushing window is
	// compared with the mean of the two around it. Disabling the plugin
	// stops it; the holds it pushed stay, as restrictions do.
	disablePlugin(t, env.API(t))
	settled := pushesAccepted(t, dataDir)
	env.Eventually(t, time.Minute, func() bool {
		time.Sleep(2 * churnPushEvery)
		n := pushesAccepted(t, dataDir)
		stopped := n == settled
		settled = n
		return stopped
	})
	after := cpuOver(window)

	idle := (baseline + after) / 2
	ratio := loaded.Seconds() / idle.Seconds()
	t.Logf("fleet=%d window=%s pushes=%d (every %s, %d holds, 1%% changed)", nodeCount, window, pushed, churnPushEvery, nodeCount)
	t.Logf("process CPU-seconds: idle before %.2f, pushing %.2f, idle after %.2f; pushing / idle mean %.3f (budget %.2f)",
		baseline.Seconds(), loaded.Seconds(), after.Seconds(), ratio, churnCPUBudget)
	if want := int(window/churnPushEvery) / 2; pushed < want {
		t.Fatalf("the plugin pushed %d times in the window, want at least %d: the window did not measure a pushing plugin", pushed, want)
	}
	if ratio > churnCPUBudget {
		t.Errorf("the pushing plugin costs %.1f%% more CPU, budget %.0f%%", (ratio-1)*100, (churnCPUBudget-1)*100)
	}
}

// cpuOver sleeps for d and returns the CPU time (user and system) this
// process spent meanwhile. Child processes, the plugin among them, are not
// counted.
func cpuOver(d time.Duration) time.Duration {
	used := func() time.Duration {
		var ru syscall.Rusage
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
			panic(err)
		}
		return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	}
	before := used()
	time.Sleep(d)
	return used() - before
}

// churnPackage is the fake plugin as acme_churn: it may push node holds
// with the two codes the periodic push uses.
func churnPackage(t *testing.T, pub *extsign.Publisher, bin string) []byte {
	t.Helper()
	body, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	return extsign.Package(t, pub, extsign.Spec{
		Manifest: extapi.Manifest{
			Schema: extapi.ManifestSchema, ID: churnPluginID, Publisher: "acme", Version: "1.0.0",
			Created: extapi.NewTime(extsign.Epoch.Add(24 * time.Hour)), Protocol: extapi.Range{Min: 1, Max: 1},
			Runtime:     &extapi.RuntimeDecl{Lifecycle: extapi.LifecycleResident},
			Requires:    map[string]int{extapi.TableCapability(extapi.KindNodeHolds): 1},
			Permissions: []string{"push:node_holds"},
			Codes:       []string{"ACME_MAINTENANCE", "ACME_QUARANTINE"},
		},
		Files: map[string]extsign.File{
			"bin/linux_" + runtime.GOARCH + "/" + churnPluginID: {Body: body, Exec: true},
			"locales/en.json": {Body: []byte(`{}`)},
		},
	})
}

// installPlugin uploads raw, reads the approval hash the first answer
// names, and installs and enables the package with that approval.
func installPlugin(t *testing.T, api *testenv.APIClient, raw []byte) {
	t.Helper()
	post := func(query string) (int, []byte) {
		req, err := http.NewRequest(http.MethodPost, api.BaseURL+"/api/v1/plugins/install"+query, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := api.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, body
	}
	status, body := post("")
	var refusal struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if status != http.StatusConflict || json.Unmarshal(body, &refusal) != nil || refusal.Code != "PLUGIN_APPROVAL_REQUIRED" {
		t.Fatalf("install without approval = %d %s", status, body)
	}
	hash, _ := refusal.Params["approve_hash"].(string)
	if status, body = post("?approve=" + hash + "&enable=true"); status != http.StatusOK {
		t.Fatalf("install = %d %s", status, body)
	}
}

// disablePlugin disables the churn plugin, which stops it.
func disablePlugin(t *testing.T, api *testenv.APIClient) {
	t.Helper()
	resp, err := api.PostJSON("/api/v1/plugins/"+churnPluginID+"/disable", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("disable = %d %s", resp.StatusCode, body)
	}
}

// pushesAccepted counts the pushes the fake plugin recorded as accepted in
// results.jsonl in its data directory.
func pushesAccepted(t *testing.T, dataDir string) int {
	t.Helper()
	f, err := os.Open(filepath.Join(dataDir, "results.jsonl"))
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r struct {
			Kind string `json:"kind"`
			OK   bool   `json:"ok"`
		}
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.OK && r.Kind == extapi.KindNodeHolds {
			n++
		}
	}
	return n
}

// heldByPlugins counts the nodes held out of scheduling by pushed state.
func heldByPlugins(t *testing.T, env *testenv.Env) int {
	t.Helper()
	nodes, err := env.Store().ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, node := range nodes {
		if string(node.SchedulingBy) == pushstate.HoldSource {
			n++
		}
	}
	return n
}
