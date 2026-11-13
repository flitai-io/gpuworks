package launchd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Host is what installing the job touches: launchctl, the plist files and the
// accounts. NewHost is the real Mac; tests give a fake launchctl and a root
// directory the plists are written under.
type Host struct {
	// Launchctl runs launchctl with args and returns its combined output.
	Launchctl func(ctx context.Context, args ...string) (string, error)
	// Root prefixes every plist path; "" on a real Mac.
	Root string
	// ProcessAlive observes whether a former job PID remains. Nil uses signal 0.
	ProcessAlive func(pid int) (bool, error)
	// ProcessTable supplies the read-only ps snapshot used to detect leftover agents.
	ProcessTable func(context.Context) (string, error)
	// Admin is true when running as root: the job is then a launch daemon.
	Admin bool
	// Self is the account running the install, and the one a launch agent
	// belongs to.
	Self Account
	// Lookup finds the account a launch daemon runs as.
	Lookup func(name string) (Account, error)
	// Chown changes a path's owner (os.Lchown).
	Chown func(path string, uid, gid int) error
	// Out takes the install's messages to the person running it.
	Out io.Writer
	// Wait is how long launchd gets to let go of an old job and to keep the
	// new one running; Tick is how often it is asked. Stable is how long the
	// new agent must keep one pid to count as running: longer than the plist's
	// ThrottleInterval, so a crash loop is not mistaken for it.
	Wait, Tick, Stable time.Duration
}

// Account is a macOS user.
type Account struct {
	Name     string
	UID, GID int
	Home     string
}

// ErrNoSession reports a launch agent that cannot start: its user has no
// login session on the Mac.
var ErrNoSession = errors.New("no login session")

type target struct {
	domain string // system or gui/<uid>
	plist  string // without Root
	owner  Account
}

func (h *Host) target(job Job) (target, error) {
	if !h.Admin {
		return target{domain: "gui/" + strconv.Itoa(h.Self.UID), plist: AgentPlist(h.Self.Home), owner: h.Self}, nil
	}
	if job.User == "" || job.User == "root" {
		// The agent starts engines and downloads models; the Metal GPU needs
		// no privileges, so it runs as the person who installed it.
		return target{}, fmt.Errorf("the agent never runs as root on a Mac: install from the account it should run as (with sudo)")
	}
	owner, err := h.Lookup(job.User)
	if err != nil {
		return target{}, fmt.Errorf("look up %s: %w", job.User, err)
	}
	return target{domain: "system", plist: DaemonPlist(), owner: owner}, nil
}

func (h *Host) path(p string) string { return filepath.Join(h.Root, p) }

func (h *Host) say(format string, args ...any) {
	if h.Out != nil {
		fmt.Fprintf(h.Out, "gpuworks: "+format+"\n", args...)
	}
}

// Install registers job with launchd and waits until launchd keeps it
// running. It replaces the job an earlier install registered, for this server
// or another (and says which). When launchd refuses the new job, the previous
// plist is put back and started, so a failed reinstall does not leave the Mac
// without an agent.
func (h *Host) Install(ctx context.Context, job Job) (pid int, err error) {
	t, err := h.target(job)
	if err != nil {
		return 0, err
	}
	if !h.Admin {
		job.User = ""
		if _, err := h.Launchctl(ctx, "print", t.domain); err != nil {
			return 0, fmt.Errorf("%w: %s is not logged in on this Mac, so a launch agent cannot start; log in on the Mac, or install with administrator rights to run the agent from boot", ErrNoSession, t.owner.Name)
		}
		if _, err := os.Stat(h.path(DaemonPlist())); err == nil {
			// Beside it a launch agent would be a second agent on the same GPU.
			return 0, fmt.Errorf("a gpuworks launch daemon is installed on this Mac, and replacing it needs administrator rights: run the install with sudo, or remove it first (sudo gpuworks-agent uninstall)")
		}
	}
	plist := h.path(t.plist)
	previous, _ := os.ReadFile(plist)
	if old, err := ReadJob(previous); err == nil && trimServer(old.Server) != "" && trimServer(old.Server) != trimServer(job.Server) {
		h.say("WARNING: replacing the gpuworks agent installed for another server (%s); that server loses this Mac", old.Server)
	}
	if h.Admin {
		// The label lives in one domain at a time: a daemon replaces the
		// launch agent an install without administrator rights left.
		h.removeAgentOf(ctx, t.owner)
		for _, dir := range []string{filepath.Dir(job.Binary), job.StateDir} {
			if err := h.adopt(dir, t.owner); err != nil {
				return 0, err
			}
		}
	}
	if err := h.writePlist(plist, job.Plist()); err != nil {
		return 0, err
	}
	if err := h.load(ctx, t); err != nil {
		if len(previous) == 0 {
			return 0, err
		}
		if restoreErr := h.writePlist(plist, previous); restoreErr == nil && h.load(ctx, t) == nil {
			return 0, fmt.Errorf("%w; the previous agent job was restored", err)
		}
		return 0, fmt.Errorf("%w; the previous agent job could not be restored either", err)
	}
	return h.awaitRunning(ctx, t, job.StateDir)
}

// load replaces whatever runs under the label with the plist's job.
func (h *Host) load(ctx context.Context, t target) error {
	service := t.domain + "/" + AgentLabel
	_, _ = h.Launchctl(ctx, "bootout", service)
	// bootout returns before launchd has let go of a running job, and a
	// bootstrap before then fails with "5: Input/output error".
	for deadline := time.Now().Add(h.Wait); time.Now().Before(deadline); {
		if _, err := h.Launchctl(ctx, "print", service); err != nil {
			break
		}
		if err := h.sleep(ctx); err != nil {
			return err
		}
	}
	var out string
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if out, err = h.Launchctl(ctx, "bootstrap", t.domain, h.path(t.plist)); err == nil {
			return nil
		}
		if err := h.sleep(ctx); err != nil {
			return err
		}
	}
	return fmt.Errorf("launchd refused %s: %s", t.plist, strings.TrimSpace(out))
}

var (
	printState   = regexp.MustCompile(`(?m)^\s*state = (\S+)`)
	printProgram = regexp.MustCompile(`(?m)^\s*program = ([^\r\n]+)`)
	printPID     = regexp.MustCompile(`(?m)^\s*pid = (\d+)`)
)

// RunningPID reads the output of `launchctl print <domain>/<label>`: the pid
// of a job launchd reports running, or 0.
func RunningPID(out string) int {
	state := printState.FindStringSubmatch(out)
	pid := printPID.FindStringSubmatch(out)
	if state == nil || state[1] != "running" || pid == nil {
		return 0
	}
	n, _ := strconv.Atoi(pid[1])
	return n
}

// awaitRunning waits for launchd to report the job running under one pid
// for Stable: KeepAlive restarts an agent that exits at once, which a single
// look would take for a running one.
func (h *Host) awaitRunning(ctx context.Context, t target, stateDir string) (int, error) {
	service := t.domain + "/" + AgentLabel
	last, since := 0, time.Time{}
	for deadline := time.Now().Add(h.Wait + h.Stable); time.Now().Before(deadline); {
		out, err := h.Launchctl(ctx, "print", service)
		pid := 0
		if err == nil {
			pid = RunningPID(out)
		}
		if pid != last {
			last, since = pid, time.Now()
		}
		if pid > 0 && time.Since(since) >= h.Stable {
			return pid, nil
		}
		if err := h.sleep(ctx); err != nil {
			return 0, err
		}
	}
	stderrLog := filepath.Join(stateDir, StderrLog)
	detail := ""
	if data, err := os.ReadFile(stderrLog); err == nil {
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(lines) > 3 {
			lines = lines[len(lines)-3:]
		}
		detail = ": " + strings.Join(lines, " | ")
	}
	return 0, fmt.Errorf("launchd does not keep the agent running; see %s%s", stderrLog, detail)
}

func (h *Host) sleep(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(h.Tick):
		return nil
	}
}

// removeAgentOf removes the launch agent of owner's session, if any.
func (h *Host) removeAgentOf(ctx context.Context, owner Account) {
	plist := h.path(AgentPlist(owner.Home))
	if _, err := os.Stat(plist); err != nil {
		return
	}
	_, _ = h.Launchctl(ctx, "bootout", "gui/"+strconv.Itoa(owner.UID)+"/"+AgentLabel)
	if err := os.Remove(plist); err == nil {
		h.say("removed %s's launch agent: the launch daemon replaces it", owner.Name)
	}
}

// writePlist puts data in place atomically. The plist carries the node token,
// so it is readable by its owner only; a daemon's belongs to root, as launchd
// requires.
func (h *Host) writePlist(plist string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(plist), "."+AgentLabel+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), plist)
}

// adopt gives owner what a previous install left in dir under another account
// (root, or the user of an earlier install): the agent must be able to write
// its state and models. Only a directory not already owner's is walked.
func (h *Host) adopt(dir string, owner Account) error {
	if dir == "" {
		return nil
	}
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if uid, ok := fileUID(info); ok && uid == owner.UID {
		return nil
	}
	return filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := h.Chown(p, owner.UID, owner.GID); err != nil {
			return fmt.Errorf("give %s to %s: %w", p, owner.Name, err)
		}
		return nil
	})
}

// Uninstall removes the job from both domains it may be in: the launch daemon
// and the launch agent of the user (the one sudo was run from, as root). It
// returns the state directories the removed jobs named. Engines the agent
// started are process groups of their own and keep running. beforeRemove runs
// after every job/process has stopped and before any plist is removed, allowing
// identity cleanup failures to retain discoverable installation definitions.
func (h *Host) Uninstall(ctx context.Context, binary string, beforeRemove func(context.Context, []string) error) ([]string, error) {
	type job struct {
		domain, plist, state string
		data                 []byte
	}
	jobs := []job{{domain: "system", plist: DaemonPlist()}, {domain: "gui/" + strconv.Itoa(h.Self.UID), plist: AgentPlist(h.Self.Home)}}
	// Validate every installed definition before touching either domain.
	for i := range jobs {
		j := &jobs[i]
		data, err := os.ReadFile(h.path(j.plist))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, errors.New("launchd definition could not be read")
		}
		if j.domain == "system" && !h.Admin {
			return nil, errors.New("removing the launch daemon needs administrator rights: sudo gpuworks-agent uninstall")
		}
		parsed, err := ReadOwnedAgentJob(data, binary)
		if err != nil {
			return nil, err
		}
		if _, err := h.AgentProcesses(ctx, binary, parsed.StateDir); err != nil {
			return nil, err
		}
		j.data, j.state = data, parsed.StateDir
	}
	// Keep every plist discoverable until every job/process has stopped. A
	// failure in the second domain must not hide the first state on retry.
	for _, j := range jobs {
		if j.data == nil {
			continue
		}
		current, err := os.ReadFile(h.path(j.plist))
		if err != nil || !bytes.Equal(current, j.data) {
			return nil, errors.New("launchd definition changed before shutdown")
		}
		if err := h.stopUninstallJob(ctx, j.domain+"/"+AgentLabel, binary, j.state); err != nil {
			return nil, err
		}
	}
	for _, j := range jobs {
		if j.data == nil {
			continue
		}
		current, err := os.ReadFile(h.path(j.plist))
		if err != nil || !bytes.Equal(current, j.data) {
			return nil, errors.New("launchd definition changed during shutdown; definitions retained")
		}
		out, err := h.Launchctl(ctx, "print", j.domain+"/"+AgentLabel)
		if !IsAbsentJob(out, err) {
			return nil, errors.New("launchd job reappeared before cleanup; definitions retained")
		}
		pids, err := h.AgentProcesses(ctx, binary, j.state)
		if err != nil {
			return nil, err
		}
		if len(pids) > 0 {
			return nil, errors.New("agent process reappeared before cleanup; definitions retained")
		}
	}
	if beforeRemove != nil {
		var stoppedStates []string
		for _, j := range jobs {
			if j.data != nil {
				stoppedStates = append(stoppedStates, j.state)
			}
		}
		if err := beforeRemove(ctx, stoppedStates); err != nil {
			return nil, err
		}
	}
	var stateDirs []string
	for _, j := range jobs {
		if j.data == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return stateDirs, err
		}
		if err := os.Remove(h.path(j.plist)); err != nil {
			return stateDirs, err
		}
		stateDirs = append(stateDirs, j.state)
		h.say("removed %s", j.plist)
	}
	return stateDirs, nil
}

// IsAbsentJob reports only launchctl errors that explicitly mean an absent
// service/domain status. Permission, cancellation and transport failures cannot
// establish that stopping the job succeeded.
func IsAbsentJob(out string, err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var status interface{ ExitCode() int }
	if !errors.As(err, &status) {
		return false
	}
	switch status.ExitCode() {
	case 3:
		return strings.Contains(out, "No such process")
	case 113:
		return strings.Contains(out, "Could not find service") || strings.Contains(out, "Could not find domain")
	}
	return false
}

func (h *Host) stopUninstallJob(ctx context.Context, service, binary, state string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	out, err := h.Launchctl(ctx, "print", service)
	if err != nil && !IsAbsentJob(out, err) {
		return errors.New("launchd job state could not be observed before stopping")
	}
	pid := 0
	if err == nil {
		pid, err = AgentProgramPID(out, binary)
		if err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	out, err = h.Launchctl(ctx, "bootout", service)
	if err != nil && !IsAbsentJob(out, err) {
		return errors.New("launchd job could not be stopped; autostart definition retained")
	}
	deadline := time.Now().Add(h.Wait)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		out, err = h.Launchctl(ctx, "print", service)
		absent := IsAbsentJob(out, err)
		if err != nil && !absent {
			return errors.New("launchd job shutdown could not be verified")
		}
		alive := false
		if pid > 0 {
			alive, err = h.uninstallProcessAlive(pid)
			if err != nil {
				return errors.New("former launchd job process exit could not be verified")
			}
		}
		if absent && !alive {
			pids, inventoryErr := h.AgentProcesses(ctx, binary, state)
			if inventoryErr != nil {
				return inventoryErr
			}
			if len(pids) == 0 {
				return nil
			}
		}
		if !time.Now().Before(deadline) {
			return errors.New("launchd job or process has not stopped; autostart definition retained")
		}
		if err := h.sleep(ctx); err != nil {
			return err
		}
	}
}

func (h *Host) uninstallProcessAlive(pid int) (bool, error) {
	if h.ProcessAlive != nil {
		return h.ProcessAlive(pid)
	}
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if err == nil || errors.Is(err, syscall.EPERM) {
		return true, nil
	}
	return false, err
}

// ReadOwnedAgentJob accepts only the agent label and run command using the
// explicitly selected executable and one absolute installation state directory.
// It never includes untrusted plist arguments or environment in errors.
func ReadOwnedAgentJob(data []byte, binary string) (Job, error) {
	spec, err := ReadSpec(data)
	if err != nil || spec.Label != AgentLabel || len(spec.Args) < 4 || spec.Args[1] != "run" {
		return Job{}, errors.New("launchd definition is not an owned agent run job")
	}
	if !sameAgentPath(spec.Args[0], binary) || (spec.Program != "" && !sameAgentPath(spec.Program, binary)) {
		return Job{}, errors.New("launchd executable identity differs from this agent")
	}
	states := 0
	for _, arg := range spec.Args[2:] {
		if arg == "--state" {
			states++
		}
		if strings.HasPrefix(arg, "--state=") {
			return Job{}, errors.New("launchd state arguments are ambiguous")
		}
	}
	job, err := ReadJob(data)
	if err != nil || states != 1 || !filepath.IsAbs(job.StateDir) || filepath.Clean(job.StateDir) == string(filepath.Separator) || strings.ContainsAny(job.StateDir, "\r\n\x1b") {
		return Job{}, errors.New("launchd installation state could not be established")
	}
	return job, nil
}

// AgentProgramPID validates a loaded job's executable before returning its PID.
// A foreign or unobservable live program must not be stopped by agent uninstall.
func AgentProgramPID(out, binary string) (int, error) {
	program := printProgram.FindStringSubmatch(out)
	if program == nil || !sameAgentPath(strings.Trim(strings.TrimSpace(program[1]), "\""), binary) {
		return 0, errors.New("loaded launchd executable identity could not be established")
	}
	pid := printPID.FindStringSubmatch(out)
	if pid == nil {
		state := printState.FindStringSubmatch(out)
		if state == nil || state[1] == "running" {
			return 0, errors.New("loaded launchd process identity could not be established")
		}
		return 0, nil
	}
	n, err := strconv.Atoi(pid[1])
	if err != nil || n <= 0 {
		return 0, errors.New("loaded launchd process identity could not be established")
	}
	return n, nil
}

func sameAgentPath(a, b string) bool {
	if !filepath.IsAbs(a) || !filepath.IsAbs(b) {
		return false
	}
	resolve := func(path string) string {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return resolved
		}
		return filepath.Clean(path)
	}
	return resolve(a) == resolve(b)
}
