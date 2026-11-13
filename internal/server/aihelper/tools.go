package aihelper

import (
	"context"
	"encoding/json"
	"fmt"
)

// ToolEnv is what the built-in tools read: the platform API as the asking
// user reads it, and two findings the app computes that no endpoint answers.
// Every tool is read-only.
type ToolEnv interface {
	// API is the platform API as the asking user reads it, or nil when the
	// request carries none.
	API() APIReader
	CompareWithHealthyReplica(instanceUUID string) (any, error)
	GPUTopology(nodeUUID string) (any, error)
}

// Tool is a callable, read-only tool.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Call        func(ctx context.Context, env ToolEnv, args json.RawMessage) (string, error)
}

// Def converts a Tool to an LLM tool definition.
func (t Tool) Def() ToolDef {
	var d ToolDef
	d.Type = "function"
	d.Function.Name = t.Name
	d.Function.Description = t.Description
	d.Function.Parameters = t.Schema
	return d
}

// Run calls the tool the way every caller hands its answer on - the agent
// loop to the model, MCP to an outside agent: a failure reads "error: ...",
// and an answer past maxToolResultChars is cut, saying how to narrow it.
// errText is the failure alone, empty on success.
func (t Tool) Run(ctx context.Context, env ToolEnv, args json.RawMessage) (result, errText string) {
	out, err := t.Call(ctx, env, args)
	if err != nil {
		return "error: " + err.Error(), err.Error()
	}
	return truncateToolResult(out), ""
}

// LookupTool finds a built-in tool by name.
func LookupTool(name string) (Tool, bool) {
	for _, t := range BuiltinTools() {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

func truncateToolResult(s string) string {
	if len(s) <= maxToolResultChars {
		return s
	}
	return clip(s, maxToolResultChars) + fmt.Sprintf("\n[truncated %d more bytes; narrow the query (a keyword, a time range or a smaller limit) to see the rest]", len(s)-maxToolResultChars)
}

// BuiltinTools is the read-only tool registry: the platform API, found and
// read like the UI reads it, plus the two findings the API does not answer
// on its own.
func BuiltinTools() []Tool {
	return []Tool{
		clusterOverviewTool(),
		searchAPITool(),
		callAPITool(),
		searchMetricsTool(),
		nodeConnectionDiagnosticsTool(),
		{
			Name:        "get_gpu_topology",
			Description: "Get a node's GPU interconnect as the platform reads it: which cards share a fast link (NVLink or the vendor's equivalent) and which NUMA node each sits on.",
			Schema:      obj(`{"node_uuid":{"type":"string","description":"the node's uuid or name"}}`, "node_uuid"),
			Call: func(ctx context.Context, env ToolEnv, args json.RawMessage) (string, error) {
				var a struct {
					NodeUUID string `json:"node_uuid"`
				}
				if err := strictArgs(args, &a); err != nil {
					return "", err
				}
				uuid := a.NodeUUID
				if api := env.API(); api != nil && uuid != "" && !nodeUUID.MatchString(uuid) {
					resolved, err := nodeUUIDByName(ctx, api, uuid)
					if err != nil {
						return "", err
					}
					uuid = resolved
				}
				return jsonResult(env.GPUTopology(uuid))
			},
		},
		{
			Name:        "compare_with_healthy_replica",
			Description: "Compare a failed instance with a running replica of the same deployment: launch command differences, node, GPUs and image. Useful when only some replicas fail.",
			Schema:      obj(`{"instance_uuid":{"type":"string"}}`, "instance_uuid"),
			Call: func(ctx context.Context, env ToolEnv, args json.RawMessage) (string, error) {
				var a struct {
					InstanceUUID string `json:"instance_uuid"`
				}
				if err := strictArgs(args, &a); err != nil {
					return "", err
				}
				return jsonResult(env.CompareWithHealthyReplica(a.InstanceUUID))
			},
		},
	}
}

func obj(props string, required ...string) json.RawMessage {
	req, _ := json.Marshal(required)
	return json.RawMessage(`{"type":"object","properties":{` + propsToSchema(props) + `},"required":` + string(req) + `}`)
}

// propsToSchema wraps a raw property map fragment (already a valid object body).
func propsToSchema(props string) string {
	// props is like `"node_uuid":{"type":"string"}` — strip the outer braces if present.
	if len(props) >= 2 && props[0] == '{' && props[len(props)-1] == '}' {
		return props[1 : len(props)-1]
	}
	return props
}

func jsonResult(v any, err error) (string, error) {
	if err != nil {
		return "", err
	}
	b, e := json.Marshal(v)
	if e != nil {
		return "", e
	}
	return string(b), nil
}
