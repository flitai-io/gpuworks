package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/rpccontext"
)

var ErrDoctorResponse = errors.New("invalid Agent doctor response")

// DoctorService chooses by live control connectivity, never by the persisted
// node state. Agent RPC results are normalized into the same report as access checks.
type DoctorService struct {
	Nodes      Nodes
	Settings   func(string) (ConnectionNode, error)
	Connection ConnectionDiagnoser
}

func (d *DoctorService) Diagnose(ctx context.Context, uuid string, inspect bool) (DoctorReport, error) {
	node, err := d.Settings(uuid)
	if err != nil {
		return DoctorReport{}, err
	}
	if d.Nodes.Online(uuid) {
		rpcCtx, cancel := rpccontext.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		raw, callErr := d.Nodes.CallNode(rpcCtx, uuid, proto.RPCNodeDoctor, map[string]any{})
		if callErr == nil {
			var report proto.DoctorReport
			if json.Unmarshal(raw, &report) != nil {
				return DoctorReport{}, ErrDoctorResponse
			}
			return AgentDoctorReport(report), nil
		}
		if ctx.Err() != nil || d.Nodes.Online(uuid) {
			return DoctorReport{}, callErr
		}
	}
	report := d.Connection.Diagnose(ctx, node, inspect)
	for _, stage := range []string{"host", "runtimes", "accelerators"} {
		report.Checks = append(report.Checks, DoctorCheck{Stage: stage, Status: "unavailable", Reason: "requires_agent", Vantage: "node"})
	}
	report.Summary = doctorSummary(report.Checks)
	var text strings.Builder
	text.WriteString("Node diagnostics\n")
	for _, check := range report.Checks {
		fmt.Fprintf(&text, "%s: %s (%s)\n", check.Stage, check.Status, check.Reason)
	}
	text.WriteString("Host, runtime and accelerator checks require a connected Agent.\n")
	report.Report = text.String()
	return report, nil
}
