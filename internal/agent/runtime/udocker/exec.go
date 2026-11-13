package udocker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// Exec runs a command in the container (fresh proot invocation). Under
// runveil cmd is the complete argv, as with `docker exec`; the udocker CLI
// path appends it to the image entrypoint.
func (d *Driver) Exec(ctx context.Context, id string, cmd []string) ([]byte, error) {
	if d.runveil {
		if len(cmd) == 0 {
			return nil, fmt.Errorf("exec requires a command")
		}
		spec, ok, err := d.loadSpec(id)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("runveil exec unavailable: workload spec for %q is missing", id)
		}
		command, groups, err := d.runveilCommand(spec, cmd, nil, "")
		if err != nil {
			return nil, err
		}
		return d.runveilExec(ctx, command, groups).CombinedOutput()
	}
	args := append([]string{"run", id}, cmd...)
	out, err := d.udo(ctx, args...)
	return []byte(out), err
}

// ExecPTY opens a controlled interactive shell rooted at the tracked workload
// rootfs. udocker 1.3.x has no `exec` command, so this is necessarily a new
// proot process; it is deliberately not described as attaching to the model
// process. The workload's environment, mounts, GPU selection and / workdir
// are reconstructed from the persisted run spec.
func (d *Driver) ExecPTY(ctx context.Context, id string, cmd []string) ([]string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("container terminal requires a container id")
	}
	if err := d.ensureProvisioned(ctx); err != nil {
		return nil, fmt.Errorf("prepare container terminal: %w", err)
	}
	spec, ok, err := d.loadSpec(id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("udocker terminal unavailable: workload spec for %q is missing", id)
	}
	if d.runveil {
		return d.runveilPTYArgv(spec, cmd)
	}
	args := []string{"run"}
	for k, v := range d.runEnv(spec) {
		args = append(args, "--env="+k+"="+v)
	}
	for _, m := range d.workloadMounts(spec) {
		args = append(args, "--volume="+m.Source+":"+m.Target)
	}
	// P1 workloads use / as their working directory. Keep this explicit so a
	// shell opened from a different host cwd still sees the model's workdir.
	args = append(args, "--workdir=/")
	if len(cmd) == 0 {
		args = append(args, "--entrypoint=/bin/sh", id, "-l")
	} else {
		args = append(args, "--entrypoint="+cmd[0], id)
		args = append(args, cmd[1:]...)
	}
	return d.udockerInvocation(args), nil
}

func (d *Driver) rememberSpec(id string, spec runtime.WorkloadSpec) error {
	clone := cloneWorkloadSpec(spec)
	d.specMu.Lock()
	if d.specs == nil {
		d.specs = make(map[string]runtime.WorkloadSpec)
	}
	d.specs[id] = clone
	d.specMu.Unlock()
	data, err := json.Marshal(clone)
	if err != nil {
		return err
	}
	return os.WriteFile(d.SpecPath(id), data, 0o600)
}

func (d *Driver) loadSpec(id string) (runtime.WorkloadSpec, bool, error) {
	d.specMu.Lock()
	spec, ok := d.specs[id]
	d.specMu.Unlock()
	if ok {
		return cloneWorkloadSpec(spec), true, nil
	}
	data, err := os.ReadFile(d.SpecPath(id))
	if os.IsNotExist(err) {
		return runtime.WorkloadSpec{}, false, nil
	}
	if err != nil {
		return runtime.WorkloadSpec{}, false, fmt.Errorf("read udocker workload spec: %w", err)
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		return runtime.WorkloadSpec{}, false, fmt.Errorf("decode udocker workload spec: %w", err)
	}
	return spec, true, nil
}

func cloneWorkloadSpec(spec runtime.WorkloadSpec) runtime.WorkloadSpec {
	clone := spec
	clone.Cmd = append([]string(nil), spec.Cmd...)
	clone.Entrypoint = append([]string(nil), spec.Entrypoint...)
	clone.GPUIndexes = append([]int(nil), spec.GPUIndexes...)
	clone.DockerArgs = append([]string(nil), spec.DockerArgs...)
	clone.PreparationNotes = append([]string(nil), spec.PreparationNotes...)
	clone.Env = make(map[string]string, len(spec.Env))
	for k, v := range spec.Env {
		clone.Env[k] = v
	}
	clone.Mounts = append([]runtime.Mount(nil), spec.Mounts...)
	clone.Ports = append([]int(nil), spec.Ports...)
	return clone
}
