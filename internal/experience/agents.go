package experience

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sync"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/simulate"
	"gpuworks.ai/gpuworks/internal/server/agentbin"
)

// member is a simulated node as enrolled in the environment's console.
type member struct {
	UUID  string `json:"uuid"`
	Token string `json:"token"`
}

// extractAgent writes the release's own agent for this host out of the
// server binary, where the simulated nodes run it from.
func extractAgent(dataDir string, agents agentbin.Files) (string, error) {
	name := "gpuworks-agent-" + goruntime.GOOS + "-" + goruntime.GOARCH
	raw, err := agents.Read("", name)
	if err != nil {
		return "", fmt.Errorf("this build carries no agent for %s/%s: %w", goruntime.GOOS, goruntime.GOARCH, err)
	}
	dir := filepath.Join(dataDir, "bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "gpuworks-agent")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o700); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// placeholderPartitionAssets gives the simulated PPU and Ascend nodes the
// dGPU runtime libraries the agent looks for before it offers dGPU; the
// simulated runtime never loads them. NVIDIA's comes from the agent itself
// (gpu.PartitionSimulator).
func placeholderPartitionAssets(dataDir string) (string, error) {
	root := filepath.Join(dataDir, "partition-assets")
	libraries := map[string][]string{
		"ascend": {"libvruntime.so"},
		"ppu":    {"hggc_quota.so", "hgml_dlsym_hook.so"},
	}
	for vendor, files := range libraries {
		dir := filepath.Join(root, vendor, "linux-"+goruntime.GOARCH)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
		for _, name := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("simulated "+name+"\n"), 0o600); err != nil {
				return "", err
			}
		}
	}
	return root, nil
}

// fleet runs one agent process per simulated node, restarting one that
// exits, until ctx ends; wait returns once every one has exited.
type fleet struct {
	agent, assets, server string
	engines, enginesToken string
	dataDir               string
	log                   *slog.Logger
	running               sync.WaitGroup
}

func (f *fleet) wait() { f.running.Wait() }

func (f *fleet) start(ctx context.Context, slot int, n node, m member) error {
	state := filepath.Join(f.dataDir, "nodes", n.Hostname)
	if err := os.MkdirAll(state, 0o700); err != nil {
		return err
	}
	spec := n.Spec
	spec.Engines, spec.EnginesToken = f.engines, f.enginesToken
	spec.PortSlot, spec.PortSlots = slot, len(scenario)
	spec.ParentPID = os.Getpid()
	raw, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	specPath := filepath.Join(state, "simulated-node.json")
	if err := os.WriteFile(specPath, raw, 0o600); err != nil {
		return err
	}
	f.running.Add(1)
	go func() {
		defer f.running.Done()
		f.keep(ctx, n.Hostname, specPath, state, m)
	}()
	return nil
}

func (f *fleet) keep(ctx context.Context, name, specPath, state string, m member) {
	log := f.log.With("node", name)
	for ctx.Err() == nil {
		// The token goes in the environment, which other users of the host
		// cannot read, not on the command line, which they can.
		cmd := exec.CommandContext(ctx, f.agent, "run", "--server", f.server, "--state", state)
		cmd.Env = append(os.Environ(),
			"GPUWORKS_TOKEN="+m.Token,
			simulate.EnvSpec+"="+specPath,
			"GPUWORKS_PARTITION_ASSETS="+f.assets,
			"GPUWORKS_STATE_DIR="+state)
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = 5 * time.Second
		err := cmd.Run()
		if ctx.Err() != nil {
			return
		}
		log.Warn("simulated node exited; restarting", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}
