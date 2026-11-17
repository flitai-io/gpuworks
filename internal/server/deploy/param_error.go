package deploy

import "fmt"

// ParamError is a launch-configuration rule that judges one request field: a
// quick_params key, quick_params_unset, or the extra_args text. Its text is
// the rule's own sentence; Key is what a form uses to put the error on the
// control the deployer edits. Callers use errors.As, never the text.
type ParamError struct {
	Key string
	Err error
}

func (e *ParamError) Error() string { return e.Err.Error() }
func (e *ParamError) Unwrap() error { return e.Err }

// Request fields a ParamError can name besides a quick_params key.
const (
	ParamFieldQuickParamsUnset = "quick_params_unset"
	ParamFieldExtraArgs        = "extra_args"
)

func paramErrorf(key, format string, args ...any) error {
	return &ParamError{Key: key, Err: fmt.Errorf(format, args...)}
}
