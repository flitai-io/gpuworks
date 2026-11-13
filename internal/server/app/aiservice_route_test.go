package app

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The assistant sends each call the way its service says: the management
// node's proxy read per call, no proxy at all, or the service's own sealed
// proxy. A proxy that cannot be unsealed makes the service unusable rather
// than sending the call direct.
func TestAIServiceRouteFollowsTheStoredChoice(t *testing.T) {
	f := newAIFixture(t, "ai-route")
	box, err := auth.NewSecretbox("ai-route-master-secret")
	if err != nil {
		t.Fatal(err)
	}
	f.ai.box = box

	route, err := f.ai.serviceRoute(&store.AIConfig{ProxyMode: "management"})
	if err != nil {
		t.Fatal(err)
	}
	if r, err := route(); err != nil || r.Direct || r.Proxy.HTTPProxy != "" {
		t.Fatalf("management without a proxy = %+v err=%v, want the process environment", r, err)
	}
	if _, err := f.st.CreatePullProxy(store.PullProxySpec{HTTPProxy: "http://mgmt.example:3128", ControlPlane: true}, ""); err != nil {
		t.Fatal(err)
	}
	if r, err := route(); err != nil || r.Proxy.HTTPProxy != "http://mgmt.example:3128" {
		t.Fatalf("management after choosing a proxy = %+v err=%v, want it read per call", r, err)
	}

	route, _ = f.ai.serviceRoute(&store.AIConfig{ProxyMode: "direct"})
	if r, _ := route(); !r.Direct {
		t.Fatalf("direct = %+v", r)
	}

	sealed, err := store.EncryptAIKey(box, "http://dave:pw@own.example:3128")
	if err != nil {
		t.Fatal(err)
	}
	route, err = f.ai.serviceRoute(&store.AIConfig{ProxyMode: "custom", ProxyURL: sealed})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := route(); r.Direct || r.Proxy.HTTPSProxy != "http://dave:pw@own.example:3128" {
		t.Fatalf("custom = %+v", r)
	}

	if _, err := f.st.CreateAIConfig(&store.AIConfig{Name: "broken", BaseURL: "https://llm.example.com/v1", Model: "m", IsDefault: true,
		ProxyMode: string(aihelper.ProxyCustom), ProxyURL: "enc:v1:not-a-sealed-value"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ai.prepare(aihelper.AskRequest{Task: aihelper.TaskAsk, Question: "hi"}); !errors.Is(err, errNoAI) {
		t.Fatalf("unsealable proxy = %v, want errNoAI", err)
	}
}
