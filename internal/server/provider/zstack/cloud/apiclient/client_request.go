package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxCloudResponseBytes = 8 << 20

type cloudReply struct {
	status     int
	body       []byte
	retryAfter time.Duration
	Location   string          `json:"location"`
	APITimeout int64           `json:"apiTimeout"`
	Error      json.RawMessage `json:"error"`
	Diagnostic *struct {
		Code string `json:"code"`
	} `json:"diagnostic"`
}

func (c *APIClient) request(ctx context.Context, policy clientPolicy, method, path string, body []byte, jobID string) (cloudReply, error) {
	delay := policy.retryInitial
	var lastErr *APIError
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return cloudReply{}, interruptedRequestError(err, lastErr)
		}
		started := time.Now()
		reply, err := c.requestOnce(ctx, policy, method, path, body, jobID)
		if policy.logger != nil {
			kind := ErrorKind("")
			var apiErr *APIError
			if errors.As(err, &apiErr) {
				kind = apiErr.Kind
			}
			policy.logger.DebugContext(ctx, "Cloud API request", "method", method,
				"path", strings.SplitN(path, "?", 2)[0], "attempt", attempt+1,
				"status", reply.status, "duration", time.Since(started), "error_kind", kind)
		}
		var apiErr *APIError
		if err == nil || method != http.MethodGet || attempt >= policy.readRetries ||
			!errors.As(err, &apiErr) || !apiErr.retryable {
			return reply, err
		}
		lastErr = apiErr
		if err := waitForCloud(ctx, max(delay, reply.retryAfter)); err != nil {
			return reply, interruptedRequestError(err, lastErr)
		}
		if delay > policy.retryMax/2 {
			delay = policy.retryMax
		} else {
			delay *= 2
		}
	}
}

func (c *APIClient) requestOnce(ctx context.Context, policy clientPolicy, method, path string, body []byte, jobID string) (cloudReply, error) {
	ctx, cancel := context.WithTimeout(ctx, policy.requestTimeout)
	defer cancel()
	reply := cloudReply{}
	req, err := c.newRequest(ctx, method, path, bytes.NewReader(body))
	if err != nil {
		return reply, err
	}
	if jobID != "" {
		req.Header.Set("X-Job-UUID", jobID)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	if method != http.MethodGet && policy.serverTimeout > 0 {
		seconds := policy.serverTimeout / time.Second
		if policy.serverTimeout%time.Second != 0 {
			seconds++
		}
		req.Header.Set("X-API-Timeout", strconv.FormatInt(int64(seconds), 10))
	}
	req, phase := traceConnection(req)
	viaProxy := requestUsesProxy(c.http, req)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
		apiErr := transportError(err)
		apiErr.Phase, apiErr.ViaProxy = phase(), viaProxy
		apiErr.OutcomeUnknown = method != http.MethodGet
		return reply, apiErr
	}
	defer resp.Body.Close()
	reply.status = resp.StatusCode
	reply.retryAfter = cloudRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxCloudResponseBytes+1))
	if readErr != nil {
		if ctx.Err() != nil {
			readErr = errors.Join(readErr, ctx.Err())
		}
		apiErr := transportError(readErr)
		apiErr.Phase, apiErr.ViaProxy = PhaseResponse, viaProxy
		apiErr.StatusCode, apiErr.OutcomeUnknown = reply.status, method != http.MethodGet
		return reply, apiErr
	}
	if len(raw) > maxCloudResponseBytes {
		return reply, &APIError{Kind: ErrorResponse, StatusCode: reply.status, OutcomeUnknown: method != http.MethodGet}
	}
	reply.body = raw
	trimmed := bytes.TrimSpace(raw)
	valid := len(trimmed) > 0 && trimmed[0] == '{' && json.Unmarshal(raw, &reply) == nil
	if err := reply.responseError(valid); err != nil {
		err.ProtocolFailure = protocolFailure(method, path, reply, err)
		err.OutcomeUnknown = method != http.MethodGet && !err.definitive
		return reply, err
	}
	return reply, nil
}

func (r cloudReply) responseError(valid bool) *APIError {
	hasError := valid && len(r.Error) > 0 && string(r.Error) != "null"
	code := ""
	authReason := AuthenticationReason("")
	cloudFailure := false
	if hasError {
		var detail struct {
			Code    string `json:"code"`
			Details string `json:"details"`
		}
		cloudFailure = json.Unmarshal(r.Error, &detail) == nil && detail.Code != ""
		code = detail.Code
		authReason = authenticationReason(detail.Details)
	}
	if valid && code == "" && r.Diagnostic != nil {
		code = r.Diagnostic.Code
	}
	err := &APIError{StatusCode: r.status, Code: code}
	if valid && r.Diagnostic != nil {
		err.DiagnosticCode = r.Diagnostic.Code
	}
	switch {
	case r.status == http.StatusUnauthorized || r.status == http.StatusForbidden:
		err.Kind, err.definitive = ErrorAuthentication, true
		err.AuthReason = authReason
	case cloudFailure:
		err.Kind, err.definitive = ErrorOperation, true
		if r.status >= http.StatusBadRequest && r.status < http.StatusInternalServerError {
			err.Kind = ErrorRequest
		}
	case hasError:
		err.Kind = ErrorResponse
	case r.status == http.StatusTooManyRequests || r.status == http.StatusBadGateway ||
		r.status == http.StatusServiceUnavailable || r.status == http.StatusGatewayTimeout:
		err.Kind, err.retryable = ErrorNetwork, true
	case r.status != http.StatusOK && r.status != http.StatusAccepted:
		err.Kind = ErrorRequest
		err.definitive = r.status >= http.StatusMultipleChoices && r.status < http.StatusInternalServerError && r.status != http.StatusRequestTimeout
	case !valid:
		err.Kind = ErrorResponse
	default:
		return nil
	}
	return err
}

func cloudRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && seconds >= 0 {
		const longest = time.Duration(1<<63 - 1)
		if seconds > int64(longest/time.Second) {
			return longest
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(0, date.Sub(now))
	}
	return 0
}

func waitForCloud(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}
