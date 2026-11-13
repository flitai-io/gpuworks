// Package nerdctl implements runtime.Driver over the nerdctl CLI (containerd).
// Nodes without a Docker daemon but with containerd/nerdctl (observed on
// 198.51.100.32) select this driver. Every command is namespace-scoped: nerdctl
// defaults to the "k8s.io" namespace, which is not usable for GPUWorks
// containers, so every invocation is pinned to Namespace explicitly — this is
// the single most important difference from the docker driver and the one
// documented failure mode in real validation
// (see artifacts/archive/2026-08-28-gitee-ascend-verification/device-share-limitation-2026-08-28.md sibling
// research: nerdctl silently placing containers in the wrong namespace).
package nerdctl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// Namespace is the containerd namespace GPUWorks uses for every nerdctl
// invocation. It is deliberately distinct from nerdctl's "k8s.io" default so
// GPUWorks containers never collide with a kubelet-managed workload sharing
// the same host.
const Namespace = "gpuworks"

// Driver runs containers via `nerdctl -n <namespace>`.
type Driver struct {
	ns string
	// authDir is where a pull's private registry login is written; "" is the
	// system temp directory.
	authDir string
}

// New returns a nerdctl driver pinned to the GPUWorks namespace.
func New() *Driver { return &Driver{ns: Namespace} }

// NewInNamespace returns a driver pinned to another containerd namespace.
// The Kubernetes runtime uses it for kubelet's "k8s.io" namespace, where the
// images its pods run from must live; nothing else should need it.
func NewInNamespace(ns string) *Driver { return &Driver{ns: ns} }

// WithAuthDir makes pulls write their private registry login under dir
// instead of the system temp directory. The kube runtime runs nerdctl in the
// node's mount namespace, where only paths shared with the agent (its state
// directory) exist.
func (d *Driver) WithAuthDir(dir string) *Driver {
	d.authDir = dir
	return d
}

// Name implements runtime.Driver.
func (d *Driver) Name() string { return "nerdctl" }

// Detect reports whether nerdctl is usable (binary present + containerd
// reachable in our namespace). Docker is preferred when both are available;
// node.go only tries this driver after docker detection fails.
func (d *Driver) Detect(ctx context.Context) (bool, error) {
	if _, err := exec.LookPath("nerdctl"); err != nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	slog.Debug("runtime command", "runtime", "nerdctl", "args", []string{"-n", d.ns, "ps"})
	if err := exec.CommandContext(ctx, "nerdctl", "-n", d.ns, "ps").Run(); err != nil {
		return false, nil // installed but containerd not reachable / no permission
	}
	return true, nil
}

// ImageExists reports whether the image is present locally.
func (d *Driver) ImageExists(ctx context.Context, image string) (bool, error) {
	slog.Debug("runtime command", "runtime", "nerdctl", "args", []string{"-n", d.ns, "image", "inspect", image})
	out, err := exec.CommandContext(ctx, "nerdctl", "-n", d.ns, "image", "inspect", image).CombinedOutput()
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err == nil {
		return true, nil
	}
	message := strings.ToLower(string(out))
	if strings.Contains(message, "no such image:") || strings.Contains(message, "no such image or container:") {
		return false, nil
	}
	return false, fmt.Errorf("nerdctl image inspect: %w: %s", err, strings.TrimSpace(string(out)))
}

// RemoveImage deletes a local image in the gpuworks namespace. containerd
// refuses while a container still uses it, and that refusal is the answer.
func (d *Driver) RemoveImage(ctx context.Context, image string) error {
	slog.Info("runtime command", "runtime", "nerdctl", "args", []string{"-n", d.ns, "image", "rm", image})
	out, err := exec.CommandContext(ctx, "nerdctl", "-n", d.ns, "image", "rm", image).CombinedOutput()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("nerdctl image rm: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (d *Driver) InspectImage(ctx context.Context, image string) (runtime.ImageInspection, error) {
	inspection, _, err := d.inspectImage(ctx, image)
	return inspection, err
}

// inspectImage keeps the config identity separate from nerdctl's lookup digest.
// No --platform is passed: nerdctl/containerd selects the host's default platform.
// For a multi-platform index, ImageConfigDesc and ImageConfig describe that
// selected platform; Image.Target can still identify the whole index. nerdctl's
// dockercompat mode converts this same selected config, so native mode does not
// change the platform selection. A Run --platform override is not applied here.
func (d *Driver) inspectImage(ctx context.Context, image string) (runtime.ImageInspection, string, error) {
	out, err := d.output(ctx, 10*time.Second, "image", "inspect", "--mode=native", "--format", "{{json .}}", image)
	if err != nil {
		return runtime.ImageInspection{}, "", err
	}
	// nerdctl's `image inspect --format {{json .}}` prints one JSON object per
	// matching image on its own line (not a JSON array like `docker image
	// inspect`); take the first line for the requested reference.
	line := strings.TrimSpace(out)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	var raw struct {
		Image struct {
			Target struct {
				Digest string `json:"digest"`
			} `json:"target"`
		} `json:"Image"`
		ImageConfigDesc struct {
			Digest string `json:"digest"`
		} `json:"ImageConfigDesc"`
		ImageConfig *struct {
			Config struct {
				Entrypoint []string       `json:"Entrypoint"`
				Cmd        []string       `json:"Cmd"`
				Env        []string       `json:"Env"`
				Volumes    map[string]any `json:"Volumes"`
			} `json:"config"`
		} `json:"ImageConfig"`
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return runtime.ImageInspection{}, "", err
	}
	// nerdctl resolves image IDs against the target (manifest/index) digest.
	// Its dockercompat Id is a config digest and cannot be used for that lookup.
	if !imageDigestPattern.MatchString(raw.Image.Target.Digest) || !imageDigestPattern.MatchString(raw.ImageConfigDesc.Digest) || raw.ImageConfig == nil {
		return runtime.ImageInspection{}, "", fmt.Errorf("nerdctl image inspect missing target digest, config digest or image config")
	}
	if imageDigestPattern.MatchString(image) && raw.Image.Target.Digest != image {
		return runtime.ImageInspection{}, "", fmt.Errorf("nerdctl image inspect returned a different target digest")
	}
	config := raw.ImageConfig.Config
	env := map[string]string{}
	for _, kv := range config.Env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	vols := make([]string, 0, len(config.Volumes))
	for v := range config.Volumes {
		vols = append(vols, v)
	}
	return runtime.ImageInspection{ID: raw.ImageConfigDesc.Digest, Entrypoint: config.Entrypoint, Cmd: config.Cmd, Env: env, Volumes: vols}, raw.Image.Target.Digest, nil
}

// PullImage pulls an image, streaming its progress to onLine in the shared pull
// line format (see pullFrames).
func (d *Driver) PullImage(ctx context.Context, image string, onLine func(string)) error {
	if err := runtime.ValidatePullProxy(ctx); err != nil {
		return err
	}
	slog.Info("runtime command", "runtime", "nerdctl", "args", []string{"-n", d.ns, "pull", image})
	// A registry login rides in a private docker config directory for this
	// pull only, never in the arguments or the user's own docker config.
	env, cleanupAuth, err := runtime.PullAuthEnvIn(ctx, image, runtime.PullProxyEnv(ctx, os.Environ()), d.authDir)
	if err != nil {
		return err
	}
	defer cleanupAuth()
	cmd := exec.CommandContext(ctx, "nerdctl", "-n", d.ns, "pull", image)
	cmd.Env = env
	stdout, writer, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdout = writer
	cmd.Stderr = writer // nerdctl writes progress to stderr; capture both like docker
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = writer.Close()
		return err
	}
	// The child owns the write end. Keeping the parent's copy open would make
	// the scanner wait forever after a normal CLI exits.
	_ = writer.Close()
	type scanResult struct {
		tail string
		err  error
	}
	scanDone := make(chan scanResult, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		var outputTail string
		frames := newPullFrames(func(line string) {
			if onLine != nil {
				onLine(line)
			}
		})
		for sc.Scan() {
			// The progress table is redrawn every 100 ms; only the lines
			// outside it can explain a failure.
			if !frames.line(sc.Text()) {
				outputTail = tailOutput(outputTail+"\n"+sc.Text(), 4096)
			}
		}
		scanDone <- scanResult{tail: outputTail, err: sc.Err()}
	}()

	err = cmd.Wait()
	// A helper process may inherit nerdctl's progress descriptor and keep it open
	// after the CLI exits. Let ordinary output drain, but do not wait forever for
	// that unrelated helper; closing our read end releases the scanner.
	var scan scanResult
	select {
	case scan = <-scanDone:
	case <-time.After(250 * time.Millisecond):
		_ = stdout.Close()
		scan = <-scanDone
	}
	_ = stdout.Close()
	if err == nil && scan.err != nil && !errors.Is(scan.err, os.ErrClosed) {
		err = scan.err
	}
	if err != nil {
		slog.Warn("runtime command failed", "runtime", "nerdctl", "args", []string{"-n", d.ns, "pull", image}, "err", err, "stderr_tail", scan.tail)
		if scan.tail != "" {
			return fmt.Errorf("nerdctl pull: %w: %s", err, scan.tail)
		}
	}
	return err
}

// TagImage records a source-selected pull under the canonical catalog ref.
func (d *Driver) TagImage(ctx context.Context, source, canonical string) error {
	slog.Info("runtime command", "runtime", "nerdctl", "args", []string{"-n", d.ns, "tag", source, canonical})
	return exec.CommandContext(ctx, "nerdctl", "-n", d.ns, "tag", source, canonical).Run()
}

// HoldsImage reports whether the gpuworks namespace holds image, for a node
// switching from nerdctl to another runtime.
func (d *Driver) HoldsImage(ctx context.Context, image string) (bool, error) {
	return d.ImageExists(ctx, image)
}

func (d *Driver) SaveImage(ctx context.Context, image string, w io.Writer) error {
	slog.Debug("runtime command", "runtime", "nerdctl", "args", []string{"-n", d.ns, "save", image})
	cmd := exec.CommandContext(ctx, "nerdctl", "-n", d.ns, "save", image)
	cmd.Stdout = w
	cmd.Stderr = io.Discard
	return cmd.Run()
}

func (d *Driver) LoadImage(ctx context.Context, image string, r io.Reader) error {
	_, err := d.LoadImageRef(ctx, image, r)
	return err
}

// LoadImageRef loads a streamed archive and returns the reference nerdctl
// named on stdout ("unpacking <ref> (sha256:…)"), or "" when it named none.
func (d *Driver) LoadImageRef(ctx context.Context, image string, r io.Reader) (string, error) {
	slog.Info("runtime command", "runtime", "nerdctl", "args", []string{"-n", d.ns, "load"}, "image", image)
	cmd := exec.CommandContext(ctx, "nerdctl", "-n", d.ns, "load")
	cmd.Stdin = r
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("nerdctl load: %w: %s", err, msg)
		}
		return "", fmt.Errorf("nerdctl load: %w", err)
	}
	return runtime.ParseLoadedImageRefFor(stdout.String(), image), nil
}

// LoadImageFile imports an archive that is already a file on disk, so a
// downloaded archive is never copied a second time through a pipe.
func (d *Driver) LoadImageFile(ctx context.Context, image, path string) error {
	_, err := d.LoadImageFileRef(ctx, image, path)
	return err
}

// LoadImageFileRef loads the archive and returns the reference nerdctl named
// on stdout ("unpacking <ref> (sha256:…)"), or "" when it named none.
func (d *Driver) LoadImageFileRef(ctx context.Context, image, path string) (string, error) {
	slog.Info("runtime command", "runtime", "nerdctl", "args", []string{"-n", d.ns, "load", "-i", path}, "image", image)
	cmd := exec.CommandContext(ctx, "nerdctl", "-n", d.ns, "load", "-i", path)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("nerdctl load -i: %w: %s", err, msg)
		}
		return "", fmt.Errorf("nerdctl load -i: %w", err)
	}
	return runtime.ParseLoadedImageRefFor(stdout.String(), image), nil
}

var (
	_ runtime.LoadedRefReporter   = (*Driver)(nil)
	_ runtime.StreamedRefReporter = (*Driver)(nil)
	_ runtime.ImageTagger         = (*Driver)(nil)
	_ runtime.LocalImageSource    = (*Driver)(nil)
)

// Prepare is a no-op for nerdctl (image pull handled separately).
func (d *Driver) Prepare(ctx context.Context, spec runtime.WorkloadSpec) error { return nil }

// ListContainers lists container names, including stopped ones. It is
// namespace-scoped like every other nerdctl operation, so the listing never
// mixes GPUWorks containers with kubelet-managed ones.
func (d *Driver) ListContainers(ctx context.Context) ([]string, error) {
	out, err := d.output(ctx, 10*time.Second, "ps", "-a", "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	return splitOutputLines(out), nil
}

// RunningContainers lists the names of the containers nerdctl reports as up
// in the GPUWorks namespace: `nerdctl ps` without -a.
func (d *Driver) RunningContainers(ctx context.Context) ([]string, error) {
	out, err := d.output(ctx, 10*time.Second, "ps", "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	return splitOutputLines(out), nil
}

// Run starts a detached container and returns its id. A run that outlives
// runtime.DefaultRunTimeout fails the launch; its container is removed once the
// CLI returns (see runtime.RunDetached).
func (d *Driver) Run(ctx context.Context, spec runtime.WorkloadSpec) (string, error) {
	_, target, err := d.inspectImage(ctx, spec.Image)
	if err != nil {
		// Observation is optional; a failed read must not prevent nerdctl from
		// following its normal image lookup/pull and launch behavior.
		slog.Warn("cannot record container image identity", "runtime", "nerdctl", "image", spec.Image, "err", err)
	} else {
		spec.DockerArgs = append(slices.Clone(spec.DockerArgs), "--label", imageTargetLabel+"="+target)
	}
	return runtime.RunDetached(ctx, runtime.DetachedRun{
		CLI: "nerdctl", Args: d.buildRunArgs(spec), Name: spec.Name, Remove: d.Remove,
		Timeout: runTimeout, AbandonGrace: abandonGrace,
	})
}

// Overridden by tests; zero selects the runtime package defaults.
var runTimeout, abandonGrace time.Duration

// buildRunArgs mirrors the docker driver's flag shape (nerdctl's CLI is
// designed to be docker-compatible for these common flags), with the
// namespace flag pinned first. One deliberate omission from the docker
// driver: no --restart flag. nerdctl (unlike dockerd) has no persistent
// restart-policy daemon behind it, and passing --restart=no is accepted but
// meaningless; leaving it off avoids implying a guarantee this runtime does
// not provide.
//
// Device injection through vendor runtime plugins (for example
// Ascend-Docker-Runtime) is docker-specific. GPUWorks' driver-aware Ascend
// injector instead supplies nerdctl with raw --device arguments and explicit
// driver/DCMI bind mounts; buildRunArgs propagates both here. See
// gpu.AscendProber.RuntimeInjectionForDriver and docs/dev/ledgers/engineering-log.md "Ascend device
// injection for the nerdctl runtime driver".
func (d *Driver) buildRunArgs(spec runtime.WorkloadSpec) []string {
	args := []string{"-n", d.ns, "run", "-d", "--name", spec.Name}

	if spec.HostNetwork {
		args = append(args, "--network", "host")
	} else {
		for _, p := range spec.Ports {
			args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", p, p))
		}
	}

	args = append(args, expandDeviceDirs(spec.DockerArgs)...)

	if spec.ShmBytes > 0 {
		args = append(args, "--shm-size", strconv.FormatInt(spec.ShmBytes, 10))
	}
	for k, v := range spec.Env {
		args = append(args, "-e", k+"="+v)
	}
	for _, m := range spec.Mounts {
		mnt := m.Source + ":" + m.Target
		if m.ReadOnly {
			mnt += ":ro"
		}
		args = append(args, "-v", mnt)
	}
	if len(spec.Entrypoint) > 0 {
		args = append(args, "--entrypoint", spec.Entrypoint[0])
	}
	args = append(args, spec.Image)
	if len(spec.Entrypoint) > 1 {
		args = append(args, spec.Entrypoint[1:]...)
	}
	args = append(args, spec.Cmd...)
	return args
}

// Stop stops a container (10s graceful window).
func (d *Driver) Stop(ctx context.Context, id string) error {
	_, err := d.outputMutating(ctx, 30*time.Second, "stop", "-t", "10", id)
	return err
}

// Remove force-deletes a container.
func (d *Driver) Remove(ctx context.Context, id string) error {
	out, err := d.outputMutating(ctx, 30*time.Second, "rm", "-f", id)
	if err != nil && (strings.Contains(out, "no such container") || strings.Contains(out, "not found") ||
		strings.Contains(err.Error(), "no such container") || strings.Contains(err.Error(), "not found")) {
		return nil
	}
	return err
}

// Status inspects a container. Unlike docker's inspect output, nerdctl's
// .State has no OOMKilled or Health fields at all (confirmed against real
// nerdctl 1.7.6 on 198.51.100.32: `{"Status":"running","Running":true,
// "Paused":false,"Restarting":false,"Pid":...,"ExitCode":0,"Error":"",
// "FinishedAt":...}`) — requesting them errors the template ("map has no
// entry for key ...") instead of leaving them empty like docker does. Both
// fields are therefore always reported as zero values here rather than
// queried.
func (d *Driver) Status(ctx context.Context, id string) (runtime.Status, error) {
	format := "{{.State.Running}}|{{.State.ExitCode}}"
	out, err := d.output(ctx, 10*time.Second, "inspect", "--format", format, id)
	if err != nil {
		// Only the CLI's own answer proves absence: a nerdctl binary that
		// could not be executed fails with "executable file not found" too.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && (strings.Contains(out, "no such object") || strings.Contains(out, "not found")) {
			return runtime.Status{Exists: false}, nil
		}
		return runtime.Status{}, err
	}
	// Output that does not parse is an error, not a stopped container: the
	// liveness monitor retires an instance on "exists but not running", so a
	// guessed value would fail a healthy one. Combined output may carry CLI
	// warnings ahead of the template result, which is the last line.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	line := strings.TrimSpace(lines[len(lines)-1])
	parts := strings.Split(line, "|")
	if len(parts) != 2 || (parts[0] != "true" && parts[0] != "false") {
		return runtime.Status{}, fmt.Errorf("unexpected nerdctl inspect output %q", line)
	}
	exitCode, err := strconv.Atoi(parts[1])
	if err != nil {
		return runtime.Status{}, fmt.Errorf("unexpected nerdctl inspect exit code %q", parts[1])
	}
	return runtime.Status{Exists: true, Running: parts[0] == "true", ExitCode: exitCode}, nil
}

// Logs returns the container's log stream.
func (d *Driver) Logs(ctx context.Context, id string, follow bool, tail int) (io.ReadCloser, error) {
	args := []string{"-n", d.ns, "logs"}
	if follow {
		args = append(args, "-f")
	}
	if tail > 0 {
		args = append(args, "--tail", strconv.Itoa(tail))
	}
	args = append(args, id)
	cmd := exec.CommandContext(ctx, "nerdctl", args...)
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		err := cmd.Wait()
		_ = pw.CloseWithError(err)
	}()
	return &procReadCloser{pr: pr, cmd: cmd}, nil
}

// Exec runs a command in the container.
func (d *Driver) Exec(ctx context.Context, id string, cmd []string) ([]byte, error) {
	args := append([]string{"-n", d.ns, "exec", id}, cmd...)
	slog.Info("runtime command", "runtime", "nerdctl", "args", args)
	c := exec.CommandContext(ctx, "nerdctl", args...)
	out, err := c.CombinedOutput()
	if err != nil {
		slog.Warn("runtime command failed", "runtime", "nerdctl", "args", args, "err", err, "stderr_tail", tailOutput(string(out), 400))
	}
	return out, err
}

// ExecPTY returns a nerdctl exec command attached to the existing container.
// The namespace stays explicit here just as it does for every other nerdctl
// operation; otherwise terminal sessions would look in nerdctl's default
// namespace and fail to find GPUWorks containers.
func (d *Driver) ExecPTY(_ context.Context, id string, cmd []string) ([]string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("nerdctl exec requires a container id")
	}
	if len(cmd) == 0 {
		cmd = []string{"/bin/sh", "-l"}
	}
	args := []string{"nerdctl", "-n", d.ns, "exec", "-i", "-t", id}
	return append(args, cmd...), nil
}

func tailOutput(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// splitOutputLines turns one-name-per-line CLI output into a slice, dropping
// the blank line a trailing newline would otherwise produce.
func splitOutputLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// output runs nerdctl (namespace-scoped) with a timeout and returns combined
// output.
func (d *Driver) output(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	full := append([]string{"-n", d.ns}, args...)
	slog.Debug("runtime command", "runtime", "nerdctl", "args", full)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nerdctl", full...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 400 {
			msg = msg[len(msg)-400:]
		}
		slog.Warn("runtime command failed", "runtime", "nerdctl", "args", full, "err", err, "stderr_tail", msg)
	}
	return string(out), err
}

// outputMutating runs a state-changing nerdctl command. Mutating commands are
// the audit trail for "who touched this node", so they log at INFO even on
// success; pure reads through output stay at Debug.
func (d *Driver) outputMutating(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	full := append([]string{"-n", d.ns}, args...)
	slog.Info("runtime command", "runtime", "nerdctl", "args", full)
	return d.output(ctx, timeout, args...)
}

// procReadCloser wraps the log pipe so Close also kills the nerdctl logs
// process.
type procReadCloser struct {
	pr  *io.PipeReader
	cmd *exec.Cmd
}

func (p *procReadCloser) Read(b []byte) (int, error) { return p.pr.Read(b) }
func (p *procReadCloser) Close() error {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	return p.pr.Close()
}

var _ runtime.Driver = (*Driver)(nil)
var _ runtime.PTYExecer = (*Driver)(nil)
var _ runtime.ImageInspector = (*Driver)(nil)
var _ runtime.ImageArchiver = (*Driver)(nil)
