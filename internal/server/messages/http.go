package messages

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Generic HTTP error codes. An HTTP error whose cause carries no code of its
// own (a store failure, a malformed id, a missing resource) is answered with
// the code of its status's category, so every error response has a code. A
// generic code says only the category; the English error text keeps the
// specific detail, and a client shows both (ui/src/apiError.ts).
const (
	ErrorBadRequest          = "BAD_REQUEST"
	ErrorUnauthorized        = "UNAUTHORIZED"
	ErrorForbidden           = "FORBIDDEN"
	ErrorNotFound            = "NOT_FOUND"
	ErrorMethodNotAllowed    = "METHOD_NOT_ALLOWED"
	ErrorRequestTimeout      = "REQUEST_TIMEOUT"
	ErrorConflict            = "CONFLICT"
	ErrorGone                = "GONE"
	ErrorRequestTooLarge     = "REQUEST_TOO_LARGE"
	ErrorUnsupportedMedia    = "UNSUPPORTED_MEDIA_TYPE"
	ErrorRangeNotSatisfiable = "RANGE_NOT_SATISFIABLE"
	ErrorUnprocessable       = "UNPROCESSABLE"
	ErrorFailedDependency    = "FAILED_DEPENDENCY"
	ErrorTooManyRequests     = "TOO_MANY_REQUESTS"
	ErrorInternal            = "INTERNAL_ERROR"
	ErrorNotImplemented      = "NOT_IMPLEMENTED"
	ErrorBadGateway          = "BAD_GATEWAY"
	ErrorServiceUnavailable  = "SERVICE_UNAVAILABLE"
	ErrorGatewayTimeout      = "GATEWAY_TIMEOUT"
	ErrorInsufficientStorage = "INSUFFICIENT_STORAGE"
)

// genericErrorCodes maps the statuses the server answers errors with to
// their category's code.
var genericErrorCodes = map[int]string{
	http.StatusBadRequest:                   ErrorBadRequest,
	http.StatusUnauthorized:                 ErrorUnauthorized,
	http.StatusForbidden:                    ErrorForbidden,
	http.StatusNotFound:                     ErrorNotFound,
	http.StatusMethodNotAllowed:             ErrorMethodNotAllowed,
	http.StatusRequestTimeout:               ErrorRequestTimeout,
	http.StatusConflict:                     ErrorConflict,
	http.StatusGone:                         ErrorGone,
	http.StatusRequestEntityTooLarge:        ErrorRequestTooLarge,
	http.StatusUnsupportedMediaType:         ErrorUnsupportedMedia,
	http.StatusRequestedRangeNotSatisfiable: ErrorRangeNotSatisfiable,
	http.StatusUnprocessableEntity:          ErrorUnprocessable,
	http.StatusFailedDependency:             ErrorFailedDependency,
	http.StatusTooManyRequests:              ErrorTooManyRequests,
	http.StatusInternalServerError:          ErrorInternal,
	http.StatusNotImplemented:               ErrorNotImplemented,
	http.StatusBadGateway:                   ErrorBadGateway,
	http.StatusServiceUnavailable:           ErrorServiceUnavailable,
	http.StatusGatewayTimeout:               ErrorGatewayTimeout,
	http.StatusInsufficientStorage:          ErrorInsufficientStorage,
}

// GenericErrorCode is the code of status's category: its own for a status
// the server answers with, otherwise BAD_REQUEST for a client error and
// INTERNAL_ERROR for anything else.
func GenericErrorCode(status int) string {
	if code, ok := genericErrorCodes[status]; ok {
		return code
	}
	if status >= 400 && status < 500 {
		return ErrorBadRequest
	}
	return ErrorInternal
}

// GenericErrorCodes lists every generic code, for the checks that each has
// client text.
func GenericErrorCodes() []string {
	out := make([]string, 0, len(genericErrorCodes))
	for _, code := range genericErrorCodes {
		out = append(out, code)
	}
	return out
}

// IsGenericErrorCode reports whether code is a status category's code.
func IsGenericErrorCode(code string) bool {
	for _, generic := range genericErrorCodes {
		if generic == code {
			return true
		}
	}
	return false
}

// HTTPErrorCode is the code and params an HTTP error answering err carries:
// a product error's own, otherwise status's generic code and no params.
func HTTPErrorCode(status int, err error) (string, map[string]any) {
	var product *ProductError
	if errors.As(err, &product) && product.Code() != "" {
		return product.Code(), product.Params()
	}
	return GenericErrorCode(status), nil
}

// WriteHTTPError answers err as {"error","code","params"}, the management
// API's error shape, for a handler outside the API package (the agent
// connect route). The API's own writer adds diagnostics and usage counting.
func WriteHTTPError(w http.ResponseWriter, status int, err error) {
	code, params := HTTPErrorCode(status, err)
	body := map[string]any{"error": err.Error(), "code": code}
	if len(params) > 0 {
		body["params"] = params
	}
	encoded, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(encoded, '\n'))
}
