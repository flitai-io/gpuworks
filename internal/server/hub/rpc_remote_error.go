package hub

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// rpcRemoteError separates a received refusal from a send/connection failure.
// A refusal proves that the request was handled, unlike a lost RPC response.
// Its text is the agent's; what the refusal means is its cause, a
// proto.RPCError or proto.StorageFilesError, which callers reach with
// errors.Is / errors.As.
type rpcRemoteError struct {
	detail string
	cause  error
}

func (e *rpcRemoteError) Error() string { return e.detail }
func (e *rpcRemoteError) Unwrap() error { return e.cause }

// remoteRPCError is the read boundary for an agent's refusal. An agent that
// sends a code is believed. An agent older than codes only ever sent text, and
// mixed-version fleets are permanent, so its refusals are recognized here by
// the exact sentences those agents send (external text: the older agent's
// wire output) and nowhere downstream.
func remoteRPCError(method, code, detail string, params map[string]any) error {
	if code == "" {
		code, params = legacyRPCError(method, detail)
	}
	if code == "" {
		return &rpcRemoteError{detail: detail}
	}
	cause := proto.RPCErrorFromWire(code, detail, params)
	if coded, ok := cause.(*proto.RPCError); ok {
		// The API answers a ProductError with its code and params, so an
		// agent refusal that reaches a response is rendered from its code
		// (apiError.<CODE>); errors.Is against the proto sentinel still holds.
		cause = messages.WrapProductError(coded.Code, detail, coded.Params, coded)
	}
	return &rpcRemoteError{detail: detail, cause: cause}
}

// storageFilesLegacyPrefix starts an older agent's storage-files refusal:
// "storage_files:<code>: <detail>", possibly after a prefix of its own.
const storageFilesLegacyPrefix = "storage_files:"

func legacyRPCError(method, detail string) (string, map[string]any) {
	text := strings.TrimSpace(detail)
	if i := strings.Index(text, storageFilesLegacyPrefix); i >= 0 {
		if code, rest, found := strings.Cut(text[i+len(storageFilesLegacyPrefix):], ": "); found {
			return proto.NewStorageFilesError(code, "%s", rest).RPCCode(), map[string]any{"detail": rest}
		}
	}
	switch {
	case strings.HasPrefix(text, "unknown rpc method: "):
		return proto.ErrorRPCUnknownMethod, nil
	case text == "agent upgrade already in progress":
		return proto.ErrorAgentUpgradeInProgress, nil
	case method == proto.RPCFSStat && strings.HasSuffix(text, ": no such file or directory"):
		// os.Stat's *PathError for ENOENT on the (linux/darwin) agent.
		return proto.ErrorPathNotFound, nil
	case method == proto.RPCBenchProbe && strings.HasPrefix(text, "models probe returned 404"):
		return proto.ErrorBenchEndpointNotFound, nil
	}
	return "", nil
}

// upgradeStatusCode is the code of an agent.upgrade.status failure; an agent
// older than codes said it only in words (external text: its wire output).
func upgradeStatusCode(status proto.AgentUpgradeStatus) string {
	if code := proto.NormalizeCode(status.Code); code != "" {
		return code
	}
	text := strings.TrimSpace(status.Error)
	switch {
	case strings.HasPrefix(text, "rolled back:"):
		return proto.ErrorAgentUpgradeRolledBack
	case text == "agent upgrade already in progress":
		return proto.ErrorAgentUpgradeInProgress
	}
	return ""
}
