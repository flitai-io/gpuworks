// Package hostproc implements runtime.Driver for nodes whose accelerator a
// container cannot reach: on macOS the Metal GPU exists only for host
// processes, since every Linux container runs inside a VM without it. A
// workload's image is an environment package - an OCI image whose files form
// a relocatable root (a Python, an engine and its pinned libraries, built for
// darwin/arm64) - pulled and unpacked once per manifest, and the workload runs
// as an ordinary process group of the agent's user with that root's paths
// translated (launch.go). Pulling, storing and unpacking reuse the image store
// the runveil driver keeps in Go (udocker/native): an environment package is
// an ordinary OCI image, so there is one store and one unpacker, not two.
// docs/adr/0044-mac-engines-as-host-processes.md says why engines are not run
// in containers on these nodes.
package hostproc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/procgroup"
	oci "gpuworks.ai/gpuworks/internal/agent/runtime/udocker/native"
	"gpuworks.ai/gpuworks/internal/proto"
)

// DriverName is the runtime name a node reports for this driver.
const DriverName = proto.RuntimeDriverHost

// SupportedOn reports whether the driver runs on goos: environment packages
// are built for macOS only.
func SupportedOn(goos string) bool {
	info, _ := runtime.DriverNamed(DriverName)
	return info.UnavailableOn(goos) == ""
}

// stopGrace is how long a stopped engine gets to exit after SIGTERM before its
// group is killed. Engines release the GPU's memory on a clean exit, which a
// kill leaves to the OS.
const stopGrace = 10 * time.Second

// Driver runs workloads as host processes.
type Driver struct {
	oci          *oci.Engine // pulls and unpacks environment packages under EnvsDir
	stateDir     string      // pid, exit, log and spec files of each workload
	instancesDir string      // per-instance HOME and TMPDIR, removed with the instance
	cacheDir     string      // per-environment caches shared by its instances
	tmpRoot      string      // where the short per-instance TMPDIRs live (tmp.go)

	goos       string
	getenv     func(string) string
	caffeinate string // keeps the Mac awake while a workload runs; "" disables

	envMu sync.Mutex // serializes unpacking environment packages
	specs sync.Mutex // serializes spec file writes
	procs procgroup.Tracker
}

// EnvsDir is where the driver keeps environment packages under baseDir: the
// filesystem whose free space decides whether another package fits.
func EnvsDir(baseDir string) string { return filepath.Join(baseDir, "host-envs") }

// New builds a driver whose state lives under baseDir.
func New(baseDir string) *Driver {
	engine := oci.NewEngine(EnvsDir(baseDir), nil)
	engine.Client = oci.NewClientForPlatform(goruntime.GOOS, goruntime.GOARCH)
	d := &Driver{
		oci:          engine,
		stateDir:     filepath.Join(baseDir, "host-state"),
		instancesDir: filepath.Join(baseDir, "host-instances"),
		cacheDir:     filepath.Join(baseDir, "host-cache"),
		tmpRoot:      "/tmp",
		goos:         goruntime.GOOS,
		getenv:       os.Getenv,
	}
	if path, err := exec.LookPath("caffeinate"); err == nil {
		d.caffeinate = path
	}
	d.procs.Dir = d.stateDir
	return d
}

func (d *Driver) Name() string { return DriverName }

// Detect reports macOS. Linux nodes run containers, whose drivers detect
// first; this one is for the hosts where a container would lose the GPU.
func (d *Driver) Detect(context.Context) (bool, error) { return SupportedOn(d.goos), nil }

func (d *Driver) ImageExists(_ context.Context, image string) (bool, error) {
	return d.oci.HasImage(image), nil
}

// PullImage downloads an environment package built for this host's OS and
// architecture; an image with no such manifest fails here rather than at run.
func (d *Driver) PullImage(ctx context.Context, image string, onLine func(string)) error {
	return d.oci.Pull(ctx, image, onLine)
}

// LoadImage registers an environment package from an image archive (docker
// save or OCI layout), for nodes that receive packages from the server or an
// offline bundle instead of a registry.
func (d *Driver) LoadImage(ctx context.Context, image string, r io.Reader) error {
	return d.oci.Load(ctx, image, r)
}

// SaveImage writes a pulled package as an image archive another Mac loads,
// which makes this node a source for node-to-node image sync.
func (d *Driver) SaveImage(ctx context.Context, image string, w io.Writer) error {
	if !d.oci.HasImage(image) {
		return fmt.Errorf("environment package %s is not on this node", image)
	}
	return d.oci.Layout.SaveArchive(ctx, oci.ParseRef(image), w)
}

// LoadImageFile is LoadImage for an archive already on the node's disk.
func (d *Driver) LoadImageFile(ctx context.Context, image, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return d.oci.Load(ctx, image, f)
}

// TagImage registers a pulled package under a second reference: a package
// pulled from a mirror is recorded under the canonical reference deployments
// name. Both share one manifest, so they unpack to one environment.
func (d *Driver) TagImage(_ context.Context, source, target string) error {
	return d.oci.Layout.TagImage(oci.ParseRef(source), oci.ParseRef(target))
}

// InspectImage reports a pulled package's identity (its manifest digest) and
// the launch defaults its config declares.
func (d *Driver) InspectImage(_ context.Context, image string) (runtime.ImageInspection, error) {
	ref := oci.ParseRef(image)
	id, err := d.oci.Layout.ImageID(ref)
	if err != nil || id == "" {
		return runtime.ImageInspection{}, fmt.Errorf("environment package %s is not pulled on this node", image)
	}
	config, err := d.oci.Layout.LoadImageConfig(ref)
	if err != nil {
		return runtime.ImageInspection{}, err
	}
	env := make(map[string]string, len(config.Env))
	for _, entry := range config.Env {
		if key, value, ok := strings.Cut(entry, "="); ok {
			env[key] = value
		}
	}
	return runtime.ImageInspection{ID: id, Entrypoint: config.Entrypoint, Cmd: config.Cmd, Env: env, Volumes: config.Volumes}, nil
}

// Prepare unpacks the workload's environment package.
func (d *Driver) Prepare(ctx context.Context, spec runtime.WorkloadSpec) error {
	_, err := d.ensureEnv(ctx, spec.Image, nil)
	return err
}

// ListContainers lists every workload this driver started and has not removed,
// running or not: the ones with a recorded spec.
func (d *Driver) ListContainers(context.Context) ([]string, error) {
	entries, err := os.ReadDir(d.stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list host workloads: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if name, ok := strings.CutSuffix(entry.Name(), ".spec.json"); ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func (d *Driver) RunningContainers(context.Context) ([]string, error) { return d.procs.Running() }

func (d *Driver) Run(ctx context.Context, spec runtime.WorkloadSpec) (string, error) {
	return d.RunWithProgress(ctx, spec, nil)
}

// RunWithProgress unpacks the environment on first use, then starts the
// workload as the leader of its own process group, so it survives an agent
// restart and Stop can end everything it spawned.
func (d *Driver) RunWithProgress(ctx context.Context, spec runtime.WorkloadSpec, onProgress func(phase, message string)) (string, error) {
	report := func(phase, message string) {
		if onProgress != nil {
			onProgress(phase, message)
		}
	}
	name := spec.Name
	if name == "" || filepath.Base(name) != name {
		return "", fmt.Errorf("invalid workload name %q", name)
	}
	report(proto.PhaseContainerPrepare, "preparing the engine environment")
	// From unpacking to the recorded start, RemoveImage must not delete the
	// environment: until the pid is on record it cannot see the workload.
	d.envMu.Lock()
	defer d.envMu.Unlock()
	root, err := d.ensureEnvLocked(ctx, spec.Image, func(line string) { report(proto.PhaseContainerUnpack, line) })
	if err != nil {
		return "", err
	}
	plan, dirs, err := d.plan(root, spec)
	if err != nil {
		return "", err
	}
	for _, dir := range []string{d.stateDir, dirs.Home, dirs.Cache} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if _, err := d.prepareInstanceTmp(name); err != nil {
		return "", err
	}
	report(proto.PhaseContainerLaunch, "starting the engine process")
	// The spec is on record before the process starts: RemoveImage and Exec
	// read it for any workload they can see.
	if err := d.rememberSpec(name, spec); err != nil {
		return "", fmt.Errorf("record host workload %s: %w", name, err)
	}
	logf, err := os.Create(d.logPath(name))
	if err != nil {
		return "", err
	}
	cmd := exec.Command(plan.Path, plan.Args[1:]...)
	cmd.Args[0] = plan.Args[0]
	cmd.Env = plan.Env
	cmd.Dir = plan.Dir
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	slog.Info("runtime command", "runtime", DriverName, "path", plan.Path, "args", plan.Args)
	if err := cmd.Start(); err != nil {
		logf.Close()
		return "", fmt.Errorf("start %s: %w", plan.Path, err)
	}
	if err := d.procs.Track(name, cmd, func() { logf.Close() }); err != nil {
		// Without its pid on record the engine would run unseen, holding
		// memory no one can stop; the reaper Track started collects it.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return "", err
	}
	d.keepAwake(cmd.Process.Pid)
	return name, nil
}

// keepAwake asks macOS not to idle-sleep while pid runs. The assertion ends
// with the process, whoever reaps it, so a restarted agent leaves it in place.
func (d *Driver) keepAwake(pid int) {
	if d.caffeinate == "" {
		return
	}
	cmd := exec.Command(d.caffeinate, "-i", "-w", strconv.Itoa(pid))
	if err := cmd.Start(); err != nil {
		slog.Warn("cannot keep the host awake for a running engine", "err", err)
		return
	}
	go func() { _ = cmd.Wait() }()
}

func (d *Driver) plan(root string, spec runtime.WorkloadSpec) (launch, instanceDirs, error) {
	config, err := d.oci.Layout.LoadImageConfig(oci.ParseRef(spec.Image))
	if err != nil {
		return launch{}, instanceDirs{}, fmt.Errorf("read environment package config of %s: %w", spec.Image, err)
	}
	dirs := instanceDirs{
		Home:  filepath.Join(d.instancesDir, spec.Name, "home"),
		Tmp:   d.instanceTmp(spec.Name),
		Cache: filepath.Join(d.cacheDir, filepath.Base(filepath.Dir(root))),
	}
	plan, err := planLaunch(root, config, spec, dirs, d.getenv, pathExists)
	return plan, dirs, err
}

func (d *Driver) Stop(ctx context.Context, id string) error {
	if pid, ok := d.procs.PID(id); ok {
		slog.Info("stopping host workload process group", "container", id, "pid", pid)
	}
	return d.procs.Stop(ctx, id, stopGrace)
}

// Remove stops the workload and deletes everything it had of its own. The
// environment package stays: other instances share it, and RemoveImage
// deletes it.
func (d *Driver) Remove(ctx context.Context, id string) error {
	if filepath.Base(id) != id || id == "" {
		return fmt.Errorf("invalid workload name %q", id)
	}
	if err := d.Stop(ctx, id); err != nil {
		return err
	}
	d.procs.Forget(id)
	_ = os.Remove(d.logPath(id))
	_ = os.Remove(d.specPath(id))
	for _, dir := range []string{filepath.Join(d.instancesDir, id), d.instanceTmp(id)} {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove host workload %s: %w", id, err)
		}
	}
	return nil
}

func (d *Driver) Status(ctx context.Context, id string) (runtime.Status, error) {
	exists, running, code := d.procs.Status(ctx, id)
	return runtime.Status{Exists: exists, Running: running, ExitCode: code}, nil
}

func (d *Driver) Logs(ctx context.Context, id string, follow bool, tail int) (io.ReadCloser, error) {
	return procgroup.Logs(ctx, d.logPath(id), follow, tail)
}

// Exec runs cmd, a complete argv as with `docker exec`, in the workload's
// environment: its PATH, variables, translated paths and working directory.
func (d *Driver) Exec(ctx context.Context, id string, cmd []string) ([]byte, error) {
	plan, err := d.execPlan(ctx, id, cmd)
	if err != nil {
		return nil, err
	}
	c := exec.CommandContext(ctx, plan.Path, plan.Args[1:]...)
	c.Args[0] = plan.Args[0]
	c.Env = plan.Env
	c.Dir = plan.Dir
	return c.CombinedOutput()
}

// ExecPTY returns an argv that opens cmd (a login shell by default) in the
// workload's environment. The PTY handler starts it with the agent's own
// environment and working directory, so env -i and a cd carry the workload's.
func (d *Driver) ExecPTY(ctx context.Context, id string, cmd []string) ([]string, error) {
	if len(cmd) == 0 {
		shell := d.getenv("SHELL")
		if shell == "" {
			shell = "/bin/zsh"
		}
		// Interactive, not login: a login shell on macOS runs path_helper from
		// /etc/zprofile, which puts the system's directories ahead of the
		// package's and makes python3 the system one.
		cmd = []string{shell, "-i"}
	}
	plan, err := d.execPlan(ctx, id, cmd)
	if err != nil {
		return nil, err
	}
	argv := append([]string{"/usr/bin/env", "-i"}, plan.Env...)
	argv = append(argv, "/bin/sh", "-c", `cd "$1" && shift && exec "$@"`, "sh", plan.Dir, plan.Path)
	return append(argv, plan.Args[1:]...), nil
}

func (d *Driver) execPlan(ctx context.Context, id string, cmd []string) (launch, error) {
	if len(cmd) == 0 {
		return launch{}, fmt.Errorf("exec requires a command")
	}
	spec, ok, err := d.loadSpec(id)
	if err != nil {
		return launch{}, err
	}
	if !ok {
		return launch{}, fmt.Errorf("host workload %q is not known on this node", id)
	}
	root, err := d.ensureEnv(ctx, spec.Image, nil)
	if err != nil {
		return launch{}, err
	}
	spec.Entrypoint, spec.Cmd = cmd, nil
	plan, _, err := d.plan(root, spec)
	return plan, err
}

// RemoveImage deletes an environment package and, once no other tag names
// the same manifest, its unpacked root and caches. A package a workload still runs from
// is refused: that refusal is the answer the operator needs.
func (d *Driver) RemoveImage(ctx context.Context, image string) error {
	// Held from the in-use check to the deletion, so no start slips between.
	d.envMu.Lock()
	defer d.envMu.Unlock()
	ref := oci.ParseRef(image)
	manifestID, _ := d.oci.Layout.ImageID(ref)
	running, err := d.procs.Running()
	if err != nil {
		return err
	}
	for _, name := range running {
		if spec, ok, _ := d.loadSpec(name); ok && spec.Image != "" {
			if id, _ := d.oci.Layout.ImageID(oci.ParseRef(spec.Image)); id != "" && id == manifestID {
				return fmt.Errorf("environment package %s is in use by %s", image, name)
			}
		}
	}
	if err := d.oci.Layout.RemoveImage(ref); err != nil {
		return err
	}
	if manifestID == "" {
		return nil
	}
	refs, err := d.oci.Layout.Images()
	if err != nil {
		return err
	}
	for _, other := range refs {
		if id, _ := d.oci.Layout.ImageID(other); id == manifestID {
			return nil
		}
	}
	if err := os.RemoveAll(d.oci.Layout.ContainerDir(envName(manifestID))); err != nil {
		return err
	}
	// Its caches (compiled kernels and the like) are only valid for it.
	return os.RemoveAll(filepath.Join(d.cacheDir, envName(manifestID)))
}

func (d *Driver) logPath(name string) string  { return filepath.Join(d.stateDir, name+".log") }
func (d *Driver) specPath(name string) string { return filepath.Join(d.stateDir, name+".spec.json") }

func (d *Driver) rememberSpec(name string, spec runtime.WorkloadSpec) error {
	data, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	d.specs.Lock()
	defer d.specs.Unlock()
	return os.WriteFile(d.specPath(name), data, 0o600)
}

func (d *Driver) loadSpec(name string) (runtime.WorkloadSpec, bool, error) {
	data, err := os.ReadFile(d.specPath(name))
	if os.IsNotExist(err) {
		return runtime.WorkloadSpec{}, false, nil
	}
	if err != nil {
		return runtime.WorkloadSpec{}, false, err
	}
	var spec runtime.WorkloadSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return runtime.WorkloadSpec{}, false, fmt.Errorf("decode host workload spec %s: %w", name, err)
	}
	return spec, true, nil
}

var (
	_ runtime.Driver            = (*Driver)(nil)
	_ runtime.RunProgressDriver = (*Driver)(nil)
	_ runtime.PTYExecer         = (*Driver)(nil)
	_ runtime.ImageRemover      = (*Driver)(nil)
	_ runtime.ImageArchiver     = (*Driver)(nil)
	_ runtime.ImageFileLoader   = (*Driver)(nil)
	_ runtime.ImageTagger       = (*Driver)(nil)
	_ runtime.ImageInspector    = (*Driver)(nil)
)
