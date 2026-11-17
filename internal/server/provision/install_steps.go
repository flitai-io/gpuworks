package provision

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// The install step rows the Linux and the Mac install share.

func archCheck(uname, goarch string) CheckResult {
	return PassCheck(CheckIDArch, messages.CheckArchDetected, uname+" -> "+goarch, map[string]any{"uname": uname, "arch": goarch})
}

func agentBinaryReadyCheck(binary string, bytes int) CheckResult {
	return PassCheck(CheckIDAgentBinary, messages.CheckAgentBinaryReady, fmt.Sprintf("%s (%d bytes)", binary, bytes), map[string]any{"binary": binary, "bytes": bytes})
}

func agentBinaryMissingCheck(binary string, err error) CheckResult {
	return FailCheck(CheckIDAgentBinary, messages.CheckAgentBinaryMissing, "the server has no "+binary, map[string]any{"binary": binary}).WithEvidence(err.Error())
}

func uploadedCheck(path string) CheckResult {
	return PassCheck(CheckIDUpload, messages.CheckAgentUploaded, path, map[string]any{"path": path})
}

func uploadFailedCheck(path, out string, err error) CheckResult {
	return FailCheck(CheckIDUpload, messages.CheckAgentUploadFailed, "the agent could not be put at "+path, map[string]any{"path": path}).WithEvidence(outputEvidence(out, err))
}

// startedCheck keeps the installer's output as evidence: it is the script's
// own account of what it registered.
func startedCheck(out string) CheckResult {
	return PassCheck(CheckIDStartAgent, messages.CheckAgentStarted, "the installer started the agent", nil).WithEvidence(out)
}

func startFailedCheck(out string, err error) CheckResult {
	return FailCheck(CheckIDStartAgent, messages.CheckAgentStartFailed, "the installer failed", nil).WithEvidence(outputEvidence(out, err))
}

func agentRunningCheck(dir, evidence string) CheckResult {
	return PassCheck(CheckIDAgentProcess, messages.CheckAgentRunning, "running with state directory "+dir, map[string]any{"dir": dir}).WithEvidence(evidence)
}

func agentNotObservedCheck(dir, evidence string) CheckResult {
	return WarnCheck(CheckIDAgentProcess, messages.CheckAgentNotObserved,
		"no agent was observed running with state directory "+dir+"; the unit may need daemon-reload", map[string]any{"dir": dir}).WithEvidence(evidence)
}
