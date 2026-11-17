package proto

// Codes an agent puts on an rpc.resp (or an agent.upgrade.status) refusal
// that the server acts on. The server decides by the code, never by the
// error text.
const (
	// ErrorRPCUnknownMethod: this agent does not implement the method; it
	// predates it.
	ErrorRPCUnknownMethod = "RPC_UNKNOWN_METHOD"
	// ErrorAgentUpgradeInProgress: an agent upgrade is already running on the
	// node.
	ErrorAgentUpgradeInProgress = "AGENT_UPGRADE_IN_PROGRESS"
	// ErrorBenchEndpointNotFound: a benchmark probe's GET /v1/models was
	// answered 404; the URL is not the root of an OpenAI-compatible API.
	ErrorBenchEndpointNotFound = "BENCHMARK_ENDPOINT_NOT_FOUND"
	// ErrorPathNotFound: the path an fs RPC named does not exist on the
	// node.
	ErrorPathNotFound = "PATH_NOT_FOUND"
	// ErrorAgentUpgradeRolledBack: an agent.upgrade.status failure reporting that
	// the new binary never registered and the previous one was restored.
	ErrorAgentUpgradeRolledBack = "AGENT_UPGRADE_ROLLED_BACK"
)

// RPCError is an RPC refusal with a code. On the agent it is what a handler
// returns for a coded refusal; on the server it is what an rpc.resp carrying
// a code becomes. errors.Is matches any two RPCErrors with the same code, so
// callers test against ErrRPCUnknownMethod / ErrAgentUpgradeInProgress.
type RPCError struct {
	Code    string
	Message string
	// Params are the refusal's interpolation parameters, as on the wire.
	Params map[string]any
}

// RPCCoder is an error that travels on an rpc.resp with a code and params:
// *RPCError and *StorageFilesError.
type RPCCoder interface {
	error
	RPCCode() string
	RPCParams() map[string]any
}

// RPCCode implements RPCCoder.
func (e *RPCError) RPCCode() string { return e.Code }

// RPCParams implements RPCCoder.
func (e *RPCError) RPCParams() map[string]any { return e.Params }

// RPCErrorFromWire is the refusal an rpc.resp carrying a code stands for: a
// *StorageFilesError for a storage-files code, an *RPCError otherwise.
func RPCErrorFromWire(code, message string, params map[string]any) error {
	code = NormalizeCode(code)
	if refusal, ok := storageFilesErrorFromRPC(code, params); ok {
		return refusal
	}
	return &RPCError{Code: code, Message: message, Params: params}
}

func (e *RPCError) Error() string { return e.Message }

// Is matches another RPCError with the same non-empty code.
func (e *RPCError) Is(target error) bool {
	t, ok := target.(*RPCError)
	return ok && t.Code != "" && t.Code == e.Code
}

var (
	// ErrRPCUnknownMethod matches an RPCError with code ErrorRPCUnknownMethod.
	ErrRPCUnknownMethod = &RPCError{Code: ErrorRPCUnknownMethod, Message: "unknown rpc method"}
	// ErrAgentUpgradeInProgress matches an RPCError with code
	// ErrorAgentUpgradeInProgress.
	ErrAgentUpgradeInProgress = &RPCError{Code: ErrorAgentUpgradeInProgress, Message: "agent upgrade already in progress"}
	// ErrBenchEndpointNotFound matches an RPCError with code
	// ErrorBenchEndpointNotFound.
	ErrBenchEndpointNotFound = &RPCError{Code: ErrorBenchEndpointNotFound, Message: "models endpoint not found"}
	// ErrPathNotFound matches an RPCError with code ErrorPathNotFound.
	ErrPathNotFound = &RPCError{Code: ErrorPathNotFound, Message: "no such file or directory"}
)

// UnknownRPCMethod is the refusal of a method this agent does not implement.
// The text is the one agents have always sent, so an older server that only
// reads text still recognizes it.
func UnknownRPCMethod(method string) *RPCError {
	return &RPCError{Code: ErrorRPCUnknownMethod, Message: "unknown rpc method: " + method}
}
