package api

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/lifecycle"
)

// Handlers take sleep and wake orders and record placement hints in the
// server's Orders (Deps.Orders, the controller's); handlers built without
// one keep a single one of their own, so their orders still exclude each
// other.
func TestHandlersUseTheServersOrders(t *testing.T) {
	server := &lifecycle.Orders{}
	shared := &apiHandlers{d: Deps{Orders: server}}
	if shared.orders() != server {
		t.Fatal("the handlers do not use the Orders they were given")
	}
	own := &apiHandlers{}
	first := own.orders()
	if first == nil || own.orders() != first {
		t.Fatal("handlers built without Orders do not keep one of their own")
	}
	if first == server {
		t.Fatal("two servers' handlers share Orders")
	}
}
