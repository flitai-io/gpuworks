package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/rpccontext"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const nodeConnectionRPCTimeout = 15 * time.Second

// An attempted RPC never falls through to another 15-second SSH operation.
// The receiving agent bounds its own work; this timeout only bounds our wait.
func (a *apiHandlers) nodeConnectionAgentReport(ctx context.Context, node *store.Node, out *dto.NodeConnectionDiagnostics) (dto.NodeConnectionCheck, bool) {
	check := dto.NodeConnectionCheck{Stage: "agent_control", Vantage: "control_plane", Outcome: "unavailable"}
	if a.d.Online == nil {
		return check, false
	}
	if !a.d.Online(node.UUID) {
		check.Outcome = "offline"
		return check, false
	}
	if a.d.CallNode == nil || a.d.NodeCaps == nil {
		return check, false
	}
	if !slices.Contains(a.d.NodeCaps(node.UUID), proto.CapConnectionDiagnostics) {
		check.Outcome = "unsupported"
		return check, false
	}
	ctx, cancel := rpccontext.WithTimeout(ctx, nodeConnectionRPCTimeout)
	defer cancel()
	raw, err := a.d.CallNode(ctx, node.UUID, proto.RPCNodeConnectionDiagnostics, struct{}{})
	at := time.Now().UTC()
	check.ObservedAt = &at
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
			check.Outcome = "cancelled"
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
			check.Outcome = "timeout"
		case errors.Is(err, proto.ErrRPCUnknownMethod):
			check.Outcome = "unsupported"
		}
		return check, true
	}
	if ctx.Err() != nil {
		check.Outcome = "cancelled"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			check.Outcome = "timeout"
		}
		return check, true
	}
	out.AgentReport = safeNodeConnectionReport(raw, node.Token)
	check.Outcome = "invalid_response"
	if out.AgentReport != nil {
		check.Outcome = "runtime_snapshot"
	}
	return check, true
}

// Only primitive connection facts cross this boundary. In particular Report,
// Findings, Host, and CLI daemon-log history are not trusted display content.
func safeNodeConnectionReport(raw json.RawMessage, token string) *proto.DoctorReport {
	if len(raw) > 32*1024 {
		return nil
	}
	var report proto.DoctorReport
	if json.Unmarshal(raw, &report) != nil || len(report.Facts) > 64 {
		return nil
	}
	out := &proto.DoctorReport{}
	seen := make(map[string]bool)
	for _, fact := range report.Facts {
		value, accepted := nodeConnectionFactValue(fact.Code, fact.Value, token)
		if !accepted {
			continue
		}
		if seen[fact.Code] || len(fact.Value) > 2048 {
			return nil
		}
		seen[fact.Code] = true
		if value != "" {
			out.Facts = append(out.Facts, proto.DoctorFact{Code: fact.Code, Value: value})
		}
	}
	if proto.ConnectionValue(*out, "connection.source") != "agent_runtime" || proto.ConnectionValue(*out, "connection.checked_at") == "" {
		return nil
	}
	return out
}

func nodeConnectionFactValue(code, value, token string) (string, bool) {
	if !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 || (token != "" && strings.Contains(value, token)) {
		value = ""
	}
	values := ""
	switch code {
	case "connection.source":
		values = "agent_runtime"
	case "connection.connect_mode":
		values = "agent_dial server_dial"
	case "connection.server_source":
		values = "flag environment state_file missing agent_runtime unknown"
	case "connection.token_present":
		values = "true false"
	case "connection.control":
		values = "probing connected not_connected smoke_verified not_applicable unknown"
	case "connection.data_lane":
		values = "unverified attached detached disabled not_negotiated pending not_applicable unknown"
	case "connection.outcome":
		values = "ok failed invalid_configuration not_applicable probing unknown"
	case "connection.reason":
		values = "bad_token proto_mismatch stale_session dns connect tls ws_upgrade timeout cancelled transport invalid_configuration none"
	case "connection.proxy":
		values = "direct environment_proxy unknown"
	case "connection.server_url", "connection.proxy_url":
		return safeNodeConnectionURL(value, token, code == "connection.proxy_url"), true
	case "connection.checked_at", "connection.retry_at":
		return nodeConnectionTime(value), true
	default:
		for _, prefix := range []string{"connection.phase.", "connection.elapsed_ms.", "connection.observed_at."} {
			stage, ok := strings.CutPrefix(code, prefix)
			if !ok || !slices.Contains(strings.Fields("configuration dial dns connect proxy_connect tls ws_upgrade yamux hello wait_registered server_hello session"), stage) {
				continue
			}
			switch prefix {
			case "connection.phase.":
				values = "ok failed timeout cancelled in_progress not_observed not_checked not_applicable"
			case "connection.elapsed_ms.":
				n, err := strconv.ParseUint(value, 10, 32)
				if err == nil && n <= 86400000 && value != "" && strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
					return strconv.FormatUint(n, 10), true
				}
				return "", true
			case "connection.observed_at.":
				return nodeConnectionTime(value), true
			}
			break
		}
		if values == "" {
			return "", false
		}
	}
	if slices.Contains(strings.Fields(values), value) {
		return value, true
	}
	return "", true
}

func nodeConnectionTime(value string) string {
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return ""
	}
	return at.UTC().Format(time.RFC3339Nano)
}

func safeNodeConnectionURL(value, token string, originOnly bool) string {
	if strings.IndexFunc(value, unicode.IsControl) >= 0 || provision.ConnectionDiagnosticURL(value) == "" {
		return ""
	}
	u, err := url.Parse(value)
	if err != nil || !utf8.ValidString(value) || (token != "" && (strings.Contains(value, token) || strings.Contains(u.Path, token) || strings.Contains(u.Host, token))) {
		return ""
	}
	if originOnly && (u.Path != "" || u.RawPath != "") {
		return ""
	}
	return value
}
