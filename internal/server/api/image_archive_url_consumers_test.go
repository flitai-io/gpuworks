package api

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

func productErrorCode(t *testing.T, err error) string {
	t.Helper()
	var product *messages.ProductError
	if !errors.As(err, &product) {
		t.Fatalf("error = %v, want a product error", err)
	}
	return product.Code()
}
