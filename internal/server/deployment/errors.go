package deployment

import (
	"errors"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// Kind is what a use case's refusal is to its caller: the request is wrong,
// the cluster's present state refuses it, or the server failed. The API maps
// it onto an HTTP status; other callers read the ProductError inside.
//
// The three are the statuses create and evaluate have always answered: no
// path of theirs answers "not found" (a model or node the request names that
// does not exist is a wrong request). planning.ScaleErrorKind is a separate,
// older vocabulary for replica scaling, which does answer a deployment gone;
// the two meet when scaling moves here.
type Kind int

const (
	// KindInternal: the server failed (a store read, an inconsistency).
	KindInternal Kind = iota
	// KindInvalid: the request itself is wrong; sending it again fails again.
	KindInvalid
	// KindConflict: the cluster cannot take the request as it stands now
	// (a stale evaluation, a node gone, a storage that refuses writes).
	KindConflict
)

// kindError is an error with the Kind its caller should read it as.
type kindError struct {
	kind Kind
	err  error
}

func (e *kindError) Error() string { return e.err.Error() }
func (e *kindError) Unwrap() error { return e.err }

// KindOf is the Kind err was refused with; an error that names none is
// internal. The outermost Kind wins: the step that answers chooses.
func KindOf(err error) Kind {
	var k *kindError
	if errors.As(err, &k) {
		return k.kind
	}
	return KindInternal
}

func withKind(kind Kind, err error) error {
	if err == nil {
		return nil
	}
	return &kindError{kind: kind, err: err}
}

// invalid marks err as a wrong request.
func invalid(err error) error { return withKind(KindInvalid, err) }

// conflict marks err as refused by the cluster's present state.
func conflict(err error) error { return withKind(KindConflict, err) }

// internal marks err as the server's own failure, over any Kind a step
// below gave it.
func internal(err error) error { return withKind(KindInternal, err) }

func productError(code, fallback string, params map[string]any) error {
	return messages.NewProductError(code, fallback, params)
}

func wrapProductError(code, fallback string, params map[string]any, cause error) error {
	return messages.WrapProductError(code, fallback, params, cause)
}

func containsProductCode(err error, code string) bool {
	var product *messages.ProductError
	return err != nil && errors.As(err, &product) && product.Code() == code
}

// sleepRefusalError is a refused sleep order or declaration as a product
// error with its English fallback.
func sleepRefusalError(refusal *planning.SleepRefusal) error {
	return productError(refusal.Code, messages.SleepRefusalFallback(refusal.Code), refusal.Params)
}

// SleepDeclarationError is a refused sleep declaration as the request's
// error: a SLEEP_ code carries its own fallback text.
func SleepDeclarationError(err error) error {
	var product *messages.ProductError
	if errors.As(err, &product) {
		fallback := product.Error()
		if strings.HasPrefix(product.Code(), "SLEEP_") {
			fallback = messages.SleepRefusalFallback(product.Code())
		}
		return productError(product.Code(), fallback, product.Params())
	}
	return err
}

// YieldPolicyViolation is a malformed yield policy as a product error.
func YieldPolicyViolation(v *yieldpolicy.Violation) error {
	params := map[string]any{"field": v.Field, "reason": v.Reason}
	text := fmt.Sprintf("%s is not a valid value", v.Field)
	switch v.Reason {
	case yieldpolicy.ReasonRange:
		params["minimum"], params["maximum"] = v.Min, v.Max
		text = fmt.Sprintf("%s is %d to %d", v.Field, v.Min, v.Max)
	case yieldpolicy.ReasonRequired:
		text = fmt.Sprintf("%s is required", v.Field)
	}
	return productError(messages.ErrorYieldPolicyInvalid, text, params)
}

func nodeDisplayName(node *store.Node) string {
	if node == nil {
		return ""
	}
	return node.DisplayName()
}

// diagnosticsFromError is the diagnostic report an error explains itself
// with, nil when it carries none.
func diagnosticsFromError(err error) *dto.DiagnosticReport {
	var explained interface{ Diagnostics() *dto.DiagnosticReport }
	if errors.As(err, &explained) {
		return explained.Diagnostics()
	}
	return nil
}
