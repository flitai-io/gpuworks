package udocker

// The udocker CLI is optional under runveil: nothing on the launch or image
// path needs it. It is still used for images the CLI itself registered in the
// past (`udocker create`, `rmi`, `tag`, `images`), behind
// GPUWORKS_UDOCKER_DISABLE_NATIVE, and as the whole launcher when
// GPUWORKS_UDOCKER_LAUNCHER=udocker. Everything that installs or invokes it
// lives here.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"gpuworks.ai/gpuworks/internal/assets"
)

func (d *Driver) cliConfigured() bool {
	return d.udockerExe != "" || d.udockerScript != "" || d.pyPath != ""
}

// ensureCLI provisions the udocker CLI on first use under runveil. It needs
// Python, which runveil nodes are not required to have, so its absence is
// reported as the CLI being unavailable rather than failing anything that
// does not need the CLI. Under the udocker launcher, ensureProvisioned has
// already done this.
func (d *Driver) ensureCLI(ctx context.Context) error {
	if d.cliConfigured() || !d.runveil {
		return nil
	}
	d.cliMu.Lock()
	defer d.cliMu.Unlock()
	if d.cliConfigured() {
		return nil
	}
	if !d.detectPython(ctx) {
		return fmt.Errorf("udocker CLI unavailable: python3 >= 3.6 not found (the CLI is needed only for images it registered earlier; re-pull or re-load them)")
	}
	if err := d.provisionCLI(ctx); err != nil {
		return fmt.Errorf("udocker CLI unavailable: %w", err)
	}
	return nil
}

// provisionCLI installs the udocker CLI: the embedded offline bundle, or an
// explicit online install. Under runveil it runs only when a CLI call is
// actually needed.
func (d *Driver) provisionCLI(ctx context.Context) error {
	// The embedded bundle is the normal path. It contains udocker's Python
	// sources, englib and gpuworks' patched static proot, so it is fully offline.
	if d.embeddedBundle {
		if _, err := assets.ExtractUdockerBundle(d.bundleDir); err != nil {
			return fmt.Errorf("extract embedded udocker bundle: %w", err)
		}
	}
	offline, err := d.useOfflineBundle(ctx)
	if err != nil {
		return err
	}
	if offline {
		if err := d.ensureOwned(ctx, d.udockerDir); err != nil {
			return fmt.Errorf("prepare udocker ownership: %w", err)
		}
		if err := d.ensureOwned(ctx, d.stateDir); err != nil {
			return fmt.Errorf("prepare udocker state ownership: %w", err)
		}
		if d.prootPath == "" {
			archName, _ := engineArchName(goruntime.GOARCH)
			src, err := assets.ExtractProot(assets.ProotStaticNoFD, filepath.Join(d.udockerDir, "bin", "proot-"+archName))
			if err != nil {
				return fmt.Errorf("extract proot: %w", err)
			}
			_, _ = assets.ExtractProot(assets.ProotStaticNoFD, filepath.Join(d.udockerDir, "bin", "proot-"+archName+"-4_8_0"))
			d.prootPath = src
		}
		d.provisioned = true
		return nil
	}

	// Online installation is a deliberately explicit compatibility escape
	// hatch. Missing/corrupt offline assets must not silently change provenance.
	if !d.allowOnlineInstall {
		return fmt.Errorf("udocker offline bundle is missing or unusable at %s (online install disabled)", d.bundleDir)
	}
	d.resolveInstalledExecutable()
	if d.udockerExe == "" {
		if err := d.pipInstallUdocker(ctx); err != nil {
			return err
		}
	}
	if _, err := d.udo(ctx, "--version"); err != nil {
		return fmt.Errorf("validate online udocker: %w", err)
	}
	// `udocker install` downloads englib (proot etc.) only in the explicitly
	// enabled compatibility path.
	if _, err := d.udo(ctx, "install"); err != nil {
		return fmt.Errorf("udocker install: %w", err)
	}

	// Swap in the validated static-nofd proot at both expected names.
	binDir := filepath.Join(d.udockerDir, "bin")
	archName, _ := engineArchName(goruntime.GOARCH)
	src, err := assets.ExtractProot(assets.ProotStaticNoFD, filepath.Join(binDir, "proot-"+archName))
	if err != nil {
		return fmt.Errorf("extract proot: %w", err)
	}
	// udocker also probes a versioned name.
	_, _ = assets.ExtractProot(assets.ProotStaticNoFD, filepath.Join(binDir, "proot-"+archName+"-4_8_0"))
	if err := d.ensureOwned(ctx, d.udockerDir); err != nil {
		return fmt.Errorf("prepare udocker ownership: %w", err)
	}
	d.prootPath = src
	d.provisioned = true
	return nil
}

// pipInstallUdocker installs udocker, tolerating pip variants across ages.
func (d *Driver) pipInstallUdocker(ctx context.Context) error {
	attempts := [][]string{
		{"-m", "pip", "install", "--break-system-packages", "-q", "udocker"},
		{"-m", "pip", "install", "--user", "-q", "udocker"},
		{"-m", "pip", "install", "-q", "udocker"},
	}
	var lastOut string
	for _, args := range attempts {
		slog.Info("runtime command", "runtime", "udocker", "args", append([]string{d.pyBin}, args...))
		out, err := exec.CommandContext(ctx, d.pyBin, args...).CombinedOutput()
		if err == nil {
			if d.resolveInstalledExecutable() {
				return nil
			}
			lastOut = "pip succeeded but the udocker console entry point is not on PATH"
			continue
		}
		lastOut = string(out)
	}
	return fmt.Errorf("pip install udocker failed: %s", truncate(lastOut, 400))
}

// useOfflineBundle wires a pre-staged udocker bundle without touching the network.
func (d *Driver) useOfflineBundle(ctx context.Context) (bool, error) {
	bundle := strings.TrimSpace(d.bundleDir)
	if bundle == "" {
		return false, nil
	}
	st, err := os.Stat(bundle)
	if err != nil || !st.IsDir() {
		return false, nil //nolint:nilerr // An unavailable optional offline bundle leaves normal provisioning available.
	}
	// The bundle's own proot builds are never copied: bin/proot-* is the
	// patched PRoot extractProot owns, and it may be executing right now (a
	// running workload, or a CLI provisioned lazily next to live runveil
	// containers). Rewriting it in place fails with ETXTBSY and, when it does
	// not, briefly swaps in an unpatched PRoot for anything launched meanwhile.
	skipProot := func(rel string) bool { return strings.HasPrefix(rel, "bin/proot") }
	if srcDir := filepath.Join(bundle, "udocker-dir"); isDir(srcDir) {
		if err := copyTree(srcDir, d.udockerDir, skipProot); err != nil {
			return true, fmt.Errorf("copy udocker offline dir: %w", err)
		}
	}
	// Self-contained v1 bundle: preseed englib so udocker never downloads its
	// execution engine, and use the packaged Python entry script directly.
	if englib := filepath.Join(bundle, "englib"); isDir(englib) {
		if err := copyTree(englib, d.udockerDir, skipProot); err != nil {
			return true, fmt.Errorf("copy udocker offline englib: %w", err)
		}
	}
	switch {
	case isExecutable(filepath.Join(bundle, "bin", "udocker")):
		d.udockerExe = filepath.Join(bundle, "bin", "udocker")
	case isExecutable(filepath.Join(bundle, "udocker")):
		d.udockerExe = filepath.Join(bundle, "udocker")
	case isExecutable(filepath.Join(bundle, "udocker", "udocker")):
		d.udockerScript = filepath.Join(bundle, "udocker", "udocker")
	case isFile(filepath.Join(bundle, "python", "udocker.py")) || isDir(filepath.Join(bundle, "python", "udocker")):
		if script := filepath.Join(bundle, "python", "udocker.py"); isFile(script) {
			d.udockerScript = script
		} else {
			d.pyPath = filepath.Join(bundle, "python")
		}
	case isFile(filepath.Join(bundle, "udocker.py")) || isDir(filepath.Join(bundle, "udocker")):
		if script := filepath.Join(bundle, "udocker.py"); isFile(script) {
			d.udockerScript = script
		} else {
			d.pyPath = bundle
		}
	default:
		return false, nil
	}
	// Use the self-built static-nofd engine. It has been validated against the
	// same vLLM rootfs where the newer socket-free build fails to load
	// even /bin/echo ("cannot stat shared object: Invalid argument"). Keep both
	// standard filenames in sync because udocker selects between them by kernel.
	binDir := filepath.Join(d.udockerDir, "bin")
	archName, _ := engineArchName(goruntime.GOARCH)
	proot, err := assets.ExtractProot(assets.ProotStaticNoFD, filepath.Join(binDir, "proot-"+archName))
	if err != nil {
		return true, fmt.Errorf("extract static-nofd proot: %w", err)
	}
	if _, err := assets.ExtractProot(assets.ProotStaticNoFD, filepath.Join(binDir, "proot-"+archName+"-4_8_0")); err != nil {
		return true, fmt.Errorf("extract versioned static-nofd proot: %w", err)
	}
	d.prootPath = proot
	if err := d.ensureOwned(ctx, d.udockerDir); err != nil {
		return true, fmt.Errorf("prepare udocker ownership: %w", err)
	}
	if out, err := d.udo(ctx, "--version"); err != nil {
		return true, fmt.Errorf("udocker offline bundle unusable: %w: %s", err, truncate(out, 400))
	}
	d.offline = true
	return true, nil
}

// udo runs a udocker CLI command with UDOCKER_DIR set, as the run user.
func (d *Driver) udo(ctx context.Context, args ...string) (string, error) {
	return d.udoWithLevel(ctx, slog.LevelWarn, args...)
}

// udoExpected is udo for commands whose failure the caller handles by design
// (currently only `rm`, see Remove). It keeps the failure out of the warning
// stream so an expected outcome is not reported as an operational problem.
func (d *Driver) udoExpected(ctx context.Context, args ...string) (string, error) {
	return d.udoWithLevel(ctx, slog.LevelDebug, args...)
}

// udoWithLevel runs a udocker CLI command and logs its failure at level.
func (d *Driver) udoWithLevel(ctx context.Context, level slog.Level, args ...string) (string, error) {
	if err := d.ensureCLI(ctx); err != nil {
		return "", err
	}
	if !d.cliConfigured() {
		return "", fmt.Errorf("udocker entry point is not configured")
	}
	full := d.udockerArgs(args...)
	if cliArgsMutate(args) {
		slog.Info("runtime command", "runtime", "udocker", "args", args)
	} else {
		slog.Debug("runtime command", "runtime", "udocker", "args", args)
	}
	var cmd *exec.Cmd
	env := append(os.Environ(),
		"UDOCKER_DIR="+d.udockerDir,
		"UDOCKER_USE_PROOT_EXECUTABLE="+d.prootPath,
	)
	if d.pyPath != "" {
		env = append(env, "PYTHONPATH="+d.pyPath)
	}
	if d.runUser != "" {
		// runuser preserves the environment we pass via -w? Use env + su.
		inner := d.shellInvocation(full)
		cmd = exec.CommandContext(ctx, "runuser", "-u", d.runUser, "--", "bash", "-lc", inner)
	} else {
		cmd = exec.CommandContext(ctx, full[0], full[1:]...)
		cmd.Env = env
	}
	cmd.Dir = processDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		tail := truncateTail(string(out), 500)
		slog.Log(ctx, level, "runtime command failed", "runtime", "udocker", "args", args, "err", err, "stderr_tail", tail)
		return string(out), fmt.Errorf("udocker %v: %w: %s", args, err, tail)
	}
	return string(out), nil
}

// cliArgsMutate reports whether a udocker CLI invocation changes local state
// (containers, images or the installation itself) rather than reading it.
// Mutating commands are the audit trail for "who touched this node", so they
// are logged at INFO even on success; pure reads stay at Debug.
func cliArgsMutate(args []string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		switch arg {
		case "create", "setup", "rm", "run", "install", "pull", "load", "rmi", "tag":
			return true
		default:
			return false
		}
	}
	return false
}

func (d *Driver) resolveInstalledExecutable() bool {
	path, err := exec.LookPath("udocker")
	if err != nil {
		return false
	}
	d.udockerExe = path
	return true
}
