//go:build cloudgpu

package l4

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
	"gpuworks.ai/gpuworks/internal/testenv/sshutil"
)

const cloudSetupTimeout = 30 * time.Minute
const cloudTestLifetime = 120 * time.Minute
const cloudRetryGrace = 45 * time.Second

// cloudNodeOfflineGrace bounds how long a startup wait tolerates the node
// being offline. The SSH tunnels reconnect within seconds and the agent redials
// every 30s, so an offline node past this is a lost connection to report, not
// a slow engine start to keep waiting for.
const cloudNodeOfflineGrace = 5 * time.Minute

// skipOrFailExternal prevents strict cloud jobs from reporting green when a
// required external capability is absent. Local exploratory runs retain the
// useful skip behavior.
func skipOrFailExternal(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

type preflightProviderFactory func(map[string]string) (cloudprovider.CloudProvider, error)

var preflightProviderFactories []preflightProviderFactory

func TestMain(m *testing.M) {
	secrets := readCloudSecrets()
	runID := cloudTestRunID()
	var providers []cloudprovider.CloudProvider
	for _, factory := range preflightProviderFactories {
		if p, err := factory(secrets); err == nil && p != nil {
			providers = append(providers, p)
		}
	}
	for _, provider := range providers {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		err := cleanupLeaked(ctx, provider, runID)
		cancel()
		if err != nil && os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

func cleanupLeaked(ctx context.Context, provider cloudprovider.CloudProvider, runID string) error {
	if strings.TrimSpace(runID) == "" {
		return nil
	}
	lister, ok := provider.(cloudprovider.ScopedLeakLister)
	if !ok {
		return nil
	}
	leaked, err := lister.ListLeakedForRunID(ctx, runID)
	if err != nil {
		return fmt.Errorf("list leaked %s instances: %w", provider.Name(), err)
	}
	for _, id := range leaked {
		if err := provider.Destroy(ctx, id); err != nil {
			return fmt.Errorf("destroy leaked %s instance %s: %w", provider.Name(), id, err)
		}
	}
	return nil
}

func cloudTestRunID() string {
	return strings.TrimSpace(os.Getenv("GPUWORKS_CLOUD_TEST_RUN_ID"))
}

type cloudTestEnv struct {
	Control    *testenv.Env
	Provider   cloudprovider.CloudProvider
	Instance   *cloudprovider.Instance
	NodeUUID   string
	NodeToken  string
	Tunnel     *sshutil.ServerDialTunnel
	Forward    *sshutil.RemoteForward
	destroy    sync.Once
	destroyErr error
}

var cloudDebugCredentialsMu sync.Mutex

type cloudDebugCredential struct {
	Provider   string `json:"provider"`
	RunID      string `json:"run_id"`
	InstanceID string `json:"instance_id"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Username   string `json:"username"`
	Password   string `json:"password,omitempty"`
	PrivateKey []byte `json:"private_key,omitempty"`
}

func (e *cloudTestEnv) cleanup(ctx context.Context) error {
	e.destroy.Do(func() {
		if e.Tunnel != nil {
			_ = e.Tunnel.Close()
		}
		if e.Forward != nil {
			_ = e.Forward.Close()
		}
		e.destroyErr = e.Provider.Destroy(ctx, e.Instance.InstanceID)
		// The server registered its platform key with the provider account
		// (RunPod); take it off again so runs do not pile keys up there.
		if forgetter, ok := e.Provider.(interface {
			ForgetAccountSSHKey(context.Context, string) error
		}); ok && e.Control != nil {
			if row, _, err := e.Control.Store().PlatformSSHKey(); err == nil {
				if err := forgetter.ForgetAccountSSHKey(ctx, row.PublicKey); err != nil && e.destroyErr == nil {
					e.destroyErr = fmt.Errorf("remove platform key from the provider account: %w", err)
				}
			}
		}
	})
	return e.destroyErr
}

func setupCloudTest(t *testing.T, p cloudprovider.CloudProvider, spec cloudprovider.GPUSpec, opts ...cloudSetupOption) *cloudTestEnv {
	t.Helper()
	return setupCloudNodeWithInstance(t, testenv.New(t), p, spec, nil, opts...)
}

func setupCloudNode(t *testing.T, env *testenv.Env, p cloudprovider.CloudProvider, spec cloudprovider.GPUSpec) *cloudTestEnv {
	return setupCloudNodeWithInstance(t, env, p, spec, nil)
}

// cloudSetupOption changes how setupCloudNodeWithInstance installs the agent.
type cloudSetupOption func(*cloudSetup)

type cloudSetup struct {
	serverDial bool
}

// withServerDial installs the agent through the provider API with the server
// dialing it: no forward to the server, and the driver chooses the agent's
// port from the instance's mappings.
func withServerDial() cloudSetupOption { return func(s *cloudSetup) { s.serverDial = true } }

// setupCloudNodeWithInstance wires the node lifecycle around an instance that
// may already have been rented by the caller (a nil instance is rented here).
// Callers that probe several machine profiles before committing pass their
// own rented instance so the provider is not charged twice.
func setupCloudNodeWithInstance(t *testing.T, env *testenv.Env, p cloudprovider.CloudProvider, spec cloudprovider.GPUSpec, rented *cloudprovider.Instance, opts ...cloudSetupOption) *cloudTestEnv {
	t.Helper()
	var setup cloudSetup
	for _, opt := range opts {
		opt(&setup)
	}
	lifetimeCtx, lifetimeCancel := context.WithTimeout(context.Background(), cloudTestLifetime)
	t.Cleanup(lifetimeCancel)
	// A provider that waits out a stock-out inside Rent (nothing is rented
	// meanwhile) gets that wait on top of the setup budget, so waiting for
	// capacity never eats the time the rented machine needs to come up.
	setupBudget := cloudSetupTimeout
	if waiter, ok := p.(cloudprovider.StockWaiter); ok && rented == nil {
		setupBudget += waiter.StockWait()
	}
	setupCtx, setupCancel := context.WithTimeout(lifetimeCtx, setupBudget)
	defer setupCancel()
	instance := rented
	if instance == nil {
		var err error
		spec.ServerDialPort = setup.serverDial
		instance, err = p.Rent(setupCtx, spec)
		if err != nil {
			if isCloudCapacityError(err) && topologyCloudTest(t) {
				skipOrFailExternal(t, "cloud capacity unavailable for %s (%s): %v", p.Name(), spec.GPUModel, err)
			}
			t.Fatal(err)
		}
	}
	state := &cloudTestEnv{Control: env, Provider: p, Instance: instance}
	t.Cleanup(func() {
		reportInterruption(t, p, instance)
		if t.Failed() && os.Getenv("GPUWORKS_CLOUD_KEEP_ON_FAILURE") == "1" {
			credentialPath, credentialErr := saveCloudDebugCredential(p, instance)
			if credentialErr != nil {
				t.Errorf("save credentials for preserved cloud instance: %v", credentialErr)
			}
			t.Logf("GPUWORKS_CLOUD_KEEP_ON_FAILURE=1: leaving failed instance %s (%s) running for investigation; "+
				"SSH endpoint %s@%s:%d; credentials file %s (0600). It is NOT automatically destroyed after go test exits; "+
				"run cleanup-leaked --run-id %s after debugging", instance.InstanceID, p.Name(), instance.Username,
				instance.PublicIP, instance.SSHPort, credentialPath, cloudProviderDebugRunID(p))
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		if err := state.cleanup(cleanupCtx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("destroy cloud instance: %v", err)
		}
	})
	go func() {
		<-lifetimeCtx.Done()
		if lifetimeCtx.Err() == context.DeadlineExceeded {
			forceCtx, forceCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer forceCancel()
			_ = state.cleanup(forceCtx)
		}
	}()
	if err := waitCloudSSH(setupCtx, instance); err != nil {
		if isCloudCapacityError(err) && topologyCloudTest(t) {
			skipOrFailExternal(t, "cloud SSH endpoint unavailable for %s (%s): %v", p.Name(), spec.GPUModel, err)
		}
		t.Fatal(err)
	}
	name := cloudprovider.TestNamePrefix + "node"
	management, hasManagement := p.(cloudprovider.ManagementProvider)
	if setup.serverDial {
		if !hasManagement || !hasProviderCredentials(management) {
			t.Fatalf("a server-dialed install goes through the %s provider API, which has no credentials here", p.Name())
		}
		var err error
		var report providerInstallReport
		state.NodeUUID, err = installCloudAgentViaProviderAPI(setupCtx, t, env, p, instance, "", &report)
		if err != nil {
			t.Fatalf("install server-dialed cloud agent: %v", err)
		}
		if report.QueuedDial != "" {
			// The install's own dial missed and the server's redial loop owns
			// the node now. Require it to get through in bounded time, so an
			// address that never works fails here with the dial error rather
			// than as a generic "not online" later.
			t.Logf("server_dial install queued for reconnection (%s); waiting up to %s for the server to reach the node", report.QueuedDial, serverDialQueuedOnlineTimeout)
			nodeState := func() (string, error) {
				node, err := env.Store().NodeByUUID(state.NodeUUID)
				if err != nil {
					return "", err
				}
				return node.State, nil
			}
			probe := func() string {
				node, err := env.Store().NodeByUUID(state.NodeUUID)
				if err != nil {
					return "node unreadable: " + err.Error()
				}
				return probeServerDialAddress(node.ServerDialAddress)
			}
			if err := waitQueuedDialNodeOnline(setupCtx, serverDialQueuedOnlineTimeout, time.Second, nodeState, probe, report.QueuedDial); err != nil {
				t.Fatal(err)
			}
		}
	} else if hasManagement && hasProviderCredentials(management) {
		var err error
		state.Forward, err = openServerForward(lifetimeCtx, instance, env.URL())
		if err != nil {
			t.Fatal(err)
		}
		// Reconnects of the forward are part of the run's evidence. The logger
		// is detached before the test ends, also when a failed instance is
		// kept and the forward is left open.
		state.Forward.SetLogf(t.Logf)
		t.Cleanup(func() { state.Forward.SetLogf(nil) })
		if err := verifyCloudServerForward(setupCtx, instance, state.Forward.URL()); err != nil {
			t.Fatalf("verify cloud server forward: %v", err)
		}
		state.NodeUUID, err = installCloudAgentViaProviderAPI(setupCtx, t, env, p, instance, state.Forward.URL(), nil)
		if err != nil {
			if topologyCloudTest(t) && isCloudCapacityError(err) {
				skipOrFailExternal(t, "cloud SSH session unavailable during agent install: %v", err)
			}
			t.Fatalf("install cloud agent: %v", err)
		}
	} else {
		state.NodeUUID, state.NodeToken = env.CreatePendingNode(t, name)
		tunnel, err := openServerDialTunnel(lifetimeCtx, instance)
		if err != nil {
			t.Fatal(err)
		}
		state.Tunnel = tunnel
		tunnel.SetLogf(t.Logf)
		t.Cleanup(func() { tunnel.SetLogf(nil) })
		if err := installCloudAgent(setupCtx, t.TempDir(), instance, env.URL(), state.NodeToken); err != nil {
			if topologyCloudTest(t) && isCloudCapacityError(err) {
				skipOrFailExternal(t, "cloud SSH session unavailable during agent install: %v", err)
			}
			t.Fatalf("install cloud agent: %v", err)
		}
		if err := dialCloudAgent(setupCtx, env, state.Tunnel.Addr(), state.NodeToken); err != nil {
			t.Fatal(err)
		}
	}
	if state.NodeUUID != "" {
		env.WaitNodeOnlineUUID(t, state.NodeUUID)
		return state
	}
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if nodes, err := env.Store().ListNodes(); err == nil {
			for _, node := range nodes {
				if node.Name == name {
					state.NodeUUID = node.UUID
					break
				}
			}
			if state.NodeUUID != "" {
				break
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	if state.NodeUUID == "" {
		t.Fatal("one-click agent install did not create a node")
	}
	deadline = time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if node, err := env.Store().NodeByUUID(state.NodeUUID); err == nil && node.State == store.NodeStateOnline {
			return state
		}
		time.Sleep(500 * time.Millisecond)
	}
	remoteOut, remoteErr := runCloudCommand(t.Context(), instance, "set +e; echo '== agent process =='; ps -eo pid=,comm= | grep -E 'gpuworks-agent|curl' | grep -v grep; echo '== state =='; find /tmp/gpuworks /opt/gpuworks -maxdepth 2 -type f -printf '%p %s bytes\\n' 2>/dev/null | sort | head -80; echo '== agent log =='; tail -n 160 /tmp/gpuworks/agent.log /opt/gpuworks/agent.log 2>&1")
	t.Fatalf("cloud node %s did not become online; ssh diagnostics (err=%v):\\n%s", state.NodeUUID, remoteErr, strings.TrimSpace(remoteOut))
	return state
}

func saveCloudDebugCredential(provider cloudprovider.CloudProvider, instance *cloudprovider.Instance) (string, error) {
	runID := cloudProviderDebugRunID(provider)
	path := strings.TrimSpace(os.Getenv("GPUWORKS_CLOUD_DEBUG_CREDENTIALS_FILE"))
	if path == "" {
		path = fmt.Sprintf("/tmp/gpuworks-cloud-debug-%s.json", safeDebugFilename(runID))
	} else if info, err := os.Stat(path); err == nil && info.IsDir() {
		path = fmt.Sprintf("%s/gpuworks-cloud-debug-%s.json", strings.TrimRight(path, "/"), safeDebugFilename(runID))
	}
	record := cloudDebugCredential{
		Provider: provider.Name(), RunID: runID, InstanceID: instance.InstanceID,
		Host: instance.PublicIP, Port: instance.SSHPort, Username: instance.Username,
		Password: instance.Password, PrivateKey: instance.PrivateKey,
	}

	cloudDebugCredentialsMu.Lock()
	defer cloudDebugCredentialsMu.Unlock()
	var records []cloudDebugCredential
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &records)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	for index := range records {
		if records[index].Provider == record.Provider && records[index].InstanceID == record.InstanceID {
			records[index] = record
			return path, writeCloudDebugCredentials(path, records)
		}
	}
	records = append(records, record)
	return path, writeCloudDebugCredentials(path, records)
}

func writeCloudDebugCredentials(path string, records []cloudDebugCredential) error {
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func cloudProviderDebugRunID(provider cloudprovider.CloudProvider) string {
	if identified, ok := provider.(interface{ RunID() string }); ok {
		if runID := strings.TrimSpace(identified.RunID()); runID != "" {
			return runID
		}
	}
	if runID := cloudTestRunID(); runID != "" {
		return runID
	}
	return "unknown-run"
}

func safeDebugFilename(value string) string {
	var out strings.Builder
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' {
			out.WriteRune(char)
		} else {
			out.WriteByte('_')
		}
	}
	if out.Len() == 0 {
		return "unknown-run"
	}
	return out.String()
}

func requireOnlineNode(t *testing.T, env *cloudTestEnv) {
	t.Helper()
	var state string
	var gpuCount int
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		node, err := env.Control.Store().NodeByUUID(env.NodeUUID)
		if err == nil {
			state, gpuCount = node.State, node.GPUCount
			if state == "online" && gpuCount > 0 {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	diagnostic, diagErr := runCloudCommand(t.Context(), env.Instance, "command -v nvidia-smi || true; nvidia-smi -L 2>&1 || true")
	if diagErr != nil {
		diagnostic = fmt.Sprintf("diagnostic failed: %v", diagErr)
	}
	t.Fatalf("external node did not report GPUs: state=%q gpu_count=%d diagnostic=%q", state, gpuCount, strings.TrimSpace(diagnostic))
}

func runCloudCommand(ctx context.Context, instance *cloudprovider.Instance, command string) (string, error) {
	if instance.Password != "" {
		return sshutil.RunCommandPassword(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.Password, command)
	}
	return sshutil.RunCommand(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.PrivateKey, command)
}

// waitDeploymentInstance keeps long cloud startup operations observable. A
// failure includes the control-plane timeline and a best-effort SSH snapshot
// from the rented host, so a slow image/model/engine phase is diagnosable.
func waitDeploymentInstance(t *testing.T, env *cloudTestEnv, deploymentID uint, timeout time.Duration) store.Instance {
	t.Helper()
	deadline := time.Now().Add(timeout)
	nextLog := time.Now()
	var noActiveSince, offlineSince time.Time
	for time.Now().Before(deadline) {
		rows, _ := env.Control.Store().InstancesForDeployment(deploymentID)
		inst, failed := currentCloudStartupAttempt(rows)
		nodeState := "unknown"
		if node, err := env.Control.Store().NodeByUUID(env.NodeUUID); err == nil {
			nodeState = node.State
		}
		if nodeState == store.NodeStateOffline {
			if offlineSince.IsZero() {
				offlineSince = time.Now()
				t.Logf("cloud startup: node %s went offline", env.NodeUUID)
			}
			if time.Since(offlineSince) >= cloudNodeOfflineGrace {
				latest := inst
				if latest == nil && len(rows) > 0 {
					latest = &rows[0]
				}
				reportCloudStartupFailure(t, env, deploymentID, fmt.Sprintf("node %s has been offline for %s; the agent lost its connection to the server", env.NodeUUID, cloudNodeOfflineGrace), latest)
			}
		} else if !offlineSince.IsZero() {
			t.Logf("cloud startup: node %s is %s again after %s offline", env.NodeUUID, nodeState, time.Since(offlineSince).Round(time.Second))
			offlineSince = time.Time{}
		}
		if inst != nil {
			noActiveSince = time.Time{}
			if inst.State == proto.InstRunning && inst.ContainerID != "" {
				return *inst
			}
			if time.Now().After(nextLog) {
				t.Logf("cloud startup: attempt=%d failed_attempts=%d instance=%s state=%s phase=%s progress=%.1f%% bytes=%d/%d rate=%d message=%q", len(rows), failed, inst.UUID, inst.State, inst.Phase, inst.Progress*100, inst.BytesDone, inst.BytesTotal, inst.RateBPS, inst.Message)
				nextLog = time.Now().Add(15 * time.Second)
			}
		} else if len(rows) > 0 {
			if noActiveSince.IsZero() {
				noActiveSince = time.Now()
			}
			if time.Since(noActiveSince) >= cloudRetryGrace {
				reportCloudStartupFailure(t, env, deploymentID, fmt.Sprintf("all %d startup attempts are terminal and no retry appeared within %s", len(rows), cloudRetryGrace), &rows[0])
			}
			if time.Now().After(nextLog) {
				t.Logf("cloud startup: waiting for retry after %d failed attempts; latest=%s message=%q", failed, rows[0].UUID, rows[0].Message)
				nextLog = time.Now().Add(15 * time.Second)
			}
		} else if time.Now().After(nextLog) {
			t.Logf("cloud startup: no instance yet; node=%s %s", nodeState, deploymentModelProgress(env, deploymentID))
			nextLog = time.Now().Add(15 * time.Second)
		}
		time.Sleep(2 * time.Second)
	}
	rows, _ := env.Control.Store().InstancesForDeployment(deploymentID)
	inst, _ := currentCloudStartupAttempt(rows)
	if inst == nil && len(rows) > 0 {
		inst = &rows[0]
	}
	reportCloudStartupFailure(t, env, deploymentID, fmt.Sprintf("startup did not finish within %s", timeout), inst)
	return store.Instance{}
}

// InstancesForDeployment is newest-first and retains failed attempts so the
// controller can enforce its retry cap. Follow the newest non-terminal row
// instead of treating multiple rows as if no instance existed.
func currentCloudStartupAttempt(rows []store.Instance) (*store.Instance, int) {
	failed := 0
	var current *store.Instance
	for i := range rows {
		switch rows[i].State {
		case proto.InstFailed:
			failed++
		case proto.InstStopped, "pending-removal":
		default:
			if current == nil {
				current = &rows[i]
			}
		}
	}
	return current, failed
}

func TestCurrentCloudStartupAttempt(t *testing.T) {
	tests := []struct {
		name       string
		rows       []store.Instance
		wantUUID   string
		wantFailed int
	}{
		{
			name:       "single active instance",
			rows:       []store.Instance{{UUID: "active", State: proto.InstStarting}},
			wantUUID:   "active",
			wantFailed: 0,
		},
		{
			name: "new active instance after failed attempt",
			rows: []store.Instance{
				{UUID: "retry", State: proto.InstPending},
				{UUID: "failed", State: proto.InstFailed},
			},
			wantUUID:   "retry",
			wantFailed: 1,
		},
		{
			name: "all attempts failed",
			rows: []store.Instance{
				{UUID: "failed-2", State: proto.InstFailed},
				{UUID: "failed-1", State: proto.InstFailed},
			},
			wantFailed: 2,
		},
		{
			name: "terminal cleanup rows around active instance",
			rows: []store.Instance{
				{UUID: "removing", State: "pending-removal"},
				{UUID: "active", State: proto.InstStarting},
				{UUID: "stopped", State: proto.InstStopped},
				{UUID: "failed", State: proto.InstFailed},
			},
			wantUUID:   "active",
			wantFailed: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, failed := currentCloudStartupAttempt(tt.rows)
			if failed != tt.wantFailed {
				t.Fatalf("failed attempts = %d, want %d", failed, tt.wantFailed)
			}
			if tt.wantUUID == "" {
				if got != nil {
					t.Fatalf("current attempt = %s, want nil", got.UUID)
				}
				return
			}
			if got == nil || got.UUID != tt.wantUUID {
				gotUUID := "<nil>"
				if got != nil {
					gotUUID = got.UUID
				}
				t.Fatalf("current attempt = %s, want %s", gotUUID, tt.wantUUID)
			}
		})
	}
}

func deploymentModelProgress(env *cloudTestEnv, deploymentID uint) string {
	dep, err := env.Control.Store().GetDeployment(deploymentID)
	if err != nil {
		return "deployment=" + err.Error()
	}
	mf, err := env.Control.Store().ModelFileByModelNode(dep.ModelID, dep.NodeID)
	if err != nil {
		return "model_file=" + err.Error()
	}
	return fmt.Sprintf("model state=%s downloaded=%d/%d transferred=%d rate=%d message=%q", mf.State, mf.Downloaded, mf.SizeBytes, mf.TransferredBytes, mf.RateBPS, mf.Message)
}

func reportCloudStartupFailure(t *testing.T, env *cloudTestEnv, deploymentID uint, reason string, inst *store.Instance) {
	t.Helper()
	rows, _ := env.Control.Store().InstancesForDeployment(deploymentID)
	containerLogs := cloudDeploymentLogDiagnostics(t, env, rows)
	var attempts []string
	for i := range rows {
		attempts = append(attempts, fmt.Sprintf("%d. uuid=%s state=%s phase=%s container=%s message=%q", len(rows)-i, rows[i].UUID, rows[i].State, rows[i].Phase, rows[i].ContainerID, rows[i].Message))
	}
	attemptSummary := strings.Join(attempts, "\n")
	var snapshot string
	if inst != nil {
		spec := inst.DesiredStateJSON
		var decoded any
		if json.Unmarshal([]byte(spec), &decoded) == nil {
			encoded, _ := json.Marshal(decoded)
			spec = string(encoded)
		}
		events, _ := env.Control.Store().InstanceEvents(inst.ID, 500)
		var timeline []string
		for _, event := range events {
			timeline = append(timeline, event.CreatedAt.Format(time.RFC3339)+" "+event.Kind+": "+event.Message)
		}
		node, _ := env.Control.Store().NodeByID(inst.NodeID)
		remote := cloudStartupCommand(inst)
		remoteOut, remoteErr := runCloudCommand(t.Context(), env.Instance, remote)
		snapshot = fmt.Sprintf("instance uuid=%s state=%s phase=%s progress=%.3f bytes=%d/%d rate=%d container=%s message=%q\nnode=%s gpu_count=%d\nspec=%s\nattempts:\n%s\nevents:\n%s\n%s\nssh diagnostics (err=%v):\n%s", inst.UUID, inst.State, inst.Phase, inst.Progress, inst.BytesDone, inst.BytesTotal, inst.RateBPS, inst.ContainerID, inst.Message, nodeState(node), nodeGPUCount(node), spec, attemptSummary, strings.Join(timeline, "\n"), containerLogs, remoteErr, strings.TrimSpace(remoteOut))
	} else {
		remoteOut, remoteErr := runCloudCommand(t.Context(), env.Instance, cloudStartupCommand(nil))
		modelState := "unknown"
		if dep, err := env.Control.Store().GetDeployment(deploymentID); err == nil {
			if mf, err := env.Control.Store().ModelFileByModelNode(dep.ModelID, dep.NodeID); err == nil {
				modelState = fmt.Sprintf("state=%s downloaded=%d/%d transferred=%d rate=%d path=%q message=%q", mf.State, mf.Downloaded, mf.SizeBytes, mf.TransferredBytes, mf.RateBPS, mf.Path, mf.Message)
			} else {
				modelState = "model file missing: " + err.Error()
			}
		}
		snapshot = fmt.Sprintf("no instance row for deployment %d; model=%s\n%s\nssh diagnostics (err=%v):\n%s", deploymentID, modelState, containerLogs, remoteErr, strings.TrimSpace(remoteOut))
	}
	t.Fatalf("%s\n%s", reason, snapshot)
}

// cloudDeploymentLogDiagnostics reads the durable server-side logstore. This
// remains available after the controller removes an earlier failed container
// and its local udocker log while preparing a retry.
func cloudDeploymentLogDiagnostics(t *testing.T, env *cloudTestEnv, rows []store.Instance) string {
	t.Helper()
	if env == nil || env.Control == nil || len(rows) == 0 {
		return "container logs: no instance rows"
	}
	seen := make(map[string]struct{}, len(rows))
	ids := make([]string, 0, len(rows))
	// InstancesForDeployment is newest-first; report attempts oldest-first.
	for index := len(rows) - 1; index >= 0; index-- {
		id := strings.TrimSpace(rows[index].UUID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return "container logs: instance UUIDs are empty"
	}

	api := env.Control.API(t)
	var result strings.Builder
	result.WriteString("container logs from server logstore:")
	for _, id := range ids {
		path := "/api/v1/logs/search?instance=" + url.QueryEscape(id) + "&limit=400"
		resp, err := api.Get(path)
		if err != nil {
			fmt.Fprintf(&result, "\n--- instance %s query failed: %v", id, err)
			continue
		}
		var page apiwire.LogsPage
		if resp.StatusCode == http.StatusOK {
			err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&page)
		} else {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
			err = fmt.Errorf("status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		resp.Body.Close()
		if err != nil {
			fmt.Fprintf(&result, "\n--- instance %s decode failed: %v", id, err)
			continue
		}
		entries := page.Items
		fmt.Fprintf(&result, "\n--- instance %s (%d lines)", id, len(entries))
		// The search endpoint is newest-first; logs read naturally oldest-first.
		for index := len(entries) - 1; index >= 0; index-- {
			line := strings.TrimSpace(entries[index].Line)
			if line != "" {
				result.WriteByte('\n')
				result.WriteString(line)
			}
		}
	}
	return result.String()
}

func nodeState(node *store.Node) string {
	if node == nil {
		return "unknown"
	}
	return node.State
}

func nodeGPUCount(node *store.Node) int {
	if node == nil {
		return 0
	}
	return len(node.GPUs)
}

func cloudStartupCommand(inst *store.Instance) string {
	container := ""
	if inst != nil {
		container = "gpuworks-" + inst.UUID
		if len(inst.UUID) > 12 {
			container = "gpuworks-" + inst.UUID[:12]
		}
	}
	quotedContainer := shellQuoteForTest(container)
	return fmt.Sprintf(`set +e
container=%s
echo '== docker =='
if command -v docker >/dev/null; then
  docker ps -a --no-trunc --format '{{.ID}} {{.Names}} {{.Status}} {{.Image}}'
  docker images --format '{{.Repository}}:{{.Tag}} {{.Size}}' | head -30
  if [ -n "$container" ]; then docker inspect "$container"; docker logs --tail 200 "$container" 2>&1; fi
else
  echo 'docker unavailable'
fi
echo '== udocker state =='
found_state=0
for state in /data/udocker-state /opt/gpuworks/udocker-state /tmp/gpuworks/udocker-state "$HOME/.config/gpuworks/udocker-state"; do
  [ -d "$state" ] || continue
  found_state=1
  echo "state_dir=$state"
  if [ -n "$container" ]; then
    for suffix in log pid spec.json; do
      file="$state/$container.$suffix"
      [ -f "$file" ] || continue
      echo "--- $file"
      if [ "$suffix" = log ]; then tail -n 200 "$file"; else sed -n '1,160p' "$file"; fi
    done
  fi
  find "$state" -maxdepth 1 -type f \( -name '*.log' -o -name '*.pid' -o -name '*.spec.json' \) -printf 'state_file=%%p size=%%s\n' 2>/dev/null | sort
done
[ "$found_state" -eq 1 ] || echo 'udocker state unavailable'
echo '== processes =='
ps -eo pid=,comm= | grep -E 'vllm|sglang|gpuworks|python|udocker' | grep -v grep
echo '== gpu =='
if command -v npu-smi >/dev/null; then npu-smi info 2>&1; elif command -v nvidia-smi >/dev/null; then nvidia-smi 2>&1; else echo 'GPU management CLI unavailable'; fi
echo '== model dirs =='
du -sh /data/models /models /workspace /opt/gpuworks /tmp/gpuworks "$HOME/.config/gpuworks" 2>&1
echo '== agent logs =='
journalctl -u gpuworks-agent --no-pager -n 200 2>&1 || true
tail -n 200 /data/agent.log /opt/gpuworks/agent.log /tmp/gpuworks/agent.log /var/log/gpuworks-agent.log 2>&1 || true`, quotedContainer)
}

func TestCloudStartupCommandDoesNotExposeProcessArguments(t *testing.T) {
	command := cloudStartupCommand(&store.Instance{UUID: "0123456789abcdef"})
	if output, err := exec.Command("sh", "-n", "-c", command).CombinedOutput(); err != nil {
		t.Fatalf("cloud startup diagnostics are not valid shell: %v: %s", err, output)
	}
	if strings.Contains(command, "ps aux") || !strings.Contains(command, "ps -eo pid=,comm=") {
		t.Fatalf("cloud startup diagnostics must list process names without argv: %s", command)
	}
	for _, required := range []string{"gpuworks-0123456789ab", "/data/udocker-state", "npu-smi info", "nvidia-smi", "/data/agent.log"} {
		if !strings.Contains(command, required) {
			t.Fatalf("cloud startup diagnostics missing %q: %s", required, command)
		}
	}
}

func shellQuoteForTest(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
