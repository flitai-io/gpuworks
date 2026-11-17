package proto

import (
	"errors"
	"fmt"
	"testing"
)

func TestRPCErrorMatchesByCodeThroughWrapping(t *testing.T) {
	err := fmt.Errorf("call node: %w", UnknownRPCMethod(RPCFSStat))
	if !errors.Is(err, ErrRPCUnknownMethod) || errors.Is(err, ErrAgentUpgradeInProgress) {
		t.Fatalf("errors.Is mismatched for %v", err)
	}
	if err.Error() != "call node: unknown rpc method: fs.stat" {
		t.Fatalf("text = %q; older servers read it", err)
	}
	if errors.Is(&RPCError{Message: "x"}, &RPCError{Message: "x"}) {
		t.Fatal("RPCErrors without a code must not match")
	}
	if errors.Is(errors.New("unknown rpc method: fs.stat"), ErrRPCUnknownMethod) {
		t.Fatal("text alone must not match")
	}
}
