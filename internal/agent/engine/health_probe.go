package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

const (
	engineHealthTimeout       = 3 * time.Second
	engineHealthRetryInterval = 2 * time.Second
	// A running engine must fail three consecutive requests before it is retired.
	runningHealthAttempts = 3
)

var errInvalidHealthEndpoint = errors.New("invalid engine health endpoint")

func engineHealthRequest(ctx context.Context, spec proto.InstanceDesiredState) (*http.Request, error) {
	path := spec.HealthPath
	if path == "" {
		path = "/health"
	}
	return http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", spec.APIPort, path), nil)
}

// probeEngineHealth retries only the HTTP check. The caller owns the successful
// response and any engine-specific readiness interpretation or failure action.
func probeEngineHealth(ctx context.Context, spec proto.InstanceDesiredState, client *http.Client, attempts int, interval time.Duration) (*http.Response, error) {
	req, err := engineHealthRequest(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalidHealthEndpoint, err)
	}
	if attempts < 1 {
		return nil, fmt.Errorf("health probe attempts must be positive")
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var resp *http.Response
		resp, err = client.Do(req.Clone(ctx))
		if err == nil && resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		if resp != nil {
			if err == nil {
				err = fmt.Errorf("HTTP %d", resp.StatusCode)
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
		}
		if attempt+1 < attempts {
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, fmt.Errorf("engine health check failed after %d attempts: %w", attempts, err)
}

type healthProbeTask struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (e *Engine) startHealthProbe(ctx context.Context, serving servingInstance) {
	e.mu.Lock()
	current, ok := e.watchableLocked(serving.spec.UUID, serving.epoch)
	if !ok || ctx.Err() != nil || current.Headless || current.APIPort <= 0 ||
		current.DesiredSleep > 0 || e.processOfLocked(current.UUID).sleepLevel > 0 || e.healthProbes[current.UUID] != nil {
		e.mu.Unlock()
		return
	}
	probeCtx, cancel := context.WithCancel(ctx)
	task := &healthProbeTask{cancel: cancel, done: make(chan struct{})}
	e.healthProbes[current.UUID] = task
	e.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			e.mu.Lock()
			delete(e.healthProbes, current.UUID)
			close(task.done)
			e.mu.Unlock()
		}()
		e.checkEngineReadiness(probeCtx, servingInstance{spec: current, epoch: serving.epoch})
	}()
}

func (e *Engine) cancelHealthProbeLocked(uuid string) {
	if task := e.healthProbes[uuid]; task != nil {
		task.cancel()
	}
}

func (e *Engine) stopHealthProbes() {
	e.mu.Lock()
	tasks := make([]*healthProbeTask, 0, len(e.healthProbes))
	for _, task := range e.healthProbes {
		task.cancel()
		tasks = append(tasks, task)
	}
	e.mu.Unlock()
	for _, task := range tasks {
		<-task.done
	}
}
