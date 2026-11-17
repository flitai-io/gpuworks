package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"

	hubpkg "gpuworks.ai/gpuworks/internal/server/hub"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Gateway error codes: every error the gateway itself answers on /v1 (and the
// playground route that shares it) is one of these, written from
// gatewayErrors by writeAPIError. The console renders them from
// apiError.<CODE> in common.json; the English template is the fallback
// every other client reads.
const (
	ErrorInvalidAPIKey               = "INVALID_API_KEY"
	ErrorModelAccessDenied           = "MODEL_ACCESS_DENIED"
	ErrorModelRequired               = "MODEL_REQUIRED"
	ErrorRequestTooLarge             = messages.ErrorRequestTooLarge
	ErrorRequestBodyInvalid          = "REQUEST_BODY_INVALID"
	ErrorRequestBodyNotUTF8          = "REQUEST_BODY_NOT_UTF8"
	ErrorPriorityClassUnknown        = "PRIORITY_CLASS_UNKNOWN"
	ErrorMethodNotAllowed            = messages.ErrorMethodNotAllowed
	ErrorChatInputUnreadable         = "CHAT_INPUT_UNREADABLE"
	ErrorModalityUnsupported         = messages.ErrorModelInputModalityUnsupported
	ErrorModelNotFound               = "MODEL_NOT_FOUND"
	ErrorModelRouteNotExposed        = "MODEL_ROUTE_NOT_EXPOSED"
	ErrorTokenizeDisabled            = "TOKENIZE_DISABLED"
	ErrorRouteNotFound               = "ROUTE_NOT_FOUND"
	ErrorInstanceNotFound            = "INSTANCE_NOT_FOUND"
	ErrorInstanceModelMismatch       = "INSTANCE_MODEL_MISMATCH"
	ErrorModelAsleep                 = "MODEL_ASLEEP"
	ErrorInstanceAsleep              = "INSTANCE_ASLEEP"
	ErrorInstanceNotRunning          = "INSTANCE_NOT_RUNNING"
	ErrorInstanceOffline             = "INSTANCE_OFFLINE"
	ErrorModelNotReady               = "MODEL_NOT_READY"
	ErrorDeploymentStartFailed       = "DEPLOYMENT_START_FAILED"
	ErrorPDPlacementUnreadable       = "PD_PLACEMENT_UNREADABLE"
	ErrorPDPrefillUnavailable        = "PD_PREFILL_UNAVAILABLE"
	ErrorDistributedNotReady         = "DISTRIBUTED_NOT_READY"
	ErrorDistributedLeaderOffline    = "DISTRIBUTED_LEADER_OFFLINE"
	ErrorWakeRequestEnded            = "WAKE_REQUEST_ENDED"
	ErrorServerRestarting            = "SERVER_RESTARTING"
	ErrorUpstreamUnavailable         = "UPSTREAM_UNAVAILABLE"
	ErrorDataLaneUnavailable         = "DATA_LANE_UNAVAILABLE"
	ErrorInstanceUnavailable         = "INSTANCE_UNAVAILABLE"
	ErrorUpstreamRejected            = "UPSTREAM_REJECTED"
	ErrorExternalRouteUnsupported    = "EXTERNAL_ROUTE_UNSUPPORTED"
	ErrorExternalModelInvalid        = "EXTERNAL_MODEL_INVALID"
	ErrorExternalModelNotFound       = "EXTERNAL_MODEL_NOT_FOUND"
	ErrorExternalEndpointInvalid     = "EXTERNAL_ENDPOINT_INVALID"
	ErrorExternalKeyUnreadable       = "EXTERNAL_KEY_UNREADABLE"
	ErrorExternalRequestInvalid      = "EXTERNAL_REQUEST_INVALID"
	ErrorExternalUpstreamUnavailable = "EXTERNAL_UPSTREAM_UNAVAILABLE"
	ErrorGatewayInternal             = messages.ErrorInternal
)

// OpenAI error types, the error.type OpenAI clients read.
const (
	typeInvalidRequest = "invalid_request_error"
	typeAuthentication = "authentication_error"
	typePermission     = "permission_error"
	typeNotFound       = "not_found_error"
	typeServer         = "server_error"
)

// gatewayError is one row of the gateway's error table: the HTTP status, the
// OpenAI error type, the request field the error is about (OpenAI's param,
// empty for none) and the English message, whose {name} placeholders the
// params fill. Status 0 is the upstream's own status (UPSTREAM_REJECTED).
type gatewayError struct {
	status   int
	typ      string
	param    string
	template string
}

var gatewayErrors = map[string]gatewayError{
	ErrorInvalidAPIKey:         {http.StatusUnauthorized, typeAuthentication, "", "invalid or missing API key"},
	ErrorModelAccessDenied:     {http.StatusForbidden, typePermission, "model", "this API key may not call model {model}"},
	ErrorModelRequired:         {http.StatusBadRequest, typeInvalidRequest, "model", "request body must specify a model"},
	ErrorRequestTooLarge:       {http.StatusRequestEntityTooLarge, typeInvalidRequest, "", "request body exceeds {max_bytes} bytes"},
	ErrorRequestBodyInvalid:    {http.StatusBadRequest, typeInvalidRequest, "", "{detail}"},
	ErrorRequestBodyNotUTF8:    {http.StatusBadRequest, typeInvalidRequest, "", "request body must be UTF-8 JSON"},
	ErrorPriorityClassUnknown:  {http.StatusBadRequest, typeInvalidRequest, "", "unknown priority class in {header}"},
	ErrorMethodNotAllowed:      {http.StatusMethodNotAllowed, typeInvalidRequest, "", "{path} only accepts {method}"},
	ErrorChatInputUnreadable:   {http.StatusBadRequest, typeInvalidRequest, "messages", "Unable to validate chat input"},
	ErrorModalityUnsupported:   {http.StatusBadRequest, typeInvalidRequest, "messages", "This model does not support {modality} input (supported: {supported}). Remove the attachment or select a compatible model."},
	ErrorModelNotFound:         {http.StatusNotFound, typeNotFound, "model", "no deployment serves model {model}"},
	ErrorModelRouteNotExposed:  {http.StatusNotFound, typeNotFound, "", "deployment {model} does not expose {method} {path}"},
	ErrorTokenizeDisabled:      {http.StatusNotFound, typeNotFound, "", "deployment {model} does not expose {method} {path}: tokenize is turned off for it"},
	ErrorRouteNotFound:         {http.StatusNotFound, typeNotFound, "", "{path} is not served on this address"},
	ErrorInstanceNotFound:      {http.StatusNotFound, typeNotFound, "", "instance {instance} does not exist"},
	ErrorInstanceModelMismatch: {http.StatusNotFound, typeNotFound, "model", "instance {instance} does not serve {model}"},
	// The leading "model is asleep; wake it first" stays as it was, for
	// clients that match on it.
	ErrorModelAsleep:              {http.StatusServiceUnavailable, typeServer, "", "model is asleep; wake it first (console, or POST /api/v1/deployments/{deployment_id}/wake; a schedule or a waiting wake may also wake it)"},
	ErrorInstanceAsleep:           {http.StatusServiceUnavailable, typeServer, "", "instance {instance} does not take requests while its deployment sleeps; wake it first"},
	ErrorInstanceNotRunning:       {http.StatusServiceUnavailable, typeServer, "", "instance {instance} is not awake and running"},
	ErrorInstanceOffline:          {http.StatusServiceUnavailable, typeServer, "", "instance {instance} is offline"},
	ErrorModelNotReady:            {http.StatusServiceUnavailable, typeServer, "", "no running instance for {model} (still starting?)"},
	ErrorDeploymentStartFailed:    {http.StatusServiceUnavailable, typeServer, "", "deployment for {model} failed to start; see the deployment list"},
	ErrorPDPlacementUnreadable:    {http.StatusServiceUnavailable, typeServer, "", "the deployment's PD placement cannot be read"},
	ErrorPDPrefillUnavailable:     {http.StatusServiceUnavailable, typeServer, "", "no prefill instance could take the request ({reason})"},
	ErrorDistributedNotReady:      {http.StatusServiceUnavailable, typeServer, "", "distributed model {model} is not ready: no replica has every rank running on an online node"},
	ErrorDistributedLeaderOffline: {http.StatusServiceUnavailable, typeServer, "", "distributed model leader is offline for {model}"},
	ErrorWakeRequestEnded:         {http.StatusServiceUnavailable, typeServer, "", "the request ended while the model woke"},
	// The server stops: the request is safe to send again.
	ErrorServerRestarting:            {http.StatusServiceUnavailable, typeServer, "", "the server is restarting; send the request again"},
	ErrorUpstreamUnavailable:         {http.StatusBadGateway, typeServer, "", "upstream error: {detail}"},
	ErrorDataLaneUnavailable:         {http.StatusServiceUnavailable, typeServer, "", "upstream error: {detail}"},
	ErrorInstanceUnavailable:         {http.StatusServiceUnavailable, typeServer, "", "upstream error: {detail}"},
	ErrorUpstreamRejected:            {0, "", "", "{detail}"},
	ErrorExternalRouteUnsupported:    {http.StatusNotFound, typeNotFound, "", "external playground models only support chat completions"},
	ErrorExternalModelInvalid:        {http.StatusBadRequest, typeInvalidRequest, "model", "invalid external playground model"},
	ErrorExternalModelNotFound:       {http.StatusNotFound, typeNotFound, "model", "external playground model no longer exists"},
	ErrorExternalEndpointInvalid:     {http.StatusBadRequest, typeInvalidRequest, "", "external endpoint has an invalid base URL"},
	ErrorExternalKeyUnreadable:       {http.StatusInternalServerError, typeServer, "", "decrypt external endpoint key: {detail}"},
	ErrorExternalRequestInvalid:      {http.StatusBadRequest, typeInvalidRequest, "", "external playground requests must be JSON: {detail}"},
	ErrorExternalUpstreamUnavailable: {http.StatusBadGateway, typeServer, "", "external upstream error: {detail}"},
	ErrorGatewayInternal:             {http.StatusInternalServerError, typeServer, "", "{detail}"},
}

// refusal is a gateway error a step of the request path settled on: a code
// of gatewayErrors and its params. A nil *refusal is no refusal; it is an
// error so the request-reading steps can return it.
type refusal struct {
	code   string
	params map[string]any
}

func refuse(code string, params map[string]any) *refusal {
	return &refusal{code: code, params: params}
}

// status is the HTTP status the refusal answers with, 0 for none.
func (e *refusal) status() int {
	if e == nil {
		return 0
	}
	return gatewayErrors[e.code].status
}

// message is the refusal's English text.
func (e *refusal) message() string {
	if e == nil {
		return ""
	}
	return renderGatewayMessage(gatewayErrors[e.code].template, e.params)
}

func (e *refusal) Error() string { return e.message() }

var placeholder = regexp.MustCompile(`\{([a-z_]+)\}`)

func renderGatewayMessage(template string, params map[string]any) string {
	return placeholder.ReplaceAllStringFunc(template, func(m string) string {
		if v, ok := params[m[1:len(m)-1]]; ok {
			return fmt.Sprint(v)
		}
		return m
	})
}

// apiErrorObject is the OpenAI error object, with the params the message was
// filled from for a client that renders the code in its own language.
type apiErrorObject struct {
	Message string         `json:"message"`
	Type    string         `json:"type"`
	Code    string         `json:"code"`
	Param   *string        `json:"param"`
	Params  map[string]any `json:"params,omitempty"`
}

// apiErrorBody is the {"error": {...}} body of code; status types an
// UPSTREAM_REJECTED error, whose status is the upstream's.
func apiErrorBody(code string, params map[string]any, status int) []byte {
	row := gatewayErrors[code]
	object := apiErrorObject{Message: renderGatewayMessage(row.template, params), Type: row.typ, Code: code, Params: params}
	if object.Type == "" {
		object.Type = typeInvalidRequest
		if status >= 500 {
			object.Type = typeServer
		}
	}
	if row.param != "" {
		param := row.param
		object.Param = &param
	}
	encoded, _ := json.Marshal(map[string]apiErrorObject{"error": object})
	return encoded
}

// writeAPIError answers r with the gateway error code and its params, and
// names the request in X-Request-ID when no earlier step did. It is the only
// writer of the gateway's own errors.
func writeAPIError(w http.ResponseWriter, r *http.Request, code string, params map[string]any) {
	if w.Header().Get("X-Request-ID") == "" {
		w.Header().Set("X-Request-ID", requestID(r))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(gatewayErrors[code].status)
	_, _ = w.Write(append(apiErrorBody(code, params, 0), '\n'))
}

// writeRefusal answers r with e.
func writeRefusal(w http.ResponseWriter, r *http.Request, e *refusal) {
	writeAPIError(w, r, e.code, e.params)
}

// WriteRouteNotFound answers a /v1 path that this listening address does not
// serve (the server's function routing) the way the gateway answers.
func WriteRouteNotFound(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, r, ErrorRouteNotFound, map[string]any{"path": r.URL.Path})
}

// requestRefusal is the refusal answering an error from reading or rewriting
// the request: its own when it is one (a body over the limit), otherwise the
// request body is invalid, with the reader's text as the detail.
func requestRefusal(err error) *refusal {
	var e *refusal
	if errors.As(err, &e) {
		return e
	}
	return refuse(ErrorRequestBodyInvalid, map[string]any{"detail": err.Error()})
}

// upstreamErrorCode classifies an error forwarding a request (the reverse
// proxy's transport): a detached data lane that did not reattach within the
// hub's wait budget, or an instance that stopped under the request, is a
// distinct, retryable condition, not a dead upstream - the node is online and
// the tunnel will likely open on the next attempt.
func upstreamErrorCode(err error) string {
	switch {
	case errors.Is(err, hubpkg.ErrDataLaneUnavailable):
		return ErrorDataLaneUnavailable
	case errors.Is(err, errInstanceUnavailable):
		return ErrorInstanceUnavailable
	}
	return ErrorUpstreamUnavailable
}
