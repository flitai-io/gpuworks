package messages

// Why an SSH step failed, decided once where internal/server/provision calls
// the network and the SSH library (provision.FailureCode). Out-of-band
// recovery records the code beside its phase and English message.
const (
	// ErrorSSHUnreachable: the TCP connection to the SSH endpoint failed
	// (refused, no route, name not resolved).
	ErrorSSHUnreachable = "SSH_UNREACHABLE"
	// ErrorSSHTimeout: connecting or the SSH handshake ran out of time.
	ErrorSSHTimeout = "SSH_TIMEOUT"
	// ErrorSSHClosedBeforeIdent: the endpoint accepted the connection and
	// closed it before identifying itself as an SSH server (sshd past
	// MaxStartups, or not an SSH port).
	ErrorSSHClosedBeforeIdent = "SSH_CLOSED_BEFORE_IDENT"
	// ErrorSSHHandshakeFailed: the SSH handshake failed for another reason.
	ErrorSSHHandshakeFailed = "SSH_HANDSHAKE_FAILED"
	// ErrorSSHHostKeyMismatch: the endpoint's host key differs from the one
	// recorded for it.
	ErrorSSHHostKeyMismatch = "SSH_HOST_KEY_MISMATCH"
	// ErrorSSHAuthFailed: the target rejected every credential offered.
	ErrorSSHAuthFailed = "SSH_AUTH_FAILED"
	// ErrorSSHCredentialInvalid: the stored credentials cannot be used at
	// all (none configured, or a key that does not parse).
	ErrorSSHCredentialInvalid = "SSH_CREDENTIAL_INVALID"
	// ErrorSSHSessionFailed: logged in, but the target refused a session.
	ErrorSSHSessionFailed = "SSH_SESSION_FAILED"
	// ErrorSSHCommandFailed: a command run on the target exited non-zero.
	ErrorSSHCommandFailed = "SSH_COMMAND_FAILED"
	// ErrorSSHConnectionLost: the SSH connection dropped mid-step.
	ErrorSSHConnectionLost = "SSH_CONNECTION_LOST"
)

// Why installing or restarting the agent over SSH failed
// (internal/server/provision).
const (
	// ErrorAgentInstallBinaryUnavailable: the server could not read the agent
	// binary for the target's platform.
	ErrorAgentInstallBinaryUnavailable = "AGENT_INSTALL_BINARY_UNAVAILABLE"
	// ErrorAgentInstallUploadStalled: uploading the agent did not finish in
	// time.
	ErrorAgentInstallUploadStalled = "AGENT_INSTALL_UPLOAD_STALLED"
	// ErrorAgentInstallForeignServer: the host's agent service was installed
	// for another server; restarting it would not bring this node back.
	ErrorAgentInstallForeignServer = "AGENT_INSTALL_FOREIGN_SERVER"
	// ErrorAgentInstallBinaryMissing: the agent binary is not on the host, or
	// the service manager could not execute it (systemd 203/EXEC).
	ErrorAgentInstallBinaryMissing = "AGENT_INSTALL_BINARY_MISSING"
	// ErrorAgentInstallDirNotWritable: the chosen install directory cannot
	// be written.
	ErrorAgentInstallDirNotWritable = "AGENT_INSTALL_DIR_NOT_WRITABLE"
	// ErrorAgentInstallStartFailed: the install script could not start the
	// agent.
	ErrorAgentInstallStartFailed = "AGENT_INSTALL_START_FAILED"
)

// Out-of-band recovery outcomes (internal/server/recovery) that are not an
// SSH or install failure.
const (
	// ErrorRecoveryAgentNotConnected: the agent runs on the host but has no
	// control session with this server.
	ErrorRecoveryAgentNotConnected = "RECOVERY_AGENT_NOT_CONNECTED"
	// ErrorRecoveryNoReconnect: restart and reinstall finished, and the agent
	// still did not reconnect in time.
	ErrorRecoveryNoReconnect = "RECOVERY_NO_RECONNECT"
	// ErrorRecoveryProviderInstanceGone: the node's cloud instance was
	// released, so recovery stopped.
	ErrorRecoveryProviderInstanceGone = "RECOVERY_PROVIDER_INSTANCE_GONE"
	// ErrorRecoveryStepFailed: a recovery step failed for a reason no code
	// names; the message is the evidence.
	ErrorRecoveryStepFailed = "RECOVERY_STEP_FAILED"
)

// Why an out-of-band recovery round did nothing (recovery.Outcome.SkippedCode).
const (
	RecoverySkippedDisabled          = "RECOVERY_SKIPPED_DISABLED"
	RecoverySkippedInProgress        = "RECOVERY_SKIPPED_IN_PROGRESS"
	RecoverySkippedOnline            = "RECOVERY_SKIPPED_ONLINE"
	RecoverySkippedNeverConnected    = "RECOVERY_SKIPPED_NEVER_CONNECTED"
	RecoverySkippedStopped           = "RECOVERY_SKIPPED_STOPPED"
	RecoverySkippedNoSSHCredential   = "RECOVERY_SKIPPED_NO_SSH_CREDENTIAL"
	RecoverySkippedGracePeriod       = "RECOVERY_SKIPPED_GRACE_PERIOD"
	RecoverySkippedBackoff           = "RECOVERY_SKIPPED_BACKOFF"
	RecoverySkippedServerDialBackoff = "RECOVERY_SKIPPED_SERVER_DIAL_BACKOFF"
	// RecoverySkippedRedial: the provider moved the agent's address and the
	// server dials it there instead of logging in.
	RecoverySkippedRedial = "RECOVERY_SKIPPED_REDIAL"
)
