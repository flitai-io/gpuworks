package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/expose"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

const exposeUsage = `  gpuworks expose up [--provider cloudflare-quick] [--systemd] [--origin URL]
  gpuworks expose status [--url-only]
  gpuworks expose down
  gpuworks expose rm
  gpuworks expose logs [-f]

  --data DIR   the data directory to use, when it cannot be discovered

The connector runs in a service of its own (` + "`gpuworks expose run`" + `), apart from the
server: restarting or upgrading the server keeps the address. ` + "`up`" + ` starts it as a
background process, which is not started again after a reboot; with --systemd it
is installed as ` + "`gpuworks-expose.service`" + ` instead. It forwards to the address the
server recorded when it last started, or --origin.
The data directory comes from --data, or server.yaml next to this binary or in the
default installation; without an installation it falls back to ./data.`

// exposeWait is how long the CLI waits for the server to publish the entry
// point before it stops waiting. Stopping the wait does not undo the request:
// the connector stays under the server's management.
const exposeWait = 3 * time.Minute

// serviceStartGrace is how long `up` gives a connector service it just started
// before saying it is not running.
var serviceStartGrace = 15 * time.Second

// errNotServing reports a request that is valid but has no address yet.
var errNotServing = errors.New("no public address yet")

// quickNote is repeated wherever an operator might copy a quick address into
// something durable.
const quickNote = "temporary address: it changes when the connector restarts, streaming (SSE) is not supported — for nodes use the server's own stable address"

// exposeCommand implements `gpuworks expose`: one public entry point per data
// dir, published by the running server through a provider the operator picks.
func exposeCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, exposeUsage+"\n")
		return errors.New("expose: a subcommand is required")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "help", "-h", "--help":
		fmt.Fprint(os.Stderr, "usage:\n"+exposeUsage+"\n")
		return nil
	case "connector-host":
		// The hidden helper the server spawns to own the connector process. It
		// is not part of the operator-facing surface (see expose.HostMain).
		os.Exit(expose.HostMain(ctx, os.Stdin))
		return nil
	case "run":
		// The connector service itself, started by `up` or by its systemd
		// unit (see expose.RunService).
		return exposeRun(ctx, rest)
	case "up":
		return exposeUp(ctx, rest)
	case "status":
		return exposeStatus(rest)
	case "down":
		return exposeDown(ctx, rest)
	case "rm":
		return exposeRemove(rest)
	case "logs":
		return exposeLogs(ctx, rest)
	default:
		fmt.Fprint(os.Stderr, exposeUsage+"\n")
		return fmt.Errorf("expose: unknown subcommand %q", sub)
	}
}

// exposeStore resolves the data dir the way the plan specifies and reports the
// rule that matched, so an operator never has to pass --data twice. quiet is
// for --url-only, whose stdout carries exactly one thing: the address.
func exposeStore(explicit string, io *selfinstall.IO, quiet bool) *expose.Store {
	dir, how := expose.ResolveDataDir(explicit)
	if !quiet {
		io.Say("data directory: %s (%s)", dir, how)
	}
	return expose.NewStore(dir)
}

func exposeUp(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("expose up", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dataDir := fs.String("data", "", "data directory (default: discovered)")
	providerFlag := fs.String("provider", "", "entry point provider: cloudflare-quick")
	systemd := fs.Bool("systemd", false, "run the connector service as the systemd unit "+expose.SystemdUnitName+" (started again after a reboot)")
	originFlag := fs.String("origin", "", "the server address to forward to (default: what the server recorded)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	io := selfinstall.NewIO()
	store := exposeStore(*dataDir, io, false)
	origin, how, err := expose.ResolveOrigin(store, *originFlag)
	if err != nil {
		return err
	}
	io.Say("forwarding to:  %s (%s)", origin, how)

	// Everything interactive happens outside the file transaction: the lock is
	// for the write, not for the operator's thinking time.
	existing, _ := store.ReadConfig()
	provider, err := chooseProvider(io, *providerFlag, existing)
	if err != nil {
		return err
	}
	credential, err := chooseCredential(io, store, provider, "", existing)
	if err != nil {
		return err
	}
	hostname := ""
	if existing != nil && existing.Provider == provider {
		hostname = existing.Hostname
	}

	release, err := store.Lock(10 * time.Second)
	if err != nil {
		return err
	}
	// Re-read under the transaction: the operator's answers were given against
	// a configuration another process may have changed meanwhile.
	current, _ := store.ReadConfig()
	status, _ := store.ReadStatus()
	cfg := &expose.Config{Provider: provider, Desired: expose.DesiredEnabled, TokenFile: credential, Hostname: hostname, Origin: origin}
	unchanged := current != nil && current.Provider == cfg.Provider && current.Desired == expose.DesiredEnabled &&
		current.TokenFile == cfg.TokenFile && current.Hostname == cfg.Hostname && current.Origin == cfg.Origin
	if unchanged && entryPointInHand(status, current) {
		// Nothing changed: reuse the running entry point instead of asking the
		// server to rebuild it — for a quick tunnel a rebuild mints a new
		// address for no reason.
		cfg.Revision = current.Revision
		io.Say("the configuration is unchanged; reusing the current entry point")
	} else {
		// A new revision is what a request is: the supervisor treats one as a
		// fresh start, which is how `up` recovers an entry point that gave up
		// after repeated failures. Reusing a revision would leave the operator
		// with a command that does nothing.
		cfg.Revision = expose.NewRevision()
	}
	writeErr := store.WriteConfig(cfg)
	release()
	if writeErr != nil {
		return writeErr
	}
	if err := ensureService(ctx, io, store, *dataDir, *systemd); err != nil {
		return err
	}
	return waitForReady(ctx, store, io, cfg.Revision)
}

// entryPointInHand reports whether the observation describes an entry point
// this server still has: the same revision, and a state that means the server
// is working on it. A failed or given-up entry point is in nobody's hands, and
// the honest answer there is a new request rather than a silent reuse.
func entryPointInHand(status *expose.Status, cfg *expose.Config) bool {
	if status == nil || cfg == nil || status.Revision != cfg.Revision {
		return false
	}
	switch status.State {
	case expose.StateReady, expose.StateDegraded, expose.StateStarting, expose.StateAwaitingRoute:
		return true
	default:
		return false
	}
}

// chooseProvider decides which provider this run uses: the flag, the saved
// choice, or an interactive pick that defaults to the free one.
func chooseProvider(io *selfinstall.IO, flagValue string, existing *expose.Config) (string, error) {
	if flagValue != "" {
		if flagValue == expose.ProviderNamed {
			// Implemented, and deliberately not offered yet: publishing a named
			// entry point needs a tunnel and a hostname the operator has already
			// set up in their own Cloudflare account, which is more than a
			// command can assume. It comes back when that setup is a single
			// documented step rather than a dashboard exercise.
			return "", fmt.Errorf("%s is not available yet; use %s (a temporary address, no setup)", expose.ProviderNamed, expose.ProviderQuick)
		}
		if flagValue != expose.ProviderQuick {
			return "", fmt.Errorf("unknown provider %q: use %s", flagValue, expose.ProviderQuick)
		}
		return flagValue, nil
	}
	if existing != nil && existing.Provider != "" {
		return existing.Provider, nil
	}
	// There is one provider to choose from, so there is nothing to ask. The
	// question comes back with the second provider, and only then in a form
	// that names what each one needs.
	return expose.ProviderQuick, nil
}

// chooseCredential returns the token file for providers that need one. The
// token is pasted into a hidden prompt or read from a file the operator names;
// it is never an argument.
func chooseCredential(io *selfinstall.IO, store *expose.Store, provider, tokenFileFlag string, existing *expose.Config) (string, error) {
	if provider != expose.ProviderNamed {
		return "", nil
	}
	if tokenFileFlag != "" {
		return tokenFileFlag, nil
	}
	if existing != nil && existing.Provider == provider && existing.TokenFile != "" {
		if _, err := os.Stat(existing.TokenFile); err == nil {
			return existing.TokenFile, nil
		}
	}
	if !io.Terminal() {
		return "", errors.New("cloudflare-named needs the tunnel token: pass --token-file PATH in an unattended run")
	}
	token, err := io.PromptPassword("Paste the Cloudflare Tunnel token (input hidden): ")
	if err != nil {
		return "", err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("no token given")
	}
	path, err := store.WriteToken(token)
	if err != nil {
		return "", fmt.Errorf("cannot store the token: %w", err)
	}
	io.Say("token stored in %s (mode 0600)", path)
	return path, nil
}

// waitForReady follows the server's observations until the entry point serves
// this server, the provider says a route is missing, or the wait runs out.
func waitForReady(ctx context.Context, store *expose.Store, io *selfinstall.IO, revision string) error {
	deadline := time.Now().Add(exposeWait)
	lastState := ""
	lastError := ""
	warnedNoServer := false
	started := time.Now()
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			io.Warn("stopped waiting; the connector service keeps working on it — `gpuworks expose status` reports it, `gpuworks expose down` stops it")
			return ctx.Err()
		}
		if !store.SupervisorHeld() && !warnedNoServer && time.Since(started) >= serviceStartGrace {
			warnedNoServer = true
			io.Warn("the connector service is not running; its log is %s", store.DaemonLogPath())
		}
		st, _ := store.ReadStatus()
		if st == nil || st.Revision != revision {
			time.Sleep(time.Second)
			continue
		}
		if !store.SupervisorHeld() {
			// A snapshot is not an address while nobody is hosting the entry
			// point it describes. Keep waiting for the service to pick the
			// request up, and never report this as its result.
			time.Sleep(time.Second)
			continue
		}
		if st.State != lastState {
			switch st.State {
			case expose.StateStarting:
				io.Say("connecting…")
			case expose.StateFailed:
				if st.LastError != "" {
					io.Warn("attempt failed: %s", st.LastError)
					lastError = st.LastError
				}
				io.Say("it will be retried automatically (attempt %d)", st.Failures+1)
			}
			lastState = st.State
		}
		switch st.State {
		case expose.StateReady, expose.StateAwaitingRoute:
			if len(st.Candidates) > 1 {
				// More than one route publishes this server; the operator picks
				// one and only that choice is remembered. The choice is a new
				// revision, so the wait has to follow that one — watching the
				// old one would report the request the answer just replaced.
				chosen, handled, err := chooseRoute(io, store, st)
				if handled {
					if err != nil {
						return err
					}
					revision = chosen
					lastState = ""
					break
				}
			}
			if st.State == expose.StateReady {
				printAddress(io, st)
				return nil
			}
			io.Say("Cloudflare is connected, but this server is not published yet.")
			if st.Message != "" {
				io.Say("%s", st.Message)
			}
			io.Warn("no public address yet: the connector stays connected and picks the route up once it exists")
			return errNotServing
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	if lastError != "" {
		io.Warn("giving up on waiting: %s", lastError)
	}
	io.Warn("still not serving after %s; the request is still active — the connector service keeps trying", exposeWait)
	return errNotServing
}

// chooseRoute asks which of several published routes to use, and commits the
// answer. It returns the revision the choice was written as, so the caller can
// wait for that request instead of the one it replaced.
func chooseRoute(io *selfinstall.IO, store *expose.Store, st *expose.Status) (string, bool, error) {
	if !io.Terminal() {
		io.Warn("several published routes point at this server: %s", strings.Join(st.Candidates, ", "))
		io.Warn("choose one by running `gpuworks expose up` from a terminal; nothing is picked automatically")
		return "", true, errNotServing
	}
	io.Say("Several published routes point at this server:")
	for i, host := range st.Candidates {
		io.Say("  %d) https://%s", i+1, host)
	}
	answer, err := io.Prompt("which one? [1]: ")
	if err != nil {
		return "", true, err
	}
	index := 1
	if trimmed := strings.TrimSpace(answer); trimmed != "" {
		if _, scanErr := fmt.Sscanf(trimmed, "%d", &index); scanErr != nil || index < 1 || index > len(st.Candidates) {
			return "", true, fmt.Errorf("please answer a number between 1 and %d", len(st.Candidates))
		}
	}
	cfg, err := store.ReadConfig()
	if err != nil {
		return "", true, err
	}
	cfg.Hostname = st.Candidates[index-1]
	cfg.Revision = expose.NewRevision()
	if err := store.WriteConfig(cfg); err != nil {
		return "", true, err
	}
	io.Say("using https://%s", cfg.Hostname)
	return cfg.Revision, false, nil
}

// printAddress is the operator-facing result: the address, and what it can and
// cannot do.
func printAddress(io *selfinstall.IO, st *expose.Status) {
	io.Say("")
	printAddressBanner(io, st.URL, st.Message)
	io.Say("")
	io.Say("provider:       %s", st.Provider)
	if st.LocalOrigin != "" {
		io.Say("serving:        %s", st.LocalOrigin)
	}
	if st.BinaryVersion != "" {
		io.Say("connector:      cloudflared %s", st.BinaryVersion)
	}
	io.Say("")
	io.Say("The address is public: anyone who has it reaches the login page. The REST API")
	io.Say("requires a session, and /v1 requires an API key unless the gateway was opened.")
	io.Say("The connector runs in its own service: restarting or upgrading the server keeps")
	io.Say("this address. `gpuworks expose down` closes the entry point.")
}

func exposeStatus(args []string) error {
	fs := flag.NewFlagSet("expose status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dataDir := fs.String("data", "", "data directory (default: discovered)")
	urlOnly := fs.Bool("url-only", false, "print only the public address (exit 1 when there is none)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	io := selfinstall.NewIO()
	store := exposeStore(*dataDir, io, *urlOnly)
	st, cfg := liveStatus(store)

	if *urlOnly {
		// The address is what a script will dial, so it is only printed when
		// the entry point is ready: a degraded or waiting one has an address
		// that does not serve this server yet (or at all), and exit 0 would
		// claim otherwise.
		if st == nil || st.State != expose.StateReady || st.URL == "" {
			io.Warn("no public address is being served right now")
			if st != nil {
				io.Say("state: %s", st.State)
				if st.URL != "" {
					io.Warn("the address %s is not ready yet", st.URL)
				}
				if st.Message != "" {
					io.Warn("%s", st.Message)
				}
			}
			return errNotServing
		}
		fmt.Println(st.URL)
		// The limits belong on stderr even here: a script reading the address
		// still deserves to know what it is reading.
		if st.Provider == expose.ProviderQuick {
			io.Warn("note: %s", quickNote)
		}
		return nil
	}
	if cfg == nil {
		io.Say("no entry point has been configured; run `gpuworks expose up`")
		return nil
	}
	io.Say("provider:  %s", cfg.Provider)
	io.Say("desired:   %s", cfg.Desired)
	io.Say("service:   %s", serviceDescription(store))
	if st == nil {
		io.Say("state:     unknown (the server has not reported yet)")
		return nil
	}
	io.Say("state:     %s", st.State)
	if st.URL != "" && st.State == expose.StateReady {
		printAddressBanner(io, st.URL, st.Message)
	} else {
		io.Say("address:   %s", orDash(st.URL))
	}
	if st.LocalOrigin != "" {
		io.Say("serving:   %s", st.LocalOrigin)
	}
	if st.BinaryVersion != "" {
		io.Say("connector: cloudflared %s", st.BinaryVersion)
	}
	// What the provider says about the address travels with the address itself
	// when there is one; without one it is the message that matters.
	bannerShowsMessage := st.URL != "" && st.State == expose.StateReady && st.Message != ""
	if !bannerShowsMessage {
		if st.Provider == expose.ProviderQuick {
			io.Warn("note: %s", quickNote)
		} else if st.Message != "" {
			io.Warn("note: %s", st.Message)
		}
		if st.Message != "" {
			io.Say("message:   %s", st.Message)
		}
	}
	if st.LastError != "" {
		io.Warn("last error: %s", st.LastError)
	}
	io.Say("log:       %s", store.LogPath())
	if !systemdServiceInstalled() {
		io.Say("service log: %s", store.DaemonLogPath())
	}
	return nil
}

// liveStatus returns the observation only when it describes something that is
// being served right now: a server must be hosting, and the revision must be
// the one the configuration asks for. Anything else is a snapshot of the past.
func liveStatus(store *expose.Store) (*expose.Status, *expose.Config) {
	cfg, _ := store.ReadConfig()
	st, _ := store.ReadStatus()
	if st == nil {
		return nil, cfg
	}
	if cfg == nil || st.Revision != cfg.Revision {
		return nil, cfg
	}
	if !store.SupervisorHeld() {
		// A lock file alone proves nothing, but a lock nobody holds proves the
		// connector service is gone — and the entry point went with it.
		st.URL = ""
		st.State = expose.StateDisabled
		st.Message = "the connector service is not running; `gpuworks expose up` starts it"
		return st, cfg
	}
	return st, cfg
}

func exposeLogs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("expose logs", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dataDir := fs.String("data", "", "data directory (default: discovered)")
	follow := fs.Bool("f", false, "follow the log")
	if err := fs.Parse(args); err != nil {
		return err
	}
	io := selfinstall.NewIO()
	store := exposeStore(*dataDir, io, false)
	return tailFile(ctx, store.LogPath(), 200, *follow, os.Stdout)
}

// tailFile writes the last lines of a file and optionally follows it. A
// rotation or truncation restarts from the beginning instead of seeking past
// the end.
func tailFile(ctx context.Context, path string, lines int, follow bool, w io.Writer) error {
	offset, err := writeTail(w, path, lines)
	if err != nil {
		return err
	}
	if !follow {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(300 * time.Millisecond):
		}
		offset, err = copyNew(w, path, offset)
		if err != nil {
			return err
		}
	}
}

func writeTail(w io.Writer, path string, n int) (int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if n > 0 {
		split := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
		if len(split) > n {
			split = split[len(split)-n:]
		}
		raw = []byte(strings.Join(split, "\n") + "\n")
	}
	if _, err := w.Write(raw); err != nil {
		return 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // Rotation can remove the path before the next follow pass.
		}
		return 0, err
	}
	return info.Size(), nil
}

func copyNew(w io.Writer, path string, offset int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return offset, nil
		}
		return offset, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return offset, err
	}
	if info.Size() < offset {
		offset = 0
	}
	if _, err := f.Seek(offset, 0); err != nil {
		return offset, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return offset, err
	}
	if len(data) > 0 {
		if _, err := w.Write(data); err != nil {
			return offset, err
		}
	}
	return info.Size(), nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
