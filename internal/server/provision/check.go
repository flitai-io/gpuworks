package provision

import (
	"fmt"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

// CheckResult is one onboarding check (ADR 0067): the SSH precheck, the SSH
// install and a cloud provider's install all report these, and the API
// returns them as they are. ID says which check it is, Status what came of
// it (proto.Check*), Code with Params the outcome the client renders in its
// language, and Message the English fallback. Evidence is what a remote
// command, an SSH library or a provider API answered, kept apart from the
// message so the message never carries foreign text.
type CheckResult struct {
	ID       string            `json:"id"`
	Status   string            `json:"status"`
	Code     string            `json:"code,omitempty"`
	Params   map[string]string `json:"params,omitempty"`
	Message  string            `json:"message,omitempty"`
	Evidence string            `json:"evidence,omitempty"`
}

// IDs of the onboarding checks, lower_snake. A check keeps its id across the
// precheck, the install and the providers' reports, so a client names it the
// same way wherever it appears.
const (
	CheckIDSSH                   = "ssh"
	CheckIDSSHAuth               = "ssh_auth"
	CheckIDPlatformKey           = "platform_key"
	CheckIDPlatformKeyProvider   = "platform_key_provider"
	CheckIDInstancePassword      = "instance_password"
	CheckIDOSArch                = "os_arch"
	CheckIDSudo                  = "sudo"
	CheckIDPython                = "python3"
	CheckIDGPU                   = "gpu"
	CheckIDFUSE                  = "fuse"
	CheckIDDeviceAccess          = "device_access"
	CheckIDDocker                = "docker"
	CheckIDNvidiaToolkit         = "nvidia_container_toolkit"
	CheckIDAgentListenPort       = "agent_listen_port"
	CheckIDServerReachability    = "server_reachability"
	CheckIDServerToAgent         = "server_to_agent"
	CheckIDConnectivity          = "connectivity"
	CheckIDServerDialPort        = "server_dial_port"
	CheckIDSecurityGroup         = "security_group"
	CheckIDCloudAssistant        = "cloud_assistant"
	CheckIDPrecheckScript        = "precheck_script"
	CheckIDInstallDir            = "install_dir"
	CheckIDInstallDirOwnership   = "install_dir_ownership"
	CheckIDInstallDirTraversal   = "install_dir_traversal"
	CheckIDInstallDirFilesystem  = "install_dir_filesystem"
	CheckIDInstallDirPersistence = "install_dir_persistence"
	CheckIDInstallDirSpace       = "install_dir_space"
	CheckIDInstallDirNames       = "install_dir_names"
	CheckIDExistingAgent         = "existing_agent"
	CheckIDExistingNode          = "existing_node"
	CheckIDPrivilege             = "privilege"
	CheckIDArch                  = "arch"
	CheckIDAgentBinary           = "agent_binary"
	CheckIDUpload                = "upload"
	CheckIDStartAgent            = "start_agent"
	CheckIDAgentProcess          = "agent_process"
	CheckIDProviderDNS           = "provider_dns"
	CheckIDDebugHelper           = "runveil_debug_helper"
	CheckIDUdockerBundle         = "udocker_bundle"
	CheckIDServerDial            = "server_dial"
	CheckIDAgentDial             = "agent_dial"
	CheckIDPrecheck              = "precheck"
	CheckIDInstallAgent          = "install_agent"
)

// ConnectionCheckID is the id of the row about a provider command channel
// (Cloud Assistant), named after the connection type.
func ConnectionCheckID(connectionType string) string {
	id := strings.ToLower(strings.TrimSpace(connectionType))
	if id == "" {
		return "connection"
	}
	return strings.NewReplacer("-", "_", " ", "_").Replace(id)
}

// newCheck builds a check. Params are written as text on the wire (numbers in
// decimal, booleans as true/false): a client interpolates them into its own
// sentence and never computes with them.
func newCheck(id, status, code, message string, params map[string]any) CheckResult {
	return CheckResult{ID: id, Status: status, Code: code, Params: paramText(params), Message: message}
}

func paramText(params map[string]any) map[string]string {
	if len(params) == 0 {
		return nil
	}
	out := make(map[string]string, len(params))
	for key, value := range params {
		switch v := value.(type) {
		case string:
			out[key] = v
		case int:
			out[key] = strconv.Itoa(v)
		case int64:
			out[key] = strconv.FormatInt(v, 10)
		case bool:
			out[key] = strconv.FormatBool(v)
		default:
			out[key] = fmt.Sprint(v)
		}
	}
	return out
}

// PassCheck is a check that ran and found nothing wrong.
func PassCheck(id, code, message string, params map[string]any) CheckResult {
	return newCheck(id, proto.CheckPass, code, message, params)
}

// WarnCheck is a check that found a problem that does not block.
func WarnCheck(id, code, message string, params map[string]any) CheckResult {
	return newCheck(id, proto.CheckWarn, code, message, params)
}

// FailCheck is a check that found a problem that blocks.
func FailCheck(id, code, message string, params map[string]any) CheckResult {
	return newCheck(id, proto.CheckFail, code, message, params)
}

// SkipCheck is a check that should have run but did not; code says why.
func SkipCheck(id, code, message string, params map[string]any) CheckResult {
	return newCheck(id, proto.CheckSkip, code, message, params)
}

// NACheck is a check that does not apply here; code says why.
func NACheck(id, code, message string, params map[string]any) CheckResult {
	return newCheck(id, proto.CheckNA, code, message, params)
}

// WithEvidence returns c carrying what an outside system answered. Blank
// evidence leaves c as it is.
func (c CheckResult) WithEvidence(evidence string) CheckResult {
	if evidence = strings.TrimSpace(evidence); evidence != "" {
		c.Evidence = evidence
	}
	return c
}

// Passed reports whether the check ran and found nothing wrong.
func (c CheckResult) Passed() bool { return c.Status == proto.CheckPass }

// Failed reports whether the check found a problem that blocks.
func (c CheckResult) Failed() bool { return c.Status == proto.CheckFail }

// outputEvidence joins a command's output and its error into evidence.
func outputEvidence(out string, err error) string {
	out = strings.TrimSpace(out)
	if err == nil {
		return out
	}
	if out == "" {
		return err.Error()
	}
	return out + ": " + err.Error()
}

// statusRank orders the statuses from the least to the most serious; skip
// outranks pass because a check that did not run is not a pass.
var statusRank = map[string]int{proto.CheckNA: 0, proto.CheckPass: 1, proto.CheckSkip: 2, proto.CheckWarn: 3, proto.CheckFail: 4}

// WorstStatus is the most serious status among checks; na when there are
// none. A status outside the five counts as skip.
func WorstStatus(checks []CheckResult) string {
	worst := proto.CheckNA
	for _, check := range checks {
		status := check.Status
		if _, ok := statusRank[status]; !ok {
			status = proto.CheckSkip
		}
		if statusRank[status] > statusRank[worst] {
			worst = status
		}
	}
	return worst
}
