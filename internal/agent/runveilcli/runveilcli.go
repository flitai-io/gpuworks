// Package runveilcli implements `gpuworks-agent runveil`, the operator's view
// of a node's runveil containers: ps, images, load, exec and logs. It works from
// the udocker driver's on-disk state, so it runs beside the agent without
// talking to it, and needs no Python or udocker CLI on the node.
package runveilcli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime/udocker"
)

const usageText = `usage: gpuworks-agent runveil [--state DIR] <command>

Commands:
  ps [-a]                     list running containers (-a: all)
  images                      list images
  load -i FILE REF            import a docker save or OCI archive (.tar or
                              .tar.gz) under REF, the image's reference in
                              the platform's image catalog
  exec NAME [CMD [ARG...]]    run CMD (default: a login shell) in a container,
                              with the workload's environment, mounts and GPUs
  logs [-f] [-n N] NAME       print a container's output (-f: follow)

NAME may be any unique prefix of a container name. --state defaults to the
agent's state directory (GPUWORKS_STATE_DIR, else /opt/gpuworks).
`

// CLI carries the command's collaborators; tests replace Run and Now.
type CLI struct {
	Driver *udocker.Driver
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Run executes argv and returns its exit status; nil runs it with the
	// CLI's stdio (RunCommand).
	Run func(ctx context.Context, argv []string) (int, error)
	Now func() time.Time
	// Poll is how often logs -f looks for new output.
	Poll time.Duration
	// Geteuid reports the caller's effective uid; nil means os.Geteuid.
	Geteuid func() int
}

// Main runs the command line args (after "runveil") and returns the exit
// status. defaultState is the agent's state directory.
func Main(ctx context.Context, args []string, defaultState string) int {
	global := flag.NewFlagSet("runveil", flag.ContinueOnError)
	global.SetOutput(os.Stderr)
	global.Usage = func() { fmt.Fprint(os.Stderr, usageText) }
	state := global.String("state", defaultState, "agent state directory")
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	cli := &CLI{
		Driver: udocker.New(*state),
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Now:    time.Now,
		Poll:   500 * time.Millisecond,
	}
	return cli.Execute(ctx, global.Args())
}

// Execute runs one subcommand and returns the exit status.
func (c *CLI) Execute(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(c.Stderr, usageText)
		return 2
	}
	var err error
	status := 0
	switch args[0] {
	case "ps":
		err = c.ps(ctx, args[1:])
	case "images":
		err = c.images(args[1:])
	case "load":
		err = c.load(ctx, args[1:])
	case "exec":
		status, err = c.exec(ctx, args[1:])
	case "logs":
		err = c.logs(ctx, args[1:])
	case "help", "-h", "--help":
		fmt.Fprint(c.Stdout, usageText)
		return 0
	default:
		fmt.Fprintf(c.Stderr, "gpuworks-agent runveil: unknown command %q\n\n%s", args[0], usageText)
		return 2
	}
	var usage usageError
	switch {
	case errors.As(err, &usage):
		fmt.Fprintf(c.Stderr, "gpuworks-agent runveil %s: %v\n", args[0], err)
		return 2
	case err != nil:
		fmt.Fprintf(c.Stderr, "gpuworks-agent runveil %s: %v\n", args[0], err)
		return 1
	}
	return status
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func (c *CLI) ps(ctx context.Context, args []string) error {
	fs := c.flags("ps")
	all := fs.Bool("a", false, "show all containers, not only running ones")
	fs.BoolVar(all, "all", false, "same as -a")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usageError{"ps takes no arguments"}
	}
	if err := c.requireState(); err != nil {
		return err
	}
	containers, err := c.Driver.Containers(ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(c.Stdout, 0, 4, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tIMAGE\tSTATUS\tPID\tCREATED")
	for _, ct := range containers {
		if !*all && ct.State != udocker.ContainerRunning {
			continue
		}
		pid := "-"
		if ct.PID > 0 {
			pid = fmt.Sprint(ct.PID)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", ct.Name, orDash(ct.Image), ct.State, pid, c.ago(ct.Created))
	}
	return tw.Flush()
}

func (c *CLI) images(args []string) error {
	if len(args) > 0 {
		return usageError{"images takes no arguments"}
	}
	if err := c.requireState(); err != nil {
		return err
	}
	images, err := c.Driver.Images()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(c.Stdout, 0, 4, 3, ' ', 0)
	fmt.Fprintln(tw, "REPOSITORY\tTAG\tIMAGE ID\tSIZE")
	for _, image := range images {
		repo, tag := splitReference(image.Reference)
		id, size := "-", "-"
		if image.ID != "" {
			id = shortID(image.ID)
			size = humanSize(image.Size)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", repo, tag, id, size)
	}
	return tw.Flush()
}

// load imports an archive copied onto the node, the runveil counterpart of
// `docker load` for nodes the console cannot reach an archive for. REF is
// required: the platform finds images by their catalog reference, which the
// tag recorded in the archive need not match.
func (c *CLI) load(ctx context.Context, args []string) error {
	fs := c.flags("load")
	input := fs.String("i", "", "image archive to import")
	fs.StringVar(input, "input", "", "same as -i")
	// Accept the flag on either side of the reference, as logs does.
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return usageError{err.Error()}
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if *input == "" {
		return usageError{"load needs -i FILE"}
	}
	if len(positional) != 1 {
		return usageError{"load needs exactly one image reference (as the image catalog shows it)"}
	}
	if err := c.requireState(); err != nil {
		return err
	}
	if err := c.requireStateOwner(); err != nil {
		return err
	}
	ref := positional[0]
	if err := c.Driver.ImportArchive(ctx, ref, *input); err != nil {
		return err
	}
	fmt.Fprintf(c.Stdout, "Loaded image: %s\n", ref)
	return nil
}

func (c *CLI) exec(ctx context.Context, args []string) (int, error) {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return 0, usageError{"exec needs a container name"}
	}
	if err := c.requireState(); err != nil {
		return 0, err
	}
	name, err := c.resolve(ctx, args[0])
	if err != nil {
		return 0, err
	}
	if err := c.requireAgentUser(name); err != nil {
		return 0, err
	}
	cmd := args[1:]
	if len(cmd) > 0 && cmd[0] == "--" {
		cmd = cmd[1:]
	}
	argv, err := c.Driver.ExecPTY(ctx, name, cmd)
	if err != nil {
		return 0, err
	}
	if c.Run != nil {
		return c.Run(ctx, argv)
	}
	return c.RunCommand(ctx, argv)
}

func (c *CLI) logs(ctx context.Context, args []string) error {
	fs := c.flags("logs")
	follow := fs.Bool("f", false, "follow the output")
	fs.BoolVar(follow, "follow", false, "same as -f")
	lines := fs.Int("n", -1, "number of lines to show from the end (default: all)")
	fs.IntVar(lines, "tail", -1, "same as -n")
	// Accept flags on either side of the name, as docker logs does.
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return usageError{err.Error()}
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(positional) != 1 {
		return usageError{"logs needs exactly one container name"}
	}
	if err := c.requireState(); err != nil {
		return err
	}
	name, err := c.resolve(ctx, positional[0])
	if err != nil {
		return err
	}
	path := c.Driver.LogPath(name)
	offset, err := printTail(c.Stdout, path, *lines)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("container %s has no output yet (it was never started on this node)", name)
	}
	if err != nil || !*follow {
		return err
	}
	return c.follow(ctx, name, path, offset)
}

// follow copies what is appended to path until the container stops running
// and nothing more arrives, or ctx ends.
func (c *CLI) follow(ctx context.Context, name, path string, offset int64) error {
	ticker := time.NewTicker(c.Poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		copied, next, err := copyFrom(c.Stdout, path, offset)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		offset = next
		if copied == 0 && !c.running(ctx, name) {
			return nil
		}
	}
}

func (c *CLI) running(ctx context.Context, name string) bool {
	containers, err := c.Driver.Containers(ctx)
	if err != nil {
		return false
	}
	for _, ct := range containers {
		if ct.Name == name {
			return ct.State == udocker.ContainerRunning
		}
	}
	return false
}

// resolve accepts a full container name or a unique prefix of one.
func (c *CLI) resolve(ctx context.Context, name string) (string, error) {
	containers, err := c.Driver.Containers(ctx)
	if err != nil {
		return "", err
	}
	var matches []string
	for _, ct := range containers {
		if ct.Name == name {
			return name, nil
		}
		if strings.HasPrefix(ct.Name, name) {
			matches = append(matches, ct.Name)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no container named %q", name)
	case 1:
		return matches[0], nil
	default:
		sort.Strings(matches)
		return "", fmt.Errorf("%q matches several containers: %s", name, strings.Join(matches, ", "))
	}
}

// requireState fails clearly when the state directory holds no runveil
// state, which is what a wrong --state (or a docker node) looks like. Other
// users may read it: ps, images and logs report the same as for root.
func (c *CLI) requireState() error {
	udockerDir, _ := c.Driver.StateDirs()
	if _, err := os.Stat(udockerDir); err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("cannot read the runveil state in %s: run as root (sudo) or as the agent's user", filepath.Dir(udockerDir))
		}
		return fmt.Errorf("no runveil state in %s: pass the agent's --state (or GPUWORKS_STATE_DIR), or this node does not use the runveil runtime", filepath.Dir(udockerDir))
	}
	return nil
}

// requireAgentUser lets exec run only as root or as the agent's user, the
// owner of the workload spec the agent wrote when it started name. A shell
// in the container runs as the runtime user with the workload's devices,
// which only those can set up; the owner of the state directory is no
// guide, since an SSH install gives it to the login user.
func (c *CLI) requireAgentUser(name string) error {
	euid := c.geteuid()
	if euid == 0 {
		return nil
	}
	info, err := os.Stat(c.Driver.SpecPath(name))
	if err != nil {
		return nil //nolint:nilerr // ExecPTY owns reporting an absent or unreadable spec; this check only rejects a known different owner.
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint32(euid) == stat.Uid {
		return nil
	}
	return fmt.Errorf("exec needs root (sudo) or the agent's user (uid %d)", stat.Uid)
}

// requireStateOwner lets load run only as root or as the owner of the
// runveil state, the only callers that can write images the run user can
// still read and replace.
func (c *CLI) requireStateOwner() error {
	euid := c.geteuid()
	if euid == 0 {
		return nil
	}
	udockerDir, _ := c.Driver.StateDirs()
	info, err := os.Stat(udockerDir)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint32(euid) == stat.Uid {
		return nil
	}
	return fmt.Errorf("load needs root (sudo) or the owner of %s (uid %d)", udockerDir, stat.Uid)
}

func (c *CLI) geteuid() int {
	if c.Geteuid != nil {
		return c.Geteuid()
	}
	return os.Geteuid()
}

func (c *CLI) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func (c *CLI) ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := c.Now().Sub(t)
	switch {
	case d < time.Minute:
		return "less than a minute ago"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute") + " ago"
	case d < 48*time.Hour:
		return plural(int(d/time.Hour), "hour") + " ago"
	default:
		return plural(int(d/(24*time.Hour)), "day") + " ago"
	}
}

// RunCommand runs argv on the CLI's stdio -- the caller's terminal, for the
// real command -- and returns its exit status. Interrupts belong to the
// child: the shell or command decides what Ctrl-C means.
func (c *CLI) RunCommand(ctx context.Context, argv []string) (int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// The argv drops to the runtime user (runuser) before PRoot starts, and that
	// user can't necessarily enter the caller's cwd (/root, for instance). The
	// container's own working directory is PRoot's -w.
	cmd.Dir = "/"
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Stdin, c.Stdout, c.Stderr
	signal.Ignore(os.Interrupt, syscall.SIGQUIT)
	defer signal.Reset(os.Interrupt, syscall.SIGQUIT)
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal()), nil
		}
		return exit.ExitCode(), nil
	}
	return 0, err
}

func splitReference(ref string) (repo, tag string) {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	if i := strings.LastIndex(ref, ":"); i >= 0 && !strings.Contains(ref[i+1:], "/") {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}

func shortID(id string) string {
	hex := strings.TrimPrefix(id, "sha256:")
	if len(hex) > 12 {
		hex = hex[:12]
	}
	return hex
}

// humanSize renders bytes the way docker does: decimal units, 3 significant
// figures.
func humanSize(n int64) string {
	units := []string{"B", "kB", "MB", "GB", "TB"}
	size := float64(n)
	i := 0
	for size >= 1000 && i < len(units)-1 {
		size /= 1000
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%dB", n)
	}
	return fmt.Sprintf("%.3g%s", size, units[i])
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
