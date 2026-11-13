package aihelper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// NodeConnectionDiagnosticReader is an optional, fixed read-only operation.
// The request's API reader applies the same permissions and diagnostic
// projection as the UI. It does not permit arbitrary POST requests.
type NodeConnectionDiagnosticReader interface {
	NodeConnectionDiagnostics(ctx context.Context, nodeUUID string) (int, []byte, error)
}

func nodeConnectionDiagnosticsTool() Tool {
	return Tool{
		Name: "get_node_connection_diagnostics",
		Description: "Read a node's connection evidence as the asking user (administrator required): saved control-plane address, recovery records, historical public entry and current Agent observations or optional SSH checks. " +
			"Uses only the node's stored identity and addresses; does not recover, restart, reinstall or change configuration. " +
			"HTTP 200 may be a partial report: inspect each check's outcome, vantage, target and observed_at. Offline, unavailable or not_checked is missing evidence, not a network failure. " +
			"Historical addresses or differing URLs do not prove the node's current target is unreachable; HTTP success does not prove WebSocket or token authentication.",
		Schema: obj(`{"node_uuid":{"type":"string","description":"the node's 32-character lowercase hexadecimal UUID"}}`, "node_uuid"),
		Call: func(ctx context.Context, env ToolEnv, args json.RawMessage) (string, error) {
			var a struct {
				NodeUUID string `json:"node_uuid"`
			}
			decoder := json.NewDecoder(bytes.NewReader(args))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&a); err != nil {
				return "", errors.New("connection diagnostics accepts only a node_uuid JSON object")
			}
			if decoder.Decode(new(any)) != io.EOF {
				return "", errors.New("connection diagnostics accepts only one JSON object")
			}
			if !nodeUUID.MatchString(a.NodeUUID) {
				return "", errors.New("node_uuid must be a 32-character lowercase hexadecimal UUID")
			}
			reader, ok := env.API().(NodeConnectionDiagnosticReader)
			if !ok {
				return "", errors.New("node connection diagnostics are unavailable to this request")
			}
			status, body, err := reader.NodeConnectionDiagnostics(ctx, a.NodeUUID)
			if err != nil {
				return "", err
			}
			if status != 200 {
				return "", fmt.Errorf("HTTP %d: %s", status, clip(redactString(string(body)), 1500))
			}
			var data any
			if json.Unmarshal(body, &data) != nil {
				return "", errors.New("node connection diagnostics returned an invalid report")
			}
			// A runtime snapshot has up to 64 facts. The normal 20-item list
			// default would hide later handshake phases of the same report.
			return compact(shape(humanize(redact(data)), maxListLimit), ""), nil
		},
	}
}
