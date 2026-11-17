package autodl

import (
	"errors"
	"strings"
)

// errAPINotFound marks an AutoDL API answer saying the instance does not
// exist (any more); call() attaches it where the answer is read.
var errAPINotFound = errors.New("autodl: instance not found")

// ErrOutOfStock marks AutoDL refusing a create because the spec has no free
// machine right now; nothing was rented, and a later try may succeed.
var ErrOutOfStock = errors.New("autodl: the GPU spec is out of stock")

// markedError keeps an answer's text and adds what it means (kind) for
// errors.Is.
type markedError struct{ err, kind error }

func (e *markedError) Error() string   { return e.err.Error() }
func (e *markedError) Unwrap() []error { return []error{e.err, e.kind} }

// classifyAnswer marks a failed AutoDL answer with what it means. External
// text: the AutoDL API states these only in words, in its envelope (code/msg,
// e.g. `code=InternalError msg=当前算力规格暂无库存, 请修改配置或稍等再试`) or
// error body; this is read here, where call() received the answer, and
// nowhere downstream.
func classifyAnswer(err error, status int, text string) error {
	switch {
	case strings.Contains(text, "暂无库存"):
		return &markedError{err: err, kind: ErrOutOfStock}
	case status == 404 || strings.Contains(strings.ToLower(text), "not found"):
		return &markedError{err: err, kind: errAPINotFound}
	}
	return err
}
