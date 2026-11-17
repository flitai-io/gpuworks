package proto

// RuntimeFallbackDaemonNotReady is the fallback reason for a runtime whose
// CLI and local socket are present but whose daemon did not answer while the
// agent started: dockerd still starting at boot, or too busy (pulling a
// large image) to answer in time. The agent rechecks it every minute and
// switches back once it answers and no instance would be disturbed.
const RuntimeFallbackDaemonNotReady = "daemon_not_ready"

// RuntimeFallbackSocketPermissionDenied is the fallback reason for a runtime
// whose daemon answers but refuses the agent's account its socket: the
// account is not in the socket's group. Unlike a daemon that is not ready it
// does not pass with time, and adding the account to the group does not
// reach the running agent either: a process's groups are fixed when it
// starts, so the agent has to be restarted. RuntimeSocketDenial says what
// the agent found.
const RuntimeFallbackSocketPermissionDenied = "socket_permission_denied"

// Agent supervisors (RuntimeSocketDenial.Supervisor): what restarts the
// agent on this host.
const (
	AgentSupervisorSystemd = "systemd"
	AgentSupervisorNohup   = "nohup"
)

// RuntimeSocketDenial is what the agent found about a container daemon
// socket its account may not open, the facts an operator needs to let it
// in. Fields the agent could not find out are empty (GID: GIDUnknown).
type RuntimeSocketDenial struct {
	// Socket is the socket's path, Group the name of the group that owns it
	// ("" when the gid has no name on the host) and GID that group's id,
	// GIDUnknown when the socket could not be read: 0 is the root group.
	Socket string `json:"socket"`
	Group  string `json:"group,omitempty"`
	GID    int    `json:"gid"`
	// GroupHeld is set when the agent's process already holds the socket's
	// group: membership is not what refuses it (the socket's mode, or a
	// security module such as SELinux or AppArmor), and adding the account
	// to the group would change nothing.
	GroupHeld bool `json:"group_held,omitempty"`
	// Account is the account the agent runs as.
	Account string `json:"account,omitempty"`
	// SameNameGID is the id of a group the account is in that bears the
	// socket group's name but is another group, 0 when it is in none: a
	// directory service (NIS, LDAP) defining its own "docker" group, which
	// does not open a socket owned by the host's local one.
	SameNameGID int `json:"same_name_gid,omitempty"`
	// RestartPending is set when the host's account database already lists
	// the account in the socket's group and only this process, started
	// before that, lacks it: restarting the agent is all that is left.
	RestartPending bool `json:"restart_pending,omitempty"`
	// Supervisor is what restarts the agent (AgentSupervisor*), "" when the
	// agent cannot tell; StateDir is its state directory, where a nohup
	// install keeps agent-restart.sh and gpuworks-agent.pid.
	Supervisor string `json:"supervisor,omitempty"`
	StateDir   string `json:"state_dir,omitempty"`
}

// GIDUnknown is RuntimeSocketDenial.GID when the socket's group could not be
// read.
const GIDUnknown = -1
