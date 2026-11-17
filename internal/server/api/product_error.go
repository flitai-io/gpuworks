package api

import (
	"errors"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

func productError(code, fallback string, params map[string]any) error {
	return messages.NewProductError(code, fallback, params)
}

func wrapProductError(code, fallback string, params map[string]any, cause error) error {
	return messages.WrapProductError(code, fallback, params, cause)
}

func writeProductError(w http.ResponseWriter, status int, code, fallback string, params map[string]any) {
	writeErr(w, status, productError(code, fallback, params))
}

// containsProductCode reports whether err is, or wraps, the product error code.
func containsProductCode(err error, code string) bool {
	var product *messages.ProductError
	return err != nil && errors.As(err, &product) && product.Code() == code
}
