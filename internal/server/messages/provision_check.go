package messages

// Node onboarding check outcomes (ADR 0067): the SSH precheck, the SSH
// install and a cloud provider's install report one provision.CheckResult
// per check, whose code is one of these. The params each code names are the
// facts the check observed; anything a remote command or a provider API
// printed travels as the check's evidence, never as a param.
const (
	// SSH login.

	// CheckSSHConnectFailed: the server could not open an SSH session to
	// {target}.
	CheckSSHConnectFailed = "CHECK_SSH_CONNECT_FAILED"
	// CheckSSHUnavailable: the provider exposes no SSH endpoint for the
	// instance; the driver's own install validates access.
	CheckSSHUnavailable = "CHECK_SSH_UNAVAILABLE"
	// CheckInstanceStopped: the instance ({instance}) is stopped.
	CheckInstanceStopped = "CHECK_INSTANCE_STOPPED"
	// CheckSSHLoggedIn: the target accepted {method} after {attempts}
	// attempts.
	CheckSSHLoggedIn = "CHECK_SSH_LOGGED_IN"
	// CheckSSHAuthRejected: the target rejected every credential offered.
	CheckSSHAuthRejected = "CHECK_SSH_AUTH_REJECTED"
	// CheckSSHNoCredential: no credential was offered.
	CheckSSHNoCredential = "CHECK_SSH_NO_CREDENTIAL"

	// The platform SSH key.

	// CheckPlatformKeyUnavailable: the server has no platform key to add.
	CheckPlatformKeyUnavailable = "CHECK_PLATFORM_KEY_UNAVAILABLE"
	// CheckPlatformKeyUsed: the platform key logged in.
	CheckPlatformKeyUsed = "CHECK_PLATFORM_KEY_USED"
	// CheckPlatformKeyPresent: the key is already in {file}.
	CheckPlatformKeyPresent = "CHECK_PLATFORM_KEY_PRESENT"
	// CheckPlatformKeyAdded: the key was added to {file}.
	CheckPlatformKeyAdded = "CHECK_PLATFORM_KEY_ADDED"
	// CheckPlatformKeyPending: the key is not in {file}; the install adds it.
	CheckPlatformKeyPending = "CHECK_PLATFORM_KEY_PENDING"
	// CheckPlatformKeyAddFailed: adding the key failed; the credential that
	// worked is kept.
	CheckPlatformKeyAddFailed = "CHECK_PLATFORM_KEY_ADD_FAILED"
	// CheckPlatformKeyUnreadable: authorized_keys could not be read.
	CheckPlatformKeyUnreadable = "CHECK_PLATFORM_KEY_UNREADABLE"
	// CheckPlatformKeyLoginFailed: the server could not log in to add it.
	CheckPlatformKeyLoginFailed = "CHECK_PLATFORM_KEY_LOGIN_FAILED"
	// CheckPlatformKeyAccountPresent: the key is already among the
	// {provider} account's SSH keys, which reach instances created from now
	// on.
	CheckPlatformKeyAccountPresent = "CHECK_PLATFORM_KEY_ACCOUNT_PRESENT"
	// CheckPlatformKeyAccountAdded: the key was added to the {provider}
	// account's SSH keys.
	CheckPlatformKeyAccountAdded = "CHECK_PLATFORM_KEY_ACCOUNT_ADDED"
	// CheckPlatformKeyInstanceAttached: the {provider} instance was given the
	// key.
	CheckPlatformKeyInstanceAttached = "CHECK_PLATFORM_KEY_INSTANCE_ATTACHED"
	// CheckPlatformKeyProviderFailed: the provider could not authorize it.
	CheckPlatformKeyProviderFailed = "CHECK_PLATFORM_KEY_PROVIDER_FAILED"
	// CheckPlatformKeyPodAdded: the key was added to the pod's PUBLIC_KEY
	// (and container {port} exposed when non-zero); the pod restarted and SSH
	// is now at {ssh}.
	CheckPlatformKeyPodAdded = "CHECK_PLATFORM_KEY_POD_ADDED"
	// CheckPlatformKeyPodRestart: the pod lacks the key; the install adds it
	// after asking, which restarts the pod and clears its container disk.
	CheckPlatformKeyPodRestart = "CHECK_PLATFORM_KEY_POD_RESTART"
	// CheckPlatformKeyAssistantPending: the instance lacks the key; the
	// install adds it through Cloud Assistant.
	CheckPlatformKeyAssistantPending = "CHECK_PLATFORM_KEY_ASSISTANT_PENDING"
	// CheckPlatformKeyAssistantWait: as Pending, but Cloud Assistant is not
	// online yet; the install waits up to {wait} for it.
	CheckPlatformKeyAssistantWait = "CHECK_PLATFORM_KEY_ASSISTANT_WAIT"
	// CheckInstancePasswordFetched: the provider API issued the password.
	CheckInstancePasswordFetched = "CHECK_INSTANCE_PASSWORD_FETCHED"
	// CheckInstancePasswordFailed: the password could not be fetched.
	CheckInstancePasswordFailed = "CHECK_INSTANCE_PASSWORD_FAILED"

	// Commands the precheck runs on the node.

	// CheckCommandNotRun: the check's command did not complete (session,
	// timeout); nothing is known about what it checks.
	CheckCommandNotRun = "CHECK_COMMAND_NOT_RUN"
	// CheckCommandFailed: the command reported a problem; its output says
	// which.
	CheckCommandFailed = "CHECK_COMMAND_FAILED"
	// CheckNoRootOrSudo: the SSH user is not root and has no passwordless
	// sudo.
	CheckNoRootOrSudo = "CHECK_NO_ROOT_OR_SUDO"
	// CheckPythonMissing: no python3 (only the udocker launcher needs it).
	CheckPythonMissing = "CHECK_PYTHON_MISSING"
	// CheckGPUToolMissing: no supported GPU tool answered on the host.
	CheckGPUToolMissing = "CHECK_GPU_TOOL_MISSING"
	// CheckDeviceAccessDenied: the SSH user cannot open every accelerator
	// device node; the evidence names the groups and the fix.
	CheckDeviceAccessDenied = "CHECK_DEVICE_ACCESS_DENIED"
	// CheckDockerMissing: docker is not installed.
	CheckDockerMissing = "CHECK_DOCKER_MISSING"
	// CheckNvidiaToolkitMissing: the NVIDIA Container Toolkit is not
	// installed.
	CheckNvidiaToolkitMissing = "CHECK_NVIDIA_TOOLKIT_MISSING"
	// CheckAgentPortUnavailable: the agent cannot listen on {listen}.
	CheckAgentPortUnavailable = "CHECK_AGENT_PORT_UNAVAILABLE"
	// CheckServerUnreachableFromNode: the node cannot reach {server}.
	CheckServerUnreachableFromNode = "CHECK_SERVER_UNREACHABLE_FROM_NODE"
	// CheckPrecheckScriptFailed: the provider-run precheck script exited
	// abnormally.
	CheckPrecheckScriptFailed = "CHECK_PRECHECK_SCRIPT_FAILED"
	// CheckCloudAssistantOnline: Cloud Assistant is online; the checks ran as
	// root.
	CheckCloudAssistantOnline = "CHECK_CLOUD_ASSISTANT_ONLINE"
	// CheckCloudAssistantOffline: Cloud Assistant is not online.
	CheckCloudAssistantOffline = "CHECK_CLOUD_ASSISTANT_OFFLINE"
	// CheckCloudAssistantRan: the install script ran as root (invocation
	// {invocation}).
	CheckCloudAssistantRan = "CHECK_CLOUD_ASSISTANT_RAN"
	// CheckConnectionNoPrecheck: the driver cannot precheck through the
	// {connection} connection; the install validates access.
	CheckConnectionNoPrecheck = "CHECK_CONNECTION_NO_PRECHECK"
	// CheckConnectionFailed: the {connection} connection failed.
	CheckConnectionFailed = "CHECK_CONNECTION_FAILED"

	// Connectivity between the server and the node.

	// CheckServerReachedNode: the server reached {address}.
	CheckServerReachedNode = "CHECK_SERVER_REACHED_NODE"
	// CheckServerReachedNodeNoListener: the server reached {address};
	// nothing listens there yet.
	CheckServerReachedNodeNoListener = "CHECK_SERVER_REACHED_NODE_NO_LISTENER"
	// CheckServerNodeTimeout: no answer from {address} within {timeout};
	// inbound TCP {port} must be open for the server.
	CheckServerNodeTimeout = "CHECK_SERVER_NODE_TIMEOUT"
	// CheckServerNodeUnreachable: the server cannot reach {address}.
	CheckServerNodeUnreachable = "CHECK_SERVER_NODE_UNREACHABLE"
	// CheckNodeAddressInvalid: there is no valid address to reach the node.
	CheckNodeAddressInvalid = "CHECK_NODE_ADDRESS_INVALID"
	// CheckServerToAgentAtInstall: the install picks the port the provider
	// maps, then checks the server reaches it.
	CheckServerToAgentAtInstall = "CHECK_SERVER_TO_AGENT_AT_INSTALL"
	// CheckConnectivityUseAgentDial: only the node reaches the server.
	CheckConnectivityUseAgentDial = "CHECK_CONNECTIVITY_USE_AGENT_DIAL"
	// CheckConnectivityUseServerDial: only the server reaches the node.
	CheckConnectivityUseServerDial = "CHECK_CONNECTIVITY_USE_SERVER_DIAL"
	// CheckConnectivityNone: neither direction checked works.
	CheckConnectivityNone = "CHECK_CONNECTIVITY_NONE"
	// CheckServerDialPortChosen: a server-dialed agent listens on container
	// port {port}, reached at {address}; {opened} says the install opened it
	// and {restarted} that the instance restarted for it.
	CheckServerDialPortChosen = "CHECK_SERVER_DIAL_PORT_CHOSEN"

	// Alibaba Cloud security groups and the server-dialed agent's port.

	// CheckSecurityGroupOpen: {group} (priority {priority}) accepts TCP
	// {port} from anywhere.
	CheckSecurityGroupOpen = "CHECK_SECURITY_GROUP_OPEN"
	// CheckSecurityGroupOpenAfterSpecific: as Open, after rules for specific
	// sources that may decide first (evidence lists them).
	CheckSecurityGroupOpenAfterSpecific = "CHECK_SECURITY_GROUP_OPEN_AFTER_SPECIFIC"
	// CheckSecurityGroupDrops: {group} (priority {priority}) drops TCP {port}
	// from anywhere.
	CheckSecurityGroupDrops = "CHECK_SECURITY_GROUP_DROPS"
	// CheckSecurityGroupDropsExceptSpecific: as Drops, but rules for specific
	// sources may let the server in (evidence lists them).
	CheckSecurityGroupDropsExceptSpecific = "CHECK_SECURITY_GROUP_DROPS_EXCEPT_SPECIFIC"
	// CheckSecurityGroupSpecificOnly: TCP {port} is accepted only from
	// specific sources (evidence lists them).
	CheckSecurityGroupSpecificOnly = "CHECK_SECURITY_GROUP_SPECIFIC_ONLY"
	// CheckSecurityGroupNoRule: no inbound rule of {groups} accepts TCP
	// {port}.
	CheckSecurityGroupNoRule = "CHECK_SECURITY_GROUP_NO_RULE"
	// CheckSecurityGroupNone: the instance lists no security group.
	CheckSecurityGroupNone = "CHECK_SECURITY_GROUP_NONE"
	// CheckSecurityGroupUnreadable: the security groups could not be read.
	CheckSecurityGroupUnreadable = "CHECK_SECURITY_GROUP_UNREADABLE"

	// The install directory ({dir}).

	// CheckInstallDirWritable: {dir} is writable by {user}.
	CheckInstallDirWritable = "CHECK_INSTALL_DIR_WRITABLE"
	// CheckInstallDirWritableSudo: {dir} is writable through passwordless
	// sudo.
	CheckInstallDirWritableSudo = "CHECK_INSTALL_DIR_WRITABLE_SUDO"
	// CheckInstallDirMacAdmin: the installer creates {dir} with
	// administrator rights.
	CheckInstallDirMacAdmin = "CHECK_INSTALL_DIR_MAC_ADMIN"
	// CheckInstallDirNotDirectory: {dir} exists but is not a directory.
	CheckInstallDirNotDirectory = "CHECK_INSTALL_DIR_NOT_DIRECTORY"
	// CheckInstallDirNoexec: {dir} is on a noexec mount ({mount}).
	CheckInstallDirNoexec = "CHECK_INSTALL_DIR_NOEXEC"
	// CheckInstallDirMacNotWritable: {dir} is not writable by {user}, who
	// has no administrator rights.
	CheckInstallDirMacNotWritable = "CHECK_INSTALL_DIR_MAC_NOT_WRITABLE"
	// CheckInstallDirRootNotWritable: {dir} is not writable even by root.
	CheckInstallDirRootNotWritable = "CHECK_INSTALL_DIR_ROOT_NOT_WRITABLE"
	// CheckInstallDirSudoNotWritable: {dir} is not writable by {user}, even
	// through sudo.
	CheckInstallDirSudoNotWritable = "CHECK_INSTALL_DIR_SUDO_NOT_WRITABLE"
	// CheckInstallDirNotWritable: {dir} is not writable by {user}, who has
	// no passwordless sudo.
	CheckInstallDirNotWritable = "CHECK_INSTALL_DIR_NOT_WRITABLE"
	// CheckInstallDirNotAbsolute: {dir} is not an absolute path.
	CheckInstallDirNotAbsolute = "CHECK_INSTALL_DIR_NOT_ABSOLUTE"
	// CheckInstallDirProbeFailed: {dir} could not be inspected.
	CheckInstallDirProbeFailed = "CHECK_INSTALL_DIR_PROBE_FAILED"
	// CheckNotNeededOnMac: engines on a Mac run as the agent's own user.
	CheckNotNeededOnMac = "CHECK_NOT_NEEDED_ON_MAC"
	// CheckInstallDirChownAllowed: files under the directory can be handed to
	// the dedicated runtime user.
	CheckInstallDirChownAllowed = "CHECK_INSTALL_DIR_CHOWN_ALLOWED"
	// CheckInstallDirChownRefused: {dir} refuses ownership changes, so
	// runveil workloads run as the agent's own user.
	CheckInstallDirChownRefused = "CHECK_INSTALL_DIR_CHOWN_REFUSED"
	// CheckInstallDirChownUnprivileged: {user} is not root and has no
	// passwordless sudo, so the agent runs as that user anyway.
	CheckInstallDirChownUnprivileged = "CHECK_INSTALL_DIR_CHOWN_UNPRIVILEGED"
	// CheckInstallDirChownUntested: no file could be created in {dir} with
	// privileges.
	CheckInstallDirChownUntested = "CHECK_INSTALL_DIR_CHOWN_UNTESTED"
	// CheckInstallDirPathOpen: every directory above {dir} can be searched
	// by the dedicated runtime user.
	CheckInstallDirPathOpen = "CHECK_INSTALL_DIR_PATH_OPEN"
	// CheckInstallDirPathClosed: {ancestor} (mode {mode}, owner
	// {owner}:{group}) cannot be searched by the dedicated runtime user, so
	// workloads cannot reach {dir}; {fix} opens it.
	CheckInstallDirPathClosed = "CHECK_INSTALL_DIR_PATH_CLOSED"
	// CheckInstallDirPathClosedUncertain: as Closed, but the install goes on
	// because {reason} (a key of installDirUncertain).
	CheckInstallDirPathClosedUncertain = "CHECK_INSTALL_DIR_PATH_CLOSED_UNCERTAIN"
	// CheckInstallDirPathClosedReinstall: as Closed, not stopping the
	// reinstall of an agent that already runs from there.
	CheckInstallDirPathClosedReinstall = "CHECK_INSTALL_DIR_PATH_CLOSED_REINSTALL"
	// CheckInstallDirPathUnknown: the permissions above {dir} could not be
	// read.
	CheckInstallDirPathUnknown = "CHECK_INSTALL_DIR_PATH_UNKNOWN"
	// CheckInstallDirPathOwnUser: workloads run as the agent's own user.
	CheckInstallDirPathOwnUser = "CHECK_INSTALL_DIR_PATH_OWN_USER"
	// CheckInstallDirPathUnprivileged: {user} is not root and has no
	// passwordless sudo, so workloads run as that user.
	CheckInstallDirPathUnprivileged = "CHECK_INSTALL_DIR_PATH_UNPRIVILEGED"
	// CheckInstallDirNetworkFilesystem: {dir} is on {filesystem}, a network
	// or userspace filesystem.
	CheckInstallDirNetworkFilesystem = "CHECK_INSTALL_DIR_NETWORK_FILESYSTEM"
	// CheckInstallDirLocalFilesystem: the directory is on {filesystem}.
	CheckInstallDirLocalFilesystem = "CHECK_INSTALL_DIR_LOCAL_FILESYSTEM"
	// CheckInstallDirFilesystemUnknown: the filesystem type is unknown.
	CheckInstallDirFilesystemUnknown = "CHECK_INSTALL_DIR_FILESYSTEM_UNKNOWN"
	// CheckInstallDirSpaceOK: {available} free of {total}.
	CheckInstallDirSpaceOK = "CHECK_INSTALL_DIR_SPACE_OK"
	// CheckInstallDirSpaceLow: {available} free of {total}; images and
	// weights need about {minimum}.
	CheckInstallDirSpaceLow = "CHECK_INSTALL_DIR_SPACE_LOW"
	// CheckInstallDirSpaceUnknown: df reported nothing.
	CheckInstallDirSpaceUnknown = "CHECK_INSTALL_DIR_SPACE_UNKNOWN"
	// CheckInstallDirPersistent: not on a container's writable layer.
	CheckInstallDirPersistent = "CHECK_INSTALL_DIR_PERSISTENT"
	// CheckInstallDirProviderDisk: {dir} is on the {provider} instance's
	// container disk ({filesystem}), lost with the instance.
	CheckInstallDirProviderDisk = "CHECK_INSTALL_DIR_PROVIDER_DISK"
	// CheckInstallDirContainerLayer: {dir} is on the container's writable
	// layer ({filesystem}).
	CheckInstallDirContainerLayer = "CHECK_INSTALL_DIR_CONTAINER_LAYER"
	// CheckInstallDirInMemory: {dir} is on {filesystem}, in memory.
	CheckInstallDirInMemory = "CHECK_INSTALL_DIR_IN_MEMORY"
	// CheckInstallDirColonAllowed: ':' is allowed in file names.
	CheckInstallDirColonAllowed = "CHECK_INSTALL_DIR_COLON_ALLOWED"
	// CheckInstallDirColonRefused: {dir} refuses ':' in file names, so the
	// runveil runtime cannot run deployments from it.
	CheckInstallDirColonRefused = "CHECK_INSTALL_DIR_COLON_REFUSED"
	// CheckInstallDirColonRefusedDocker: as Refused, on a node with docker,
	// which keeps its images elsewhere.
	CheckInstallDirColonRefusedDocker = "CHECK_INSTALL_DIR_COLON_REFUSED_DOCKER"
	// CheckInstallDirNamesUntested: no file could be created in {dir}.
	CheckInstallDirNamesUntested = "CHECK_INSTALL_DIR_NAMES_UNTESTED"

	// An agent or node already on the host.

	// CheckExistingAgentNone: no gpuworks agent is on the host.
	CheckExistingAgentNone = "CHECK_EXISTING_AGENT_NONE"
	// CheckExistingAgentOtherServer: the gpuworks-agent service belongs to
	// {server}; installing replaces it.
	CheckExistingAgentOtherServer = "CHECK_EXISTING_AGENT_OTHER_SERVER"
	// CheckExistingAgentUnknownServer: a gpuworks-agent service whose server
	// cannot be read; installing replaces it.
	CheckExistingAgentUnknownServer = "CHECK_EXISTING_AGENT_UNKNOWN_SERVER"
	// CheckExistingAgentThisServer: the service runs for this server;
	// installing replaces it and its node goes offline.
	CheckExistingAgentThisServer = "CHECK_EXISTING_AGENT_THIS_SERVER"
	// CheckExistingAgentOutside: an agent for this server runs outside the
	// service; installing replaces it.
	CheckExistingAgentOutside = "CHECK_EXISTING_AGENT_OUTSIDE"
	// CheckExistingAgentOthers: agents for {servers} run outside the
	// service; installing leaves them running.
	CheckExistingAgentOthers = "CHECK_EXISTING_AGENT_OTHERS"
	// CheckExistingAgentUnreadable: the host could not be searched for one.
	CheckExistingAgentUnreadable = "CHECK_EXISTING_AGENT_UNREADABLE"
	// CheckExistingAgentReplaced: the install replaced {replaced} on request.
	CheckExistingAgentReplaced = "CHECK_EXISTING_AGENT_REPLACED"
	// CheckExistingNodeNone: no node here uses this SSH host.
	CheckExistingNodeNone = "CHECK_EXISTING_NODE_NONE"
	// CheckExistingNodeFound: node {node} already uses this SSH host
	// ({host}); its agent would be replaced.
	CheckExistingNodeFound = "CHECK_EXISTING_NODE_FOUND"
	// CheckExistingNodeUnknown: the nodes could not be looked up.
	CheckExistingNodeUnknown = "CHECK_EXISTING_NODE_UNKNOWN"

	// Install steps.

	// CheckPrivilegeSudo: a non-root user installs with passwordless sudo.
	CheckPrivilegeSudo = "CHECK_PRIVILEGE_SUDO"
	// CheckPrivilegeDirectFallback: sudo could not claim {dir}, which the
	// user can write; the install goes on without sudo.
	CheckPrivilegeDirectFallback = "CHECK_PRIVILEGE_DIRECT_FALLBACK"
	// CheckPrivilegeDirNotWritable: the chosen {dir} cannot be written.
	CheckPrivilegeDirNotWritable = "CHECK_PRIVILEGE_DIR_NOT_WRITABLE"
	// CheckPrivilegeUserDirFallback: sudo cannot claim {requested}; the
	// install uses {dir} instead.
	CheckPrivilegeUserDirFallback = "CHECK_PRIVILEGE_USER_DIR_FALLBACK"
	// CheckPrivilegeNoFallback: sudo failed and no rootless directory is
	// writable.
	CheckPrivilegeNoFallback = "CHECK_PRIVILEGE_NO_FALLBACK"
	// CheckPrivilegeUploadFallback: the upload to {requested} failed; the
	// install retries in {dir}.
	CheckPrivilegeUploadFallback = "CHECK_PRIVILEGE_UPLOAD_FALLBACK"
	// CheckMacRoot: root on a Mac installs a launch daemon.
	CheckMacRoot = "CHECK_MAC_ROOT"
	// CheckMacSudo: passwordless sudo installs a launch daemon running as
	// the SSH user.
	CheckMacSudo = "CHECK_MAC_SUDO"
	// CheckMacSudoPassword: sudo with the SSH password does the same.
	CheckMacSudoPassword = "CHECK_MAC_SUDO_PASSWORD"
	// CheckMacNoAdmin: no administrator rights: a launch agent of the user's
	// login session.
	CheckMacNoAdmin = "CHECK_MAC_NO_ADMIN"
	// CheckArchDetected: the host reports {uname}, which runs the {arch}
	// agent.
	CheckArchDetected = "CHECK_ARCH_DETECTED"
	// CheckArchUnsupported: no supported architecture could be detected.
	CheckArchUnsupported = "CHECK_ARCH_UNSUPPORTED"
	// CheckArchMacIntel: macOS nodes need Apple silicon; this Mac reports
	// {uname}.
	CheckArchMacIntel = "CHECK_ARCH_MAC_INTEL"
	// CheckAgentBinaryReady: {binary} ({bytes} bytes) is ready to upload.
	CheckAgentBinaryReady = "CHECK_AGENT_BINARY_READY"
	// CheckAgentBinaryMissing: the server has no {binary}.
	CheckAgentBinaryMissing = "CHECK_AGENT_BINARY_MISSING"
	// CheckAgentUploaded: the agent is at {path}.
	CheckAgentUploaded = "CHECK_AGENT_UPLOADED"
	// CheckAgentUploadFailed: the agent could not be put at {path}.
	CheckAgentUploadFailed = "CHECK_AGENT_UPLOAD_FAILED"
	// CheckAgentStarted: the installer started the agent.
	CheckAgentStarted = "CHECK_AGENT_STARTED"
	// CheckAgentStartFailed: the installer failed.
	CheckAgentStartFailed = "CHECK_AGENT_START_FAILED"
	// CheckAgentRunning: an agent runs with state directory {dir}.
	CheckAgentRunning = "CHECK_AGENT_RUNNING"
	// CheckAgentNotObserved: no agent was seen running with {dir}.
	CheckAgentNotObserved = "CHECK_AGENT_NOT_OBSERVED"
	// CheckAgentUnverified: the provider driver did not report whether the
	// agent process runs.
	CheckAgentUnverified = "CHECK_AGENT_UNVERIFIED"
	// CheckProviderDNSSet: the provider's accelerated DNS resolvers are set.
	CheckProviderDNSSet = "CHECK_PROVIDER_DNS_SET"
	// CheckProviderDNSNotSet: they could not be set.
	CheckProviderDNSNotSet = "CHECK_PROVIDER_DNS_NOT_SET"
	// CheckDebugHelperReady: the runveil debug helper is at {path}.
	CheckDebugHelperReady = "CHECK_DEBUG_HELPER_READY"
	// CheckDebugHelperFailed: the helper could not be installed.
	CheckDebugHelperFailed = "CHECK_DEBUG_HELPER_FAILED"
	// CheckUdockerBundleUploaded: {bytes} bytes uploaded to {path}.
	CheckUdockerBundleUploaded = "CHECK_UDOCKER_BUNDLE_UPLOADED"
	// CheckUdockerBundleSourceUnset: udocker_bundle_source is not set; the
	// remote bundle path is passed through.
	CheckUdockerBundleSourceUnset = "CHECK_UDOCKER_BUNDLE_SOURCE_UNSET"
	// CheckUdockerBundleFailed: the bundle could not be installed.
	CheckUdockerBundleFailed = "CHECK_UDOCKER_BUNDLE_FAILED"

	// After the install: the first session.

	// CheckServerDialConnected: the server connected to the agent at
	// {address}.
	CheckServerDialConnected = "CHECK_SERVER_DIAL_CONNECTED"
	// CheckServerDialNotConnected: the server could not connect to
	// {address} yet.
	CheckServerDialNotConnected = "CHECK_SERVER_DIAL_NOT_CONNECTED"
	// CheckServerDialNoAddress: the install named no address to dial.
	CheckServerDialNoAddress = "CHECK_SERVER_DIAL_NO_ADDRESS"
	// CheckServerDialUnconfigured: this server cannot dial agents.
	CheckServerDialUnconfigured = "CHECK_SERVER_DIAL_UNCONFIGURED"
	// CheckAgentSessionEstablished: the agent's session is up.
	CheckAgentSessionEstablished = "CHECK_AGENT_SESSION_ESTABLISHED"
	// CheckAgentSessionWaiting: the agent started; its first session is
	// not up yet.
	CheckAgentSessionWaiting = "CHECK_AGENT_SESSION_WAITING"
	// CheckAgentSessionUnobserved: the server cannot observe sessions here.
	CheckAgentSessionUnobserved = "CHECK_AGENT_SESSION_UNOBSERVED"
	// CheckPrecheckPassed: all {total} precheck items passed.
	CheckPrecheckPassed = "CHECK_PRECHECK_PASSED"
	// CheckPrecheckAttention: {count} of {total} precheck items need
	// attention: {checks} (check ids).
	CheckPrecheckAttention = "CHECK_PRECHECK_ATTENTION"
	// CheckPrecheckEmpty: the provider precheck reported no items.
	CheckPrecheckEmpty = "CHECK_PRECHECK_EMPTY"
)
