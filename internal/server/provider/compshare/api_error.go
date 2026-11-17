package compshare

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// errInstanceNotFound is a listing that does not contain the instance asked
// for: it is gone.
var errInstanceNotFound = errors.New("compshare: instance not found")

// ErrCapacityUnavailable is a placement that found no zone with room for the
// requested machine: CompShare has no such machine right now.
var ErrCapacityUnavailable = errors.New("compshare: requested capacity unavailable")

// APIError is a CompShare (UCloud) API answer that is not a success: a non-2xx
// HTTP status, or a 2xx envelope with a non-zero RetCode. Callers read
// RetCode with errors.As; what an answer means for a retry or a delete is
// decided by the methods below, which are the only readers of Message.
type APIError struct {
	Status  int
	RetCode int
	Message string
}

func (e *APIError) Error() string {
	if e.RetCode != 0 {
		return fmt.Sprintf("compshare: API RetCode %d: %s", e.RetCode, e.Message)
	}
	return fmt.Sprintf("compshare: HTTP %d: %s", e.Status, e.Message)
}

// RetCodes CompShare answers for a capacity or backend blip. 226645 is a
// zone briefly out of stock during create; 8433 is "Service error, please
// retry", observed (2026-09 CI) to make a whole zone's capacity answers vanish
// for a query round, so it rides the same retry.
const (
	retCodeCreateBusy   = 226645
	retCodeServiceError = 8433
)

// transient: the request may succeed on another try. External text:
// "service unavailable" is what CompShare's gateway says (HTTP 503 body or
// RetCode message) when it sheds load.
func (e *APIError) transient() bool {
	return e.RetCode == retCodeCreateBusy || e.RetCode == retCodeServiceError ||
		e.Status >= http.StatusInternalServerError || strings.Contains(strings.ToLower(e.Message), "service unavailable")
}

// notFound: the API says the resource does not exist (any more). External
// text: CompShare's Describe/Terminate actions name a deleted instance only in
// words, in English or Chinese.
func (e *APIError) notFound() bool {
	text := strings.ToLower(e.Message)
	return strings.Contains(text, "not found") || strings.Contains(text, "not exist") ||
		strings.Contains(text, "不存在") || strings.Contains(text, "已删除")
}

// RetCode matches, through errors.Is, a CompShare API answer with that
// RetCode anywhere in an error tree (a placement walk joins one error per
// zone): errors.Is(err, compshare.RetCode(226641)).
type RetCode int

func (c RetCode) Error() string { return fmt.Sprintf("compshare: API RetCode %d", int(c)) }

// Is matches a RetCode target equal to this answer's RetCode.
func (e *APIError) Is(target error) bool {
	code, ok := target.(RetCode)
	return ok && e.RetCode != 0 && int(code) == e.RetCode
}
