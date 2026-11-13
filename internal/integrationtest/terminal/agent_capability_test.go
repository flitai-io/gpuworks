package terminal

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	agentterminal "gpuworks.ai/gpuworks/internal/agent/terminal"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestRealAgentAdvertisesWebtermCapability exercises the capability from the
// real agent Hello message through the hub and terminal API.  The other
// terminal tests intentionally use a small hand-built API harness to isolate
// admission and frame translation; this test keeps that coverage while making
// sure the production agent-to-server wiring actually enables the endpoint.
func TestRealAgentAdvertisesWebtermCapability(t *testing.T) {
	if !agentterminal.Supported() {
		t.Skip("web terminal is not supported on this platform")
	}
	env := testenv.New(t)
	env.AddNode("terminal-agent", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("fake"))
	env.WaitNodeOnline(t, "terminal-agent")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("online nodes=%+v err=%v", nodes, err)
	}
	api := env.API(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(env.URL(), "http") + "/api/v1/nodes/" + nodes[0].UUID + "/terminal?rows=24&cols=80"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + api.Token}},
	})
	if err != nil {
		t.Fatalf("real agent terminal handshake: %v", err)
	}
	defer conn.CloseNow()
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("terminal info frame: %v", err)
	}
	if typ != websocket.MessageText || !strings.Contains(string(data), `"type":"info"`) {
		t.Fatalf("terminal info type=%v data=%q", typ, data)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "capability test complete")
}

// A real agent with an udocker-labelled runtime that does not implement the
// controlled-shell extension must keep the container bit absent. The fake
// runtime is deliberate here: it exercises the production hello, status and
// preflight wiring for an old agent while the real udocker acceptance runs in
// test/l4.
func TestUdockerRuntimePreflightStaysUnavailableWithoutControlledShell(t *testing.T) {
	if !agentterminal.Supported() {
		t.Skip("web terminal is not supported on this platform")
	}
	env := testenv.New(t)
	env.AddNode("udocker-terminal-agent", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("udocker"))
	env.WaitNodeOnline(t, "udocker-terminal-agent")
	// The preflight below reads the node's reported runtime. That fact rides
	// the status report after the node goes online, so asserting without
	// waiting for it reads "unknown" under load and the reason comes back as
	// the agent not supporting container terminals at all.
	env.WaitNodeRuntime(t, "udocker-terminal-agent", "udocker")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("online nodes=%+v err=%v", nodes, err)
	}
	api := env.API(t)
	resp, err := api.Get("/api/v1/nodes/" + nodes[0].UUID + "/terminal/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preflight status=%d", resp.StatusCode)
	}
	var got struct {
		ContainerSupported  bool   `json:"container_supported"`
		ContainerReason     string `json:"container_reason"`
		ContainerRuntime    string `json:"container_runtime"`
		ContainerCapability string `json:"container_capability"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ContainerSupported || got.ContainerRuntime != "udocker" || got.ContainerCapability != proto.CapWebtermContainerUdocker || !strings.Contains(got.ContainerReason, "unavailable") {
		t.Fatalf("udocker preflight=%+v", got)
	}
}
