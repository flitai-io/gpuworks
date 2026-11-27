package selfinstall

import (
	"fmt"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/launchd"
)

// UnitConfig is everything the rendered systemd unit needs. Every setting of
// the server itself lives in server.yaml; the unit only says where it is.
type UnitConfig struct {
	Binary       string
	ConfigPath   string
	DataDir      string // for RequiresMountsFor
	ServiceUser  string
	ServiceGroup string
	// SupplementaryGroups are extra groups the service runs with, which
	// install hooks ask for (ServiceGroups); systemd only.
	SupplementaryGroups []string
	StderrLog           string // launchd's StandardErrorPath; systemd has the journal
}

// RenderUnit produces the systemd unit for an installed control plane.
//
// Notes on the settings that are deliberate rather than copied from the agent
// unit:
//
//   - No setting and no password appears here: the server reads server.yaml,
//     and the unit file is world-readable, so the bootstrap password travels
//     through <data>/initial-credentials instead.
//   - KillMode is left at the default (control-group), unlike the agent, which
//     must not kill the inference containers it started. The server owns no
//     containers, and its children must go away with it.
//   - TimeoutStopSec gives graceful shutdown room to finish in-flight model and
//     image syncs before systemd escalates to SIGKILL.
//   - Delegate=yes hands the unit's cgroup to the server, which moves itself
//     into a leaf of it and gives each plugin a cgroup with memory, CPU and
//     process limits. An installation upgraded from a unit without it keeps
//     its unit (upgrade rewrites the unit only to move onto server.yaml), so
//     its plugins share the server's cgroup and the RSS watchdog bounds
//     their memory until the next install.
func RenderUnit(cfg UnitConfig) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=gpuworks control plane\n")
	b.WriteString("Documentation=https://flitai.io/docs/gpuworks\n")
	b.WriteString("After=network-online.target\n")
	fmt.Fprintf(&b, "RequiresMountsFor=%s\n", systemdQuote(cfg.DataDir))
	b.WriteString("\n[Service]\n")
	b.WriteString("Type=simple\n")
	if user := strings.TrimSpace(cfg.ServiceUser); user != "" {
		fmt.Fprintf(&b, "User=%s\n", user)
	}
	if group := strings.TrimSpace(cfg.ServiceGroup); group != "" {
		fmt.Fprintf(&b, "Group=%s\n", group)
	}
	if len(cfg.SupplementaryGroups) > 0 {
		fmt.Fprintf(&b, "SupplementaryGroups=%s\n", systemdArgs(cfg.SupplementaryGroups))
	}
	fmt.Fprintf(&b, "ExecStart=%s\n", systemdArgs(append([]string{cfg.Binary}, serveArgs(cfg)...)))
	fmt.Fprintf(&b, "ExecStartPre=/usr/bin/test -x %s\n", systemdQuote(cfg.Binary))
	b.WriteString("Restart=always\n")
	b.WriteString("RestartSec=3\n")
	b.WriteString("TimeoutStopSec=30\n")
	b.WriteString("Delegate=yes\n")
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")
	return b.String()
}

// LaunchdEnv marks a server launchd runs. Its plist is KeepAlive, so the
// server may restart by exiting, as under systemd (INVOCATION_ID).
const LaunchdEnv = "GPUWORKS_LAUNCHD"

// RenderPlist produces the launch daemon plist for an installed control plane
// on macOS: the unit's counterpart, with the same command line. It runs as
// the account that installed it (UserName), from boot, and launchd starts it
// again 3 s after it exits (KeepAlive, ThrottleInterval) like Restart=always
// with RestartSec=3. Its children stop with it, and ExitTimeOut gives graceful
// shutdown the unit's 30 s. The plist holds no secret: it is world-readable.
func RenderPlist(cfg UnitConfig) string {
	return string(launchd.Spec{
		Label:       launchd.ServerLabel,
		User:        strings.TrimSpace(cfg.ServiceUser),
		Args:        append([]string{cfg.Binary}, serveArgs(cfg)...),
		Env:         map[string]string{LaunchdEnv: "1"},
		WorkingDir:  filepath.Dir(cfg.Binary),
		StderrPath:  cfg.StderrLog,
		ExitTimeout: 30,
	}.Plist())
}

// systemdQuote renders one argument or assignment value. systemd splits on
// whitespace unless a value is quoted, expands %-specifiers, and treats the
// first "=" of an Environment line as the separator, so all three need care.
func systemdQuote(value string) string {
	if value == "" {
		return `""`
	}
	if !strings.ContainsAny(value, " \t\"'\\%") {
		return value
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '%':
			// "%%" is a literal percent sign in a unit file.
			b.WriteString("%%")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// SystemdCommandLine is args as an Exec*= line of a unit or drop-in, each
// argument quoted the way the server's own ExecStart is.
func SystemdCommandLine(args []string) string { return systemdArgs(args) }

func systemdArgs(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, systemdQuote(arg))
	}
	return strings.Join(quoted, " ")
}

// serveArgs is the command line of the server in every service mode: only
// the configuration file, so there is one place to read what it runs with.
func serveArgs(cfg UnitConfig) []string {
	return []string{"serve", "--config", cfg.ConfigPath}
}

// supervisedServeArgs is the nohup mode's command line: the supervisor in
// front of serveArgs, which starts the server again after it exits as the
// unit's Restart=always does.
func supervisedServeArgs(cfg UnitConfig) []string {
	return []string{"serve", "--" + SuperviseFlag, "--config", cfg.ConfigPath}
}
