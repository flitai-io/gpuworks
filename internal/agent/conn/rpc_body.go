package conn

import (
	"errors"

	"gpuworks.ai/gpuworks/internal/proto"
)

// rpcRespBody mirrors the server's rpcResponse.
type rpcRespBody struct {
	Result any            `json:"result,omitempty"`
	Error  string         `json:"error,omitempty"`
	Code   string         `json:"code,omitempty"`
	Params map[string]any `json:"params,omitempty"`
}

// rpcErrorBody is the refusal of an RPC: its text, and the code and params of
// a proto.RPCCoder anywhere in its chain.
func rpcErrorBody(err error) rpcRespBody {
	body := rpcRespBody{Error: err.Error()}
	var coded proto.RPCCoder
	if errors.As(err, &coded) {
		body.Code, body.Params = coded.RPCCode(), coded.RPCParams()
	}
	return body
}
