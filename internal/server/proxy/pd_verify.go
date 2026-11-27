package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

// PDHandoff is the outcome of one verification handoff through a named
// prefill instance (plan §4.7): whether the decoder pulled KV for it and how
// many bytes it counted doing so.
type PDHandoff struct {
	PrefillUUID    string
	DecodeUUID     string
	DecodeNodeUUID string
	PrefillNode    string
	// Bytes is the growth of the decoder's KV transfer byte counter across
	// the handoff; zero with Failed unset means the engine exposes no NIXL
	// counters.
	Bytes float64
	// Measured is false when the mode's engine exposes no transfer counter;
	// the handoff is then judged by the decoder's answer alone.
	Measured  bool
	Failed    bool
	Reason    string
	StartedAt time.Time
	EndedAt   time.Time
}

// verifyPrompt is long enough to span several KV blocks on any engine, so a
// real transfer shows on the decoder's counters, and short enough to cost
// nothing to prefill.
var verifyPrompt = strings.Repeat("gpuworks verifies the prefill to decode handoff. ", 64)

// VerifyPDHandoff sends one small request through the two stages of a PD
// deployment, pinned to the named prefill instance and a decoder the
// balancer picks, and reads the decoder's KV transfer counters before and
// after. It is an internal check: the request is not accounted as traffic.
func (p *Proxy) VerifyPDHandoff(ctx context.Context, deploymentID uint, prefillUUID string) (PDHandoff, error) {
	out := PDHandoff{PrefillUUID: prefillUUID, StartedAt: time.Now()}
	route, decoders, err := p.pdRouteByID(deploymentID)
	if err != nil {
		return out, err
	}
	var prefill *candidate
	for i := range route.prefill {
		if route.prefill[i].instanceUUID == prefillUUID {
			prefill = &route.prefill[i]
		}
	}
	if prefill == nil {
		return out, fmt.Errorf("prefill instance %s is not routable", prefillUUID)
	}
	out.PrefillNode = prefill.nodeUUID
	lease, _ := p.balancer.pick(decoders, nil, p.hub.Online, admission{})
	if lease == nil {
		return out, errors.New("no decode instance is routable")
	}
	defer lease.release(outcomeCanceled)
	decode := lease.c
	out.DecodeUUID, out.DecodeNodeUUID = decode.instanceUUID, decode.nodeUUID

	body, _ := json.Marshal(map[string]any{"model": decode.modelName, "prompt": verifyPrompt, "max_tokens": 1, "temperature": 0})
	if route.mode.Wire == pdmode.WireConcurrent {
		return p.verifyConcurrent(ctx, out, *prefill, decode, body)
	}
	out.Measured = route.mode.TransferBytesMetric != ""
	before, err := p.scrapeTransfer(ctx, decode)
	if err != nil {
		return out, fmt.Errorf("read decoder metrics: %w", err)
	}
	transferID, engineID := "", ""
	if route.mode.SynthesizedHandoff {
		id, err := p.bootstrapEngineID(ctx, *prefill, route.placement.TP(pdmode.RolePrefill))
		if err != nil {
			out.Failed, out.Reason, out.EndedAt = true, pdFallbackNoBootstrap, time.Now()
			return out, nil
		}
		transferID, engineID = newTransferID(), id
	}
	prefillRequest, _ := prefillBody(body, transferID)
	resp, err := p.callPrefill(ctx, *prefill, "/v1/completions", prefillRequest, nil)
	if err != nil {
		out.Failed, out.Reason, out.EndedAt = true, pdFallbackUnreachable, time.Now()
		return out, nil
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxPrefillResponseBytes))
	_ = resp.Body.Close()
	var answer struct {
		Params json.RawMessage `json:"kv_transfer_params"`
	}
	_ = json.Unmarshal(data, &answer)
	if route.mode.SynthesizedHandoff && resp.StatusCode == http.StatusOK {
		answer.Params = synthesizedHandoff(*prefill, engineID, transferID)
	}
	if resp.StatusCode != http.StatusOK || len(bytes.TrimSpace(answer.Params)) == 0 || bytes.Equal(bytes.TrimSpace(answer.Params), []byte("null")) {
		out.Failed, out.Reason, out.EndedAt = true, pdFallbackNoKVParams, time.Now()
		if resp.StatusCode != http.StatusOK {
			out.Reason = pdFallbackServerError
		}
		return out, nil
	}
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(body, &doc)
	doc[kvTransferField] = answer.Params
	decodeBody, _ := json.Marshal(doc)
	status, err := p.post(ctx, decode, "/v1/completions", decodeBody)
	after, scrapeErr := p.scrapeTransfer(ctx, decode)
	out.EndedAt = time.Now()
	switch {
	case err != nil || status != http.StatusOK:
		out.Failed, out.Reason = true, "decode_failed"
	case scrapeErr != nil:
		return out, fmt.Errorf("read decoder metrics: %w", scrapeErr)
	case after.failed > before.failed:
		out.Failed, out.Reason = true, "transfer_failed"
	default:
		out.Bytes = after.bytes - before.bytes
	}
	return out, nil
}

// pdRouteByID finds a PD deployment's routing state in the current snapshot.
func (p *Proxy) pdRouteByID(deploymentID uint) (*pdRoute, []candidate, error) {
	p.routes.rebuild()
	snap := p.routes.current.Load()
	if snap != nil {
		for _, deployments := range snap.byName {
			for _, rd := range deployments {
				if rd.id == deploymentID && rd.pd != nil {
					return rd.pd, rd.candidates, nil
				}
			}
		}
	}
	return nil, nil, fmt.Errorf("deployment %d is not a routable PD deployment", deploymentID)
}

type transferCounters struct{ bytes, failed float64 }

// scrapeTransfer reads a decoder's NIXL counters from its /metrics.
func (p *Proxy) scrapeTransfer(ctx context.Context, c candidate) (transferCounters, error) {
	ctx = context.WithValue(ctx, targetKey, target{nodeUUID: c.nodeUUID, port: c.port, instanceUUID: c.instanceUUID})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/metrics", c.port), nil)
	if err != nil {
		return transferCounters{}, err
	}
	resp, err := p.transport.RoundTrip(req)
	if err != nil {
		return transferCounters{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return transferCounters{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	return parseTransferCounters(resp.Body), nil
}

// parseTransferCounters sums the NIXL byte and failure counters across label
// sets (TP ranks, engines) in a Prometheus text exposition.
func parseTransferCounters(r io.Reader) transferCounters {
	var out transferCounters
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		var dst *float64
		switch {
		case strings.HasPrefix(line, "vllm:nixl_bytes_transferred_sum"):
			dst = &out.bytes
		case strings.HasPrefix(line, "vllm:nixl_num_failed_transfers_total"):
			dst = &out.failed
		default:
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if v, err := strconv.ParseFloat(fields[len(fields)-1], 64); err == nil {
			*dst += v
		}
	}
	return out
}

// post sends a non-streamed request to one instance and drains its answer.
func (p *Proxy) post(ctx context.Context, c candidate, path string, body []byte) (int, error) {
	return p.postStage(ctx, c, path, body, nil)
}

// postStage is post for a request's prefill copy, carrying its stage's span.
func (p *Proxy) postStage(ctx context.Context, c candidate, path string, body []byte, stage *pdStage) (int, error) {
	resp, err := p.callPrefill(ctx, c, path, body, stage)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxPrefillResponseBytes))
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// verifyConcurrent runs one concurrent handoff (SGLang): the prefill and the
// decode request go out together with the same bootstrap room, and the
// decoder's 200 is the evidence; the engine exposes no transfer counter.
func (p *Proxy) verifyConcurrent(ctx context.Context, out PDHandoff, prefill, decode candidate, body []byte) (PDHandoff, error) {
	if prefill.kvHost == "" || prefill.kvPort == 0 {
		return out, fmt.Errorf("prefill instance %s has no bootstrap endpoint", prefill.instanceUUID)
	}
	var doc map[string]any
	_ = json.Unmarshal(body, &doc)
	doc["bootstrap_host"], doc["bootstrap_port"], doc["bootstrap_room"] = prefill.kvHost, prefill.kvPort, time.Now().UnixNano()&(1<<62-1)
	paired, _ := json.Marshal(doc)
	prefillDone := make(chan int, 1)
	go func() {
		status, err := p.post(ctx, prefill, "/v1/completions", paired)
		if err != nil {
			status = 0
		}
		prefillDone <- status
	}()
	status, err := p.post(ctx, decode, "/v1/completions", paired)
	prefillStatus := <-prefillDone
	out.EndedAt = time.Now()
	switch {
	case prefillStatus == 0:
		out.Failed, out.Reason = true, pdFallbackUnreachable
	case prefillStatus >= 500:
		out.Failed, out.Reason = true, pdFallbackServerError
	case err != nil || status != http.StatusOK:
		out.Failed, out.Reason = true, "decode_failed"
	}
	return out, nil
}
