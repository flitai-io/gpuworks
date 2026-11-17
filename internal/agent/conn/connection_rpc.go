package conn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// A caller's RPC deadline does not travel over the wire. Keep a local limit,
// and never turn this snapshot request into a new connection or a doctor run.
const connectionDiagnosticBudget = 10 * time.Second

func (c *Conn) handleConnectionDiagnosticsRPC(ctx context.Context, env proto.Envelope, params json.RawMessage) {
	report, err := c.connectionDiagnostics(ctx, params)
	resp := rpcRespBody{Result: report}
	if err != nil {
		resp = rpcErrorBody(err)
	}
	_ = c.send(proto.TRPCResp, env.ID, resp)
}

func (c *Conn) connectionDiagnostics(ctx context.Context, params json.RawMessage) (proto.DoctorReport, error) {
	if !c.diagnosticRunning.CompareAndSwap(false, true) {
		return proto.DoctorReport{}, errors.New("connection diagnostics already running")
	}
	defer c.diagnosticRunning.Store(false)
	ctx, cancel := context.WithTimeout(ctx, connectionDiagnosticBudget)
	defer cancel()
	if len(params) > 1024 {
		return proto.DoctorReport{}, errors.New("connection diagnostics accepts an empty object")
	}
	if len(bytes.TrimSpace(params)) != 0 {
		dec := json.NewDecoder(bytes.NewReader(params))
		dec.DisallowUnknownFields()
		var input struct{}
		if err := dec.Decode(&input); err != nil {
			return proto.DoctorReport{}, errors.New("connection diagnostics accepts an empty object")
		}
		var extra any
		if dec.Decode(&extra) != io.EOF {
			return proto.DoctorReport{}, errors.New("connection diagnostics accepts an empty object")
		}
	}
	if err := ctx.Err(); err != nil {
		return proto.DoctorReport{}, err
	}
	report := c.ConnectionReport()
	if err := ctx.Err(); err != nil {
		return proto.DoctorReport{}, err
	}
	return report, nil
}
