package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Known health payloads share one bounded read. Strata keeps HTTP200 while
// its child is not READY; the catalog wrapper uses HTTP503 on loader failure.
type engineReadiness struct {
	Service string `json:"service"`
	Loaded  *bool  `json:"loaded"`
	APIKey  *bool  `json:"api_key"`
	// These fields belong to the catalog callable wrapper. Keep them opaque
	// here so another server's status/error shape cannot break Strata probes.
	Status json.RawMessage `json:"status"`
	Error  json.RawMessage `json:"error"`
}

func readEngineReadiness(resp *http.Response) (engineReadiness, error) {
	defer resp.Body.Close()
	var state engineReadiness
	err := readProbeJSON(resp.Body, 64<<10, &state)
	return state, err
}

// checkEngineReadiness retires engines that repeatedly fail their HTTP check.
// A Strata child failure still uses its existing in-container recovery path.
func (e *Engine) checkEngineReadiness(ctx context.Context, serving servingInstance) {
	spec := serving.spec
	if spec.Headless || spec.APIPort <= 0 || spec.DesiredSleep > 0 {
		return
	}
	e.mu.Lock()
	asleep := e.sleepLevel[spec.UUID] > 0
	e.mu.Unlock()
	if asleep {
		return
	}
	client := &http.Client{Timeout: e.healthProbeTimeout}
	resp, probeErr := probeEngineHealth(ctx, spec, client, e.healthProbeAttempts, e.healthProbeInterval)
	if ctx.Err() != nil || errors.Is(probeErr, errInvalidHealthEndpoint) {
		if resp != nil {
			resp.Body.Close()
		}
		return
	}
	if probeErr == nil {
		state, decodeErr := readEngineReadiness(resp)
		if spec.Engine != engines.Callable || decodeErr != nil || state.Service != "strata" || state.Loaded == nil || *state.Loaded || state.APIKey == nil || *state.APIKey {
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/v1/models", spec.APIPort), nil)
		if err != nil {
			return
		}
		resp, err = client.Do(req)
		if err != nil {
			return
		}
		models, valid := readStrataModels(resp)
		if !valid || len(models) > 0 {
			return
		}
	}
	e.mu.Lock()
	current, ok := e.watchableLocked(spec.UUID, serving.epoch)
	if !ok || ctx.Err() != nil || current.DesiredSleep > 0 || e.sleepLevel[spec.UUID] > 0 {
		e.mu.Unlock()
		return
	}
	delete(e.servingEpoch, spec.UUID)
	e.inFlight[spec.UUID] = true
	attemptCtx, cancel := context.WithCancel(ctx)
	e.inFlightCancel[spec.UUID] = cancel
	e.mu.Unlock()
	defer e.finishEnsure(current)
	if probeErr != nil {
		e.retireStopped(attemptCtx, current, serving.epoch, probeErr.Error())
		return
	}
	e.reportStateProgress(current, proto.InstStarting, containerName(spec.UUID), "", "engine child is not loaded; recovering", proto.PhaseHealthCheck, 0, 0, 0, 0)
	e.probeAndWatch(attemptCtx, current, containerName(current.UUID), serving.epoch)
}

func readStrataModels(resp *http.Response) ([]json.RawMessage, bool) {
	defer resp.Body.Close()
	var models struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}
	err := readProbeJSON(resp.Body, 1<<20, &models)
	return models.Data, err == nil && resp.StatusCode == http.StatusOK && models.Object == "list" && models.Data != nil
}

func intentionallyUnloaded(models []json.RawMessage) bool {
	for _, raw := range models {
		var model struct {
			Status struct {
				Value string `json:"value"`
			} `json:"status"`
		}
		if json.Unmarshal(raw, &model) == nil && model.Status.Value == "unloaded" {
			return true
		}
	}
	return false
}

// requestStrataLoad initiates one load for an owned startup attempt. The
// short HTTP timeout only releases the probe's socket: the pinned server's
// loader continues after disconnect, while probeAndReport keeps its original
// startup deadline. Results are consumed by that same owner, never reported
// by this goroutine after removal or a successful readiness transition.
func requestStrataLoad(ctx context.Context, client *http.Client, port int) <-chan string {
	done := make(chan string, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/load", port), strings.NewReader("{}"))
		if err != nil {
			done <- fmt.Sprintf("invalid engine load endpoint: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if resp != nil {
			// Readiness determines the outcome; this drain only permits socket reuse.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
		}
		detail := ""
		if err != nil {
			detail = fmt.Sprintf("engine child is not loaded; waiting for readiness (/load: %v)", err)
		} else if resp.StatusCode != http.StatusOK {
			detail = fmt.Sprintf("engine child is not loaded; /load returned HTTP %d", resp.StatusCode)
		}
		done <- detail
	}()
	return done
}

// A failed or partial JSON response is not evidence for a recovery action.
func readProbeJSON(body io.Reader, limit int64, dst any) error {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("engine probe JSON exceeds %d bytes", limit)
	}
	return json.Unmarshal(data, dst)
}
