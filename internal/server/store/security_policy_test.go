package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// A database without the row, or with a corrupt one, is the restrictive
// policy; a written policy reads back unchanged.
func TestSecurityPolicyDefaultsAndRoundTrip(t *testing.T) {
	st, err := Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if got := st.SecurityPolicy(); got != (SecurityPolicy{}) {
		t.Fatalf("fresh policy = %+v, want zero", got)
	}
	if err := st.SetSecurityPolicy(SecurityPolicy{PlaygroundPreviewNetwork: true}); err != nil {
		t.Fatal(err)
	}
	if raw := st.GetSetting(SettingSecurityPolicy); raw != `{"playground_preview_network":true,"inference_open_access":false}` {
		t.Fatalf("stored value = %s", raw)
	}
	if got := st.SecurityPolicy(); !got.PlaygroundPreviewNetwork || got.InferenceOpenAccess {
		t.Fatalf("round trip = %+v", got)
	}
	if err := st.SetSecurityPolicy(SecurityPolicy{InferenceOpenAccess: true}); err != nil {
		t.Fatal(err)
	}
	if got := st.SecurityPolicy(); got.PlaygroundPreviewNetwork || !got.InferenceOpenAccess {
		t.Fatalf("open access round trip = %+v", got)
	}
	if err := st.SetSetting(SettingSecurityPolicy, `{"playground_preview_network":`); err != nil {
		t.Fatal(err)
	}
	if got := st.SecurityPolicy(); got != (SecurityPolicy{}) {
		t.Fatalf("corrupt value = %+v, want zero", got)
	}
}

// The decision path reports an unreadable setting instead of answering with
// the default, so the gateway can tell "closed" from "cannot tell".
func TestInferenceOpenAccessFailsClosedAndReportsReadErrors(t *testing.T) {
	st, err := Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// No row at all: the default, closed, and not an error.
	if open, err := st.InferenceOpenAccess(); open || err != nil {
		t.Fatalf("missing row = %v, %v; want false, nil", open, err)
	}
	if err := st.SetSecurityPolicy(SecurityPolicy{InferenceOpenAccess: true}); err != nil {
		t.Fatal(err)
	}
	if open, err := st.InferenceOpenAccess(); !open || err != nil {
		t.Fatalf("enabled = %v, %v; want true, nil", open, err)
	}
	// A corrupt value is the restrictive policy, not a storage failure.
	if err := st.SetSetting(SettingSecurityPolicy, `{"inference_open_access":`); err != nil {
		t.Fatal(err)
	}
	if open, err := st.InferenceOpenAccess(); open || err != nil {
		t.Fatalf("corrupt row = %v, %v; want false, nil (the restrictive policy)", open, err)
	}
}
