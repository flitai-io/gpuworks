// Package docker implements runtime.Driver over the docker CLI. Shelling out to
// the CLI (rather than the SDK) keeps the agent binary light and CGO-free; the
// CLI is present whenever the docker driver is selected for a node.
package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	hostruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// Driver runs containers via `docker`.
type Driver struct {
	runMu       sync.Mutex
	pendingRuns map[string]*pendingRun
}

// New returns a docker driver.
func New() *Driver { return &Driver{} }

// Name implements runtime.Driver.
func (d *Driver) Name() string { return "docker" }

// Detect reports whether Docker is usable for workloads on this host. A
// reachable remote daemon is deliberately rejected because bind mounts and
// accelerator devices would resolve on the remote machine, not this node.
func (d *Driver) Detect(ctx context.Context) (bool, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		slog.Debug("docker runtime unavailable", "reason", "docker binary not found")
		return false, nil
	}
	if host, ok := localDockerHost(); !ok {
		slog.Debug("docker runtime unavailable", "reason", "local docker socket unavailable or endpoint is remote", "host", host)
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	slog.Debug("runtime command", "runtime", "docker", "args", []string{"info", "--format", "{{.ServerVersion}}"})
	var stderr strings.Builder
	cmd := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}")
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		slog.Debug("docker runtime unavailable", "reason", "docker info failed", "err", err, "stderr", strings.TrimSpace(stderr.String()))
		if daemonNotReady(ctx.Err(), stderr.String()) {
			return false, fmt.Errorf("%w: %s", runtime.ErrDaemonNotReady, firstLine(stderr.String(), err))
		}
		return false, nil // installed, but this agent may not use it (permission denied)
	}
	return true, nil
}

// daemonNotReady reports whether a failed `docker info` is a daemon that has
// not come up or is too busy to answer, rather than one this agent may not
// use. dockerd starting at boot leaves the socket refusing connections;
// dockerd pulling a large image answers slower than the timeout.
func daemonNotReady(ctxErr error, stderr string) bool {
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		return true
	}
	text := strings.ToLower(stderr)
	if strings.Contains(text, "permission denied") {
		return false
	}
	return strings.Contains(text, "cannot connect to the docker daemon") || strings.Contains(text, "connection refused") || strings.Contains(text, "is the docker daemon running")
}

func firstLine(stderr string, err error) string {
	if line, _, _ := strings.Cut(strings.TrimSpace(stderr), "\n"); line != "" {
		return line
	}
	return err.Error()
}

// localDockerHost resolves the endpoint Docker can use without shelling out.
// GPUWorks workloads depend on local bind mounts, so tcp/ssh endpoints and
// non-default contexts are not valid node runtimes.
func localDockerHost() (string, bool) {
	if contextName := strings.TrimSpace(os.Getenv("DOCKER_CONTEXT")); contextName != "" && contextName != "default" {
		return "context://" + contextName, false
	}
	host := strings.TrimSpace(os.Getenv("DOCKER_HOST"))
	if host != "" {
		if !strings.HasPrefix(host, "unix://") {
			return host, false
		}
		host = strings.TrimPrefix(host, "unix://")
		return host, isUnixSocket(host)
	}
	for _, candidate := range []string{"/var/run/docker.sock", filepath.Join(strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")), "docker.sock")} {
		if strings.TrimSpace(candidate) == "docker.sock" {
			continue
		}
		if isUnixSocket(candidate) {
			return candidate, true
		}
	}
	return "/var/run/docker.sock", false
}

func isUnixSocket(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode()&os.ModeSocket != 0
}

// ImageExists reports whether the image is present locally.
func (d *Driver) ImageExists(ctx context.Context, image string) (bool, error) {
	slog.Debug("runtime command", "runtime", "docker", "args", []string{"image", "inspect", image})
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", image).CombinedOutput()
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err == nil {
		return true, nil
	}
	if strings.Contains(strings.ToLower(string(out)), "no such image:") {
		return false, nil
	}
	return false, fmt.Errorf("docker image inspect: %w: %s", err, strings.TrimSpace(string(out)))
}

// RemoveImage deletes a local image. Docker refuses while a container still
// uses it, and that refusal is what the operator needs to see.
func (d *Driver) RemoveImage(ctx context.Context, image string) error {
	slog.Info("runtime command", "runtime", "docker", "args", []string{"image", "rm", image})
	out, err := exec.CommandContext(ctx, "docker", "image", "rm", image).CombinedOutput()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("docker image rm: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (d *Driver) InspectImage(ctx context.Context, image string) (runtime.ImageInspection, error) {
	// Render the complete object instead of indexing optional Config fields in a
	// Go template. Docker omits nil fields (for example Entrypoint on alpine),
	// and the template engine treats a missing map key as an error.
	out, err := output(ctx, 10*time.Second, "image", "inspect", "--format", "{{json .}}", image)
	if err != nil {
		return runtime.ImageInspection{}, fmt.Errorf("docker image inspect: %w: %s", err, strings.TrimSpace(out))
	}
	var raw struct {
		ID     string `json:"Id"`
		Size   *int64 `json:"Size"`
		Config struct {
			Entrypoint []string       `json:"Entrypoint"`
			Cmd        []string       `json:"Cmd"`
			Env        []string       `json:"Env"`
			Volumes    map[string]any `json:"Volumes"`
		} `json:"Config"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &raw); err != nil {
		return runtime.ImageInspection{}, err
	}
	env := map[string]string{}
	for _, kv := range raw.Config.Env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	vols := make([]string, 0, len(raw.Config.Volumes))
	for v := range raw.Config.Volumes {
		vols = append(vols, v)
	}
	if raw.Size != nil && *raw.Size < 0 {
		raw.Size = nil
	}
	return runtime.ImageInspection{RuntimeSizeBytes: raw.Size, ID: raw.ID, Entrypoint: raw.Config.Entrypoint, Cmd: raw.Config.Cmd, Env: env, Volumes: vols}, nil
}

// PullImage pulls an image, streaming progress lines to onLine.
func (d *Driver) PullImage(ctx context.Context, image string, onLine func(string)) error {
	if err := checkPullProxy(ctx); err != nil {
		return err
	}
	slog.Info("runtime command", "runtime", "docker", "args", []string{"pull", image})
	// A registry login rides in a private config directory for this pull only,
	// never in the arguments or the user's own docker config.
	env, cleanupAuth, err := runtime.PullAuthEnv(ctx, image, os.Environ())
	if err != nil {
		return err
	}
	defer cleanupAuth()
	cmd := exec.CommandContext(ctx, "docker", "pull", image)
	cmd.Env = env
	// Use a manually-created pipe or terminal instead of StdoutPipe. exec.Cmd.Wait
	// closes the parent end of a StdoutPipe as soon as the child exits, which
	// races the scanner below for fast-failing docker commands and can drop all
	// output.
	stdout, childEnd, err := pullOutput(cmd)
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = childEnd.Close()
		return err
	}
	// The child owns its end. Keeping the parent's copy open would make the
	// scanner wait forever after a normal CLI exits.
	_ = childEnd.Close()
	type scanResult struct {
		tail string
		err  error
	}
	scanDone := make(chan scanResult, 1)
	go func() {
		var outputTail string
		var check pullStatusCheck
		err := scanPullLines(stdout, func(line string) {
			outputTail = tailOutput(outputTail+"\n"+line, 4096)
			if onLine != nil {
				if notice := check.notice(line); notice != "" {
					onLine(notice)
				}
				onLine(line)
			}
		})
		scanDone <- scanResult{tail: outputTail, err: err}
	}()
	err = cmd.Wait()
	// A helper process may inherit docker's progress descriptor and keep it open
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
		slog.Warn("runtime command failed", "runtime", "docker", "args", []string{"pull", image}, "err", err, "stderr_tail", scan.tail)
		if scan.tail != "" {
			return fmt.Errorf("docker pull: %w: %s", err, scan.tail)
		}
	}
	return err
}

// TagImage records a source-selected pull under the canonical catalog ref.
func (d *Driver) TagImage(ctx context.Context, source, canonical string) error {
	slog.Info("runtime command", "runtime", "docker", "args", []string{"tag", source, canonical})
	return exec.CommandContext(ctx, "docker", "tag", source, canonical).Run()
}

// HoldsImage reports whether docker holds image, for a node switching from
// docker to another runtime.
func (d *Driver) HoldsImage(ctx context.Context, image string) (bool, error) {
	return d.ImageExists(ctx, image)
}

func (d *Driver) SaveImage(ctx context.Context, image string, w io.Writer) error {
	slog.Debug("runtime command", "runtime", "docker", "args", []string{"save", image})
	cmd := exec.CommandContext(ctx, "docker", "save", image)
	cmd.Stdout = w
	cmd.Stderr = io.Discard
	return cmd.Run()
}

func (d *Driver) LoadImage(ctx context.Context, image string, r io.Reader) error {
	_, err := d.LoadImageRef(ctx, image, r)
	return err
}

// LoadImageRef loads a streamed archive and returns the reference docker named
// on stdout ("Loaded image: <ref>"), or "" when the archive carried no tag.
// A relayed archive is never a file here, so this is the only place its real
// reference can be learned.
func (d *Driver) LoadImageRef(ctx context.Context, image string, r io.Reader) (string, error) {
	slog.Info("runtime command", "runtime", "docker", "args", []string{"load"}, "image", image)
	cmd := exec.CommandContext(ctx, "docker", "load")
	cmd.Stdin = r
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("docker load: %w: %s", err, msg)
		}
		return "", fmt.Errorf("docker load: %w", err)
	}
	return runtime.ParseLoadedImageRefFor(stdout.String(), image), nil
}

// LoadImageFile imports an archive that is already a file on disk. Handing
// docker the path instead of a reader avoids a second full-size copy of a
// multi-gigabyte archive and lets docker report what it actually imported.
func (d *Driver) LoadImageFile(ctx context.Context, image, path string) error {
	_, err := d.LoadImageFileRef(ctx, image, path)
	return err
}

// LoadImageFileRef loads the archive and returns the reference docker named on
// stdout ("Loaded image: <ref>"), or "" when the archive carried no tag.
func (d *Driver) LoadImageFileRef(ctx context.Context, image, path string) (string, error) {
	slog.Info("runtime command", "runtime", "docker", "args", []string{"load", "-i", path}, "image", image)
	cmd := exec.CommandContext(ctx, "docker", "load", "-i", path)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("docker load -i: %w: %s", err, msg)
		}
		return "", fmt.Errorf("docker load -i: %w", err)
	}
	return runtime.ParseLoadedImageRefFor(stdout.String(), image), nil
}

var (
	_ runtime.LoadedRefReporter   = (*Driver)(nil)
	_ runtime.StreamedRefReporter = (*Driver)(nil)
	_ runtime.ImageTagger         = (*Driver)(nil)
	_ runtime.LocalImageSource    = (*Driver)(nil)
)

// Prepare is a no-op for docker (image pull handled separately).
func (d *Driver) Prepare(ctx context.Context, spec runtime.WorkloadSpec) error { return nil }

// ListContainers lists container names, including stopped ones. The command and
// its format are part of the contract with the one-VM-two-logical-nodes wrapper,
// which scopes this listing to a single logical node's containers.
func (d *Driver) ListContainers(ctx context.Context) ([]string, error) {
	out, err := output(ctx, 10*time.Second, "ps", "-a", "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	return splitOutputLines(out), nil
}

// RunningContainers lists the names of the containers docker reports as up:
// `docker ps` without -a. It is a `ps` invocation like ListContainers, so the
// one-VM-two-logical-nodes wrapper scopes it the same way.
func (d *Driver) RunningContainers(ctx context.Context) ([]string, error) {
	out, err := output(ctx, 10*time.Second, "ps", "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	return splitOutputLines(out), nil
}

func buildRunArgs(spec runtime.WorkloadSpec) []string {
	args := []string{"run", "-d", "--name", spec.Name, "--restart", "no"}

	// Linux can reach host-network containers directly. Docker Desktop runs
	// the daemon in a VM, so a Darwin/Windows agent must publish the same port
	// explicitly even when the server spec requests host networking; otherwise
	// health/proxy probes would target the host while the port exists only in
	// the VM.
	if spec.HostNetwork && hostruntime.GOOS == "linux" {
		args = append(args, "--network", "host")
	} else {
		for _, p := range spec.Ports {
			args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", p, p))
		}
	}

	args = append(args, spec.DockerArgs...)

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
	_, err := outputMutating(ctx, 30*time.Second, "stop", "-t", "10", id)
	return err
}

// Remove force-deletes a container.
func (d *Driver) Remove(ctx context.Context, id string) error {
	if err := d.checkPendingRun(ctx, id); err != nil {
		return err
	}
	return d.remove(ctx, id)
}

func (d *Driver) remove(ctx context.Context, id string) error {
	// The daemon's "No such container" is in the CLI output, not in err.
	out, err := outputMutating(ctx, 30*time.Second, "rm", "-f", id)
	if err != nil && strings.Contains(out, "No such container") {
		return nil
	}
	return err
}

// Status inspects a container.
func (d *Driver) Status(ctx context.Context, id string) (runtime.Status, error) {
	if err := d.checkPendingRun(ctx, id); err != nil {
		return runtime.Status{}, err
	}
	format := "{{.State.Running}}|{{.State.ExitCode}}|{{.State.OOMKilled}}|{{if .State.Health}}{{.State.Health.Status}}{{end}}"
	out, err := output(ctx, 10*time.Second, "inspect", "--format", format, id)
	if err != nil {
		detail := strings.ToLower(out + " " + err.Error())
		if strings.Contains(detail, "no such object") || strings.Contains(detail, "no such container") {
			return runtime.Status{Exists: false}, nil
		}
		return runtime.Status{}, err
	}
	// Output that does not parse is an error, not a stopped container: the
	// liveness monitor retires an instance on "exists but not running". The
	// template result is the last line; combined output may carry warnings.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	line := strings.TrimSpace(lines[len(lines)-1])
	parts := strings.Split(line, "|")
	exitCode, convErr := 0, errors.New("missing exit code")
	if len(parts) == 4 {
		exitCode, convErr = strconv.Atoi(parts[1])
	}
	if convErr != nil || (parts[0] != "true" && parts[0] != "false") {
		return runtime.Status{}, fmt.Errorf("unexpected docker inspect output %q", line)
	}
	return runtime.Status{Exists: true, Running: parts[0] == "true", ExitCode: exitCode, OOMKilled: parts[2] == "true", Health: parts[3]}, nil
}

// Logs returns the container's log stream.
func (d *Driver) Logs(ctx context.Context, id string, follow bool, tail int) (io.ReadCloser, error) {
	args := []string{"logs"}
	if follow {
		args = append(args, "-f")
	}
	if tail > 0 {
		args = append(args, "--tail", strconv.Itoa(tail))
	}
	args = append(args, id)
	cmd := exec.CommandContext(ctx, "docker", args...)
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
	args := append([]string{"exec", id}, cmd...)
	slog.Info("runtime command", "runtime", "docker", "args", args)
	c := exec.CommandContext(ctx, "docker", args...)
	out, err := c.CombinedOutput()
	if err != nil {
		slog.Warn("runtime command failed", "runtime", "docker", "args", args, "err", err, "stderr_tail", tailOutput(string(out), 400))
	}
	return out, err
}

// ExecPTY returns a docker exec command that attaches to the existing
// container's namespaces. The agent owns PTY allocation and lifecycle; keeping
// this method argv-only avoids importing terminal code into the runtime layer.
func (d *Driver) ExecPTY(_ context.Context, id string, cmd []string) ([]string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("docker exec requires a container id")
	}
	host, ok := localDockerHost()
	if !ok {
		return nil, errors.New("docker container terminal requires an available local unix socket and default context")
	}
	host, err := filepath.Abs(host)
	if err != nil {
		return nil, fmt.Errorf("resolve docker container terminal endpoint: %w", err)
	}
	if len(cmd) == 0 {
		cmd = []string{"/bin/sh", "-l"}
	}
	// Terminal sessions construct a clean environment. Pin the validated node
	// endpoint in argv so exec cannot silently fall back to another daemon.
	args := []string{"docker", "--host", "unix://" + host, "exec", "-i", "-t", id}
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

// output runs docker with a timeout and returns combined output.
func output(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	slog.Debug("runtime command", "runtime", "docker", "args", args)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 400 {
			msg = msg[len(msg)-400:]
		}
		// "No such image/object" answers a lookup; it is not a failure of
		// the command (the image inventory asks this of every absent catalog
		// image every minute).
		level := slog.LevelWarn
		if lower := strings.ToLower(msg); strings.Contains(lower, "no such image:") || strings.Contains(lower, "no such object") {
			level = slog.LevelDebug
		}
		slog.Log(ctx, level, "runtime command failed", "runtime", "docker", "args", args, "err", err, "stderr_tail", msg)
	}
	return string(out), err
}

// outputMutating runs a daemon-state-changing command. Mutating commands are
// the audit trail for "who touched this node", so they log at INFO even on
// success; pure reads through output stay at Debug.
func outputMutating(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	slog.Info("runtime command", "runtime", "docker", "args", args)
	return output(ctx, timeout, args...)
}

// procReadCloser wraps the log pipe so Close also kills the docker logs process.
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
