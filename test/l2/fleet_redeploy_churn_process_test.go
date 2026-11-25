//go:build multiproc

package l2

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/integrationtest/fleet"
)

// TestFleet1000NodesRedeployChurnAcrossProcesses runs the same churn as the
// in-process fleet case with the control plane where it really lives: the
// shipped server binary, in its own process, with the fleet in a second one.
//
// The in-process shape cannot answer what the server costs, because its memory,
// its CPU and its scheduler are the fleet's as well. Here each side is read
// separately from /proc, and a session the server drops is a session it dropped
// on its own account.
//
// Everything between "the fleet is online" and "the churn is over" is the
// shared driver in internal/integrationtest/fleet, so the two shapes differ in
// process topology and in nothing else.
func TestFleet1000NodesRedeployChurnAcrossProcesses(t *testing.T) {
	if os.Getenv("GPUWORKS_TEST_PERF") != "1" {
		t.Skip("fleet load case; set GPUWORKS_TEST_PERF=1 (make test-fleet-perf-processes) on an idle host to run")
	}
	nodeCount := fleet.EnvInt("GPUWORKS_FLEET_NODES", 1000)
	churnCount := fleet.EnvInt("GPUWORKS_FLEET_CHURN_NODES", nodeCount/2)
	rounds := fleet.EnvInt("GPUWORKS_FLEET_ROUNDS", 2)
	if churnCount > nodeCount {
		churnCount = nodeCount
	}
	// Both processes need descriptors: the server one per session per lane, the
	// fleet the same plus its own state. The limit is the test process's, and
	// children inherit it.
	requireFleetFileDescriptors(t, 6*nodeCount+2048)

	serverBin := fleetBinary(t, "GPUWORKS_FLEET_SERVER_BIN", "./cmd/gpuworks")
	hostBin := fleetBinary(t, "GPUWORKS_FLEET_HOST_BIN", "./test/l2/cmd/fleet-host")

	workRoot := fleetWorkRoot(t)
	dataDir := filepath.Join(workRoot, "server-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	server := serverCommand(t, fleetAdminPassword, serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", dataDir)
	server.Stdout, server.Stderr = fleetProcessLog(t, workRoot, "server.log")
	if err := server.Start(); err != nil {
		t.Fatalf("start the control plane: %v", err)
	}
	t.Cleanup(func() { stopProcess(server) })
	peak := &peakRSS{}
	t.Cleanup(peak.sample(server.Process.Pid, time.Second))

	client := fleetLogin(t, base)

	// Observe every join, including the half-full to full fleet transition.
	// A final online count cannot prove list responsiveness during onboarding.
	roster := make([]fleetRosterEntry, 0, nodeCount)
	modelDir := filepath.Join(workRoot, "preloaded-model")
	var host *exec.Cmd
	nextRosterPath := filepath.Join(workRoot, "next-roster.json")
	for _, limit := range []int{nodeCount / 2, nodeCount} {
		if limit == len(roster) {
			continue
		}
		start := time.Now()
		phase := fmt.Sprintf("onboarding-%d-to-%d", len(roster), limit)
		probe := fleet.StartListReadProbe(client, phase, 1)
		t.Cleanup(func() { probe.Stop() })
		from := len(roster)
		for i := from; i < limit; i++ {
			name := fmt.Sprintf("fleet-%04d", i)
			identity, err := client.CreateNode(name)
			if err != nil {
				t.Fatalf("enrol %s: %v", name, err)
			}
			roster = append(roster, fleetRosterEntry{Name: name, UUID: identity.UUID, Token: identity.Token, Churn: i < churnCount})
		}
		if host == nil {
			host = startFleetGroup(t, hostBin, base, workRoot, modelDir, nextRosterPath, roster[from:])
		} else {
			writeFleetRoster(t, nextRosterPath, roster[from:])
		}
		if _, err := fleet.WaitFleetCards(client, limit, max(fleet.EnvInt("GPUWORKS_FLEET_GPUS", 1), 1), 10*time.Minute); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: online in %s", phase, time.Since(start).Round(time.Millisecond))
		checkFleetReadProbe(t, probe)
		stable := fleet.StartListReadProbe(client, fmt.Sprintf("stable-%d", limit), 3)
		t.Cleanup(func() { stable.Stop() })
		// Slow-but-in-budget routes get enough time to supply real samples.
		if err := stable.WaitSamples(); err != nil {
			t.Error(err)
		}
		checkFleetReadProbe(t, stable)
	}

	// The weights are already on the node, so the control plane registers the
	// directory over node RPC instead of the fleet downloading a copy each.
	modelID, err := client.RegisterLocalModel(roster[0].UUID, modelDir, "qwen3-fleet-fake")
	if err != nil {
		t.Fatalf("register the preloaded model: %v", err)
	}
	churnUUIDs := make([]string, 0, churnCount)
	for i := 0; i < churnCount; i++ {
		if i > 0 {
			if err := client.RegisterModelPath(modelID, roster[i].UUID, modelDir); err != nil {
				t.Fatalf("register the model on %s: %v", roster[i].Name, err)
			}
		}
		churnUUIDs = append(churnUUIDs, roster[i].UUID)
	}
	gpuReads := fleet.GPUReadsEnabled()
	if gpuReads {
		if err := fleet.WaitGPUInventoryMetrics(client, nodeCount, 45*time.Second); err != nil {
			t.Fatal(err)
		}
		t.Logf("GPU inventory telemetry preflight passed for %d synthetic cards", nodeCount)
	}

	dbPath := filepath.Join(dataDir, "gpuworks.db")
	baselineDB := fleetDatabaseBytes(dbPath)
	serverBefore := readProcessCPU(server.Process.Pid)
	hostBefore := readProcessCPU(host.Process.Pid)

	watch := &fleet.NodeWatch{Snapshot: func() ([]fleet.NodeState, error) { return fleet.ListNodes(client) }}
	report, err := fleet.Churn{
		Client: client, ModelID: modelID, ChurnNodes: churnUUIDs,
		Rounds: rounds, Reads: fleet.ReadsEnabled(), GPUReads: gpuReads, Watch: watch,
		Log: func(format string, args ...any) { t.Logf(format, args...) },
	}.Run()
	if err != nil {
		t.Fatalf("churn: %v", err)
	}

	t.Logf("fleet=%d churn=%d rounds=%d", nodeCount, churnCount, rounds)
	for _, line := range report.Lines() {
		t.Log(line)
	}
	t.Logf("database grew %.1f MiB", float64(fleetDatabaseBytes(dbPath)-baselineDB)/(1<<20))
	serverCPU := readProcessCPU(server.Process.Pid) - serverBefore
	t.Logf("server process: RSS=%.1f MiB, %.1f CPU-seconds during the churn",
		readProcessRSSMiB(server.Process.Pid), serverCPU)
	// RSS covers onboarding and churn; CPU-seconds cover exactly the churn
	// window used in baseline/candidate comparisons. These coarse ceilings
	// catch resource regressions, not small performance differences.
	maxRSS := float64(fleet.EnvInt("GPUWORKS_FLEET_MAX_SERVER_RSS_MIB", 2048))
	maxCPU := float64(fleet.EnvInt("GPUWORKS_FLEET_MAX_SERVER_CPU_SECONDS", max(60, nodeCount*rounds)))
	t.Logf("server resource budgets: peak RSS=%.1f/%.1f MiB; churn CPU=%.1f/%.1f seconds", peak.max(), maxRSS, serverCPU, maxCPU)
	if peak.max() > maxRSS {
		t.Errorf("server RSS peak %.1f MiB exceeds budget %.1f MiB", peak.max(), maxRSS)
	}
	if serverCPU > maxCPU {
		t.Errorf("server churn CPU %.1f seconds exceeds budget %.1f seconds", serverCPU, maxCPU)
	}
	t.Logf("fleet process: RSS=%.1f MiB, %.1f CPU-seconds during the churn",
		readProcessRSSMiB(host.Process.Pid), readProcessCPU(host.Process.Pid)-hostBefore)

	lost, err := fleet.OfflineNodes(client)
	if err != nil {
		t.Fatalf("read the fleet after the churn: %v", err)
	}
	for _, problem := range report.Problems(lost) {
		t.Error(problem)
	}
}

type fleetRosterEntry struct {
	Name  string `json:"name"`
	UUID  string `json:"uuid"`
	Token string `json:"token"`
	Churn bool   `json:"churn"`
}

// fleetBinary uses a prebuilt binary when one is named, and builds it
// otherwise. A machine with no Go toolchain - which is where a fleet this size
// actually fits - can only run the case the first way.
func fleetBinary(t *testing.T, envName, pkg string) string {
	t.Helper()
	if path := strings.TrimSpace(os.Getenv(envName)); path != "" {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s=%s: %v", envName, path, err)
		}
		return path
	}
	return buildBinary(t, pkg)
}

// fleetWorkRoot keeps the run's files where the operator asked, so a run on a
// shared machine writes only inside the directory it was given, and so a failed
// run can be examined after the test process is gone.
func fleetWorkRoot(t *testing.T) string {
	t.Helper()
	if dir := strings.TrimSpace(os.Getenv("GPUWORKS_FLEET_WORK_DIR")); dir != "" {
		run := filepath.Join(dir, "run-"+strconv.FormatInt(time.Now().Unix(), 10))
		if err := os.MkdirAll(run, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Logf("run files in %s", run)
		return run
	}
	return t.TempDir()
}

func fleetProcessLog(t *testing.T, dir, name string) (io.Writer, io.Writer) {
	t.Helper()
	if os.Getenv("GPUWORKS_FLEET_PROCESS_LOGS") != "1" {
		return io.Discard, io.Discard
	}
	file, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file, file
}

func fleetLogin(t *testing.T, base string) *fleet.Client {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		client, err := fleet.Login(base, "admin", fleetAdminPassword)
		if err == nil {
			return client
		}
		if time.Now().After(deadline) {
			t.Fatalf("log in to the control plane at %s: %v", base, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never appeared within %s", path, timeout)
}

// fleetDatabaseBytes is the control-plane database plus its write-ahead log and
// shared-memory file, which are part of what it costs on disk.
func fleetDatabaseBytes(path string) int64 {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if info, err := os.Stat(path + suffix); err == nil {
			total += info.Size()
		}
	}
	return total
}

func startFleetGroup(t *testing.T, hostBin, base, workRoot, modelDir, nextRosterPath string, roster []fleetRosterEntry) *exec.Cmd {
	t.Helper()
	rosterPath := filepath.Join(workRoot, "roster.json")
	writeFleetRoster(t, rosterPath, roster)
	readyPath := filepath.Join(workRoot, "fleet.ready")
	// GPUWORKS_FLEET_GPUS sets the cards each node reports (default 1): every
	// card adds its checks to every status report.
	gpus := strconv.Itoa(fleet.EnvInt("GPUWORKS_FLEET_GPUS", 1))
	host := exec.Command(hostBin, "--roster", rosterPath, "--next-roster", nextRosterPath, "--server", base, "--state-root", filepath.Join(workRoot, "agents"), "--model-dir", modelDir, "--ready", readyPath, "--gpus", gpus)
	host.Env = os.Environ()
	if fleet.GPUReadsEnabled() {
		// Reading the GPU endpoint also requires the deterministic samples
		// checked by WaitGPUInventoryMetrics; do not depend on a second opt-in.
		host.Env = append(host.Env, "GPUWORKS_FLEET_GPU_METRICS=1")
	}
	host.Stdout, host.Stderr = fleetProcessLog(t, workRoot, "fleet-host.log")
	if err := host.Start(); err != nil {
		t.Fatalf("start the fleet: %v", err)
	}
	t.Cleanup(func() { stopProcess(host) })
	waitForFile(t, readyPath, time.Minute)
	return host
}

func writeFleetRoster(t *testing.T, path string, roster []fleetRosterEntry) {
	t.Helper()
	encoded, err := json.Marshal(roster)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp", encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
}

func checkFleetReadProbe(t *testing.T, probe *fleet.ListReadProbe) {
	t.Helper()
	lines, problems := probe.Stop()
	for _, line := range lines {
		t.Log(line)
	}
	for _, problem := range problems {
		t.Error(problem)
	}
}
