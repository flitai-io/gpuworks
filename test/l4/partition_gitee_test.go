//go:build cloudgpu && partition

package l4

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/partitioncap"
	"gpuworks.ai/gpuworks/internal/testenv"
	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
)

type giteePartitionMatrixEntry struct {
	name, zone, model, image, frameworkVersion string
	// archs carries every architecture the profile may boot as, in the
	// accelerator.NormalizeArch spelling (x86_64, arm64); the observed
	// node must match one of them. Gitee publishes per-model image lists but
	// no architecture field, and fallback models share images across arches.
	archs                                         []string
	wantRuntime, wantProbeStatus, wantProbeDetail string
	wantReady                                     bool
}

// TestGiteeAscendPartitionCapability rents each Ascend profile and checks the
// dGPU capability verdict. Stock-out handling: GITEE_STOCK_WAIT (default 30m,
// "0" disables) is how long it keeps retrying the whole matrix when every
// profile's create reports no available host.
//
//	GPUWORKS_TEST_STRICT=1 go test -v -tags=cloudgpu,partition -run '^TestGiteeAscendPartitionCapability$' -timeout=60m ./test/l4
func TestGiteeAscendPartitionCapability(t *testing.T) {
	secrets := skipIfNoSecrets(t, "GITEE_API_TOKEN")
	runID, err := cloudProviderRunID()
	if err != nil {
		t.Fatal(err)
	}
	// Every profile is ordered coverage with an availability escape: Gitee
	// keeps sold-out models listed in /compute/specs and only the create call
	// reveals host supply, so a profile whose create returns "没有找到符合条件
	// 的可用主机" is skipped this run and the next profile carries the signal.
	// 910B2C was rented out across 2026-10-02..03 while 910B2 stayed listable.
	matrix := []giteePartitionMatrixEntry{
		{
			name: "CANN8_5_x86_64_910B2C", zone: "gitee", model: "Ascend 910B2C", archs: []string{"x86_64"}, frameworkVersion: "8.5",
			image:       "easzlab.io.local:5000/ascend/torch-npu:2.9.0-CANN-8.5.1-py311-openeuler24.03",
			wantRuntime: "udocker", wantProbeStatus: proto.PartitionProbeFailed,
			wantProbeDetail: "only root user on physical machine",
		},
		{
			// The CANN9 fallback profile: same outer-container boundary, same
			// unprivileged vcann-rt probe verdict expected under udocker/PRoot.
			// Gitee offers this image for both 910B2C and 910B2, and the 910B2
			// arch is not advertised, so both arches are accepted here and the
			// observed one is recorded into the evidence file.
			name: "CANN9_910B2", zone: "gitee", model: "Ascend 910B2", archs: []string{"x86_64", "arm64"}, frameworkVersion: "9",
			image:           "easzlab.io.local:5000/metax/torch-npu:2.10.0-910b-openeuler24.03-py3.11",
			wantRuntime:     "udocker",
			wantProbeStatus: proto.PartitionProbeFailed,
			wantProbeDetail: "only root user on physical machine",
		},
		// x86_64/CANN9 and arm64/CANN9 serving coverage is asserted by the two
		// full deploy+chat cases. Gitee lists a CANN8.5 arm64 image but rejects
		// it as an outer rental with "镜像和主机不匹配"; the inner CANN8.5/PRoot
		// limitation is recorded separately rather than mislabeled as passing
		// coverage.
	}
	// Gitee's only stock signal is the create call, so when every profile
	// reports no available host the whole matrix is retried every few minutes
	// until the wait budget runs out. Nothing is rented while waiting: a
	// refused create makes no instance.
	wait, err := cloudprovider.StockWaitFromEnv("GITEE_STOCK_WAIT", cloudprovider.DefaultStockWait)
	if err != nil {
		t.Fatal(err)
	}
	rented, err := cloudprovider.RetryOnStockOut(t.Context(), wait, "gitee Ascend",
		func(err error) bool { return errors.Is(err, errGiteeNoHostAnywhere) },
		func(ctx context.Context) (bool, error) {
			return giteePartitionRound(ctx, t, secrets["GITEE_API_TOKEN"], runID, matrix)
		})
	if err != nil {
		skipOrFailExternal(t, "no Gitee Ascend profile rentable: %v", err)
	}
	if !rented {
		skipOrFailExternal(t, "no Gitee Ascend profile rentable: every profile's create failed for a reason other than missing hosts")
	}
}

var errGiteeNoHostAnywhere = errors.New("every candidate create reported no available host")

// giteePartitionRound rents each profile once. A profile whose create fails
// is skipped while another carries the signal. Only when no profile is
// rentable and every refusal was "no available host" does it return
// errGiteeNoHostAnywhere, which the caller waits out; the skipped profiles
// are reported as subtests only on a round that is final, so waited-out
// rounds leave no repeated subtests.
func giteePartitionRound(ctx context.Context, t *testing.T, token, runID string, matrix []giteePartitionMatrixEntry) (bool, error) {
	t.Helper()
	type passedOver struct {
		name string
		err  error
	}
	var skipped []passedOver
	rented, allNoHost := false, true
	for _, entry := range matrix {
		provider, err := cloudprovider.NewGitee(token, entry.zone, entry.image, runID+"-"+strings.ToLower(entry.name))
		if err != nil {
			t.Fatal(err)
		}
		// Rent includes waiting for the instance to reach running with SSH:
		// healthy rentals took up to ~2.5 min on the CANN9 profile, and a 3
		// minute budget cut one off mid-start (2026-10-25). Two profiles at
		// 6 minutes plus install and probe stay inside the test timeout.
		probeCtx, probeCancel := context.WithTimeout(ctx, 6*time.Minute)
		instance, rentErr := provider.Rent(probeCtx, cloudprovider.GPUSpec{GPUModel: entry.model, GPUCount: 1, MinVRAMMB: 32 << 10})
		probeCancel()
		if rentErr != nil {
			if !cloudprovider.IsGiteeNoHost(rentErr) {
				allNoHost = false
			}
			skipped = append(skipped, passedOver{entry.name, rentErr})
			continue
		}
		rented, allNoHost = true, false
		t.Run(entry.name, func(t *testing.T) { runGiteePartitionProfile(t, entry, provider, instance) })
	}
	if !rented && allNoHost {
		reasons := make([]string, 0, len(skipped))
		for _, s := range skipped {
			reasons = append(reasons, s.name+": "+s.err.Error())
		}
		return false, fmt.Errorf("%w (%s)", errGiteeNoHostAnywhere, strings.Join(reasons, "; "))
	}
	for _, s := range skipped {
		t.Run(s.name, func(t *testing.T) {
			if isCloudCapacityError(s.err) {
				t.Skipf("gitee has no capacity for this profile right now, capability profile untested this run: %v", s.err)
			}
			t.Fatal(s.err)
		})
	}
	return rented, nil
}

func runGiteePartitionProfile(t *testing.T, entry giteePartitionMatrixEntry, provider *cloudprovider.Gitee, instance *cloudprovider.Instance) {
	env := setupCloudNodeWithInstance(t, testenv.New(t), provider, cloudprovider.GPUSpec{GPUModel: entry.model, GPUCount: 1, MinVRAMMB: 32 << 10}, instance)
	requireOnlineNode(t, env)
	node, err := env.Control.Store().NodeWithGPUs(env.NodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	archOK := false
	for _, arch := range entry.archs {
		if accelerator.NormalizeArch(node.Arch) == arch {
			archOK = true
			break
		}
	}
	if !archOK || !strings.HasPrefix(node.FrameworkVersion, entry.frameworkVersion) {
		t.Fatalf("observed profile arch=%q framework=%q/%q, want one of %v CANN %s.x", node.Arch, node.Framework, node.FrameworkVersion, entry.archs, entry.frameworkVersion)
	}
	if len(node.GPUs) < 1 {
		t.Fatal("rented Ascend node reported no GPUs")
	}
	capabilities := partitioncap.ForGPU(node, &node.GPUs[0])
	var probeResults []proto.PartitionProbeResult
	if err := json.Unmarshal([]byte(node.PartitionProbes), &probeResults); err != nil {
		t.Fatalf("decode partition probes %q: %v", node.PartitionProbes, err)
	}
	probeStatus, probeDetail := "", ""
	for _, result := range probeResults {
		if result.Backend == accelerator.BackendAscendLogical {
			probeStatus, probeDetail = result.Status, result.Detail
		}
	}
	wantStatus := partitioncap.StatusUnavailable
	if entry.wantReady {
		wantStatus = partitioncap.StatusReady
	}
	if len(capabilities) != 1 || capabilities[0].Backend != accelerator.BackendAscendLogical || capabilities[0].Status != wantStatus || entry.wantReady && probeStatus != proto.PartitionProbePassed || !entry.wantReady && probeStatus != entry.wantProbeStatus || !strings.Contains(probeDetail, entry.wantProbeDetail) || node.RuntimeDriver != entry.wantRuntime || !strings.Contains(node.PartitionAssets, "libvruntime.so") {
		diagnostic, diagnosticErr := runCloudCommand(t.Context(), env.Instance, `set +e
printf 'arch='; uname -m
printf '\nnpu-smi-info\n'; timeout 20s npu-smi info 2>&1
printf '\ndevice-share-card-0\n'; timeout 10s npu-smi info -t device-share -i 0 -c 0 2>&1
printf '\ndevice-share-card-1\n'; timeout 10s npu-smi info -t device-share -i 1 -c 0 2>&1
printf '\nassets\n'; find /opt/gpuworks /tmp/gpuworks -type f -path '*dgpu-runtime*' -printf '%p %s bytes\n' 2>/dev/null | sort
printf 'disk\n'; df -h / /opt/gpuworks 2>/dev/null
printf 'agent-log\n'; tail -n 100 /opt/gpuworks/agent.log /tmp/gpuworks/agent.log 2>&1 | sed -E 's/(node-token[= ]+)[^ ]+/\1[redacted]/g'`)
		archivePartitionEvidence(t, strings.ReplaceAll(t.Name(), "/", "-")+"-failure.txt", diagnostic)
		t.Fatalf("Ascend capability=%+v raw_assets=%q diagnostic_err=%v\n%s", capabilities, node.PartitionAssets, diagnosticErr, diagnostic)
	}
	archivePartitionEvidence(t, strings.ReplaceAll(t.Name(), "/", "-")+".txt", fmt.Sprintf("instance=ledger-owned-and-destroyed\nmodel=%s\narch=%s\nframework=%s %s\nruntime=%s\ngpu=%s %s\nprobe=%s raw=%s\ncapability=%+v\n", entry.model, node.Arch, node.Framework, node.FrameworkVersion, node.RuntimeDriver, node.GPUs[0].Name, node.GPUs[0].ArchID, probeStatus, node.PartitionProbes, capabilities[0]))

	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := env.cleanup(cleanupCtx); err != nil {
		t.Fatalf("destroy Gitee instance %s: %v", env.Instance.InstanceID, err)
	}
	leaked, err := provider.ListLeakedForRunID(cleanupCtx, provider.RunID())
	if err != nil || len(leaked) != 0 {
		t.Fatalf("Gitee cleanup run=%s leaked=%v err=%v", provider.RunID(), leaked, err)
	}
}
