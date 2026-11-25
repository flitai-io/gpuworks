//go:build docker

package l3

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentengine "gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	dockerdrv "gpuworks.ai/gpuworks/internal/agent/runtime/docker"
	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/proxy"
	"gpuworks.ai/gpuworks/internal/server/proxy/storeplane"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type childReadinessNetworks struct{}

func (childReadinessNetworks) ServingEndpoints() (map[string]network.ServingEndpoint, error) {
	return nil, nil
}

type childReadinessTunnel struct{}

func (childReadinessTunnel) Online(string) bool { return true }
func (childReadinessTunnel) OpenTunnel(ctx context.Context, _ string, dst string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", dst)
}

// Real Docker container + agent liveness + store/bus + gateway: Strata's
// internal child can be unready while HTTP and the container stay running.
func TestDockerChildReadinessRejectsWaitersAndRecoversSameContainer(t *testing.T) {
	if err := exec.Command("docker", "info").Run(); err != nil {
		skipDocker(t, err)
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "fixture")
	build := exec.Command("go", "build", "-o", binary, "./cmd/strata-health-fixture")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY fixture /fixture\nENTRYPOINT [\"/fixture\"]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	image := fmt.Sprintf("gpuworks/strata-readiness-test:%d", os.Getpid())
	if out, err := exec.Command("docker", "build", "-t", image, dir).CombinedOutput(); err != nil {
		t.Fatalf("image build: %v %s", err, out)
	}
	defer exec.Command("docker", "image", "rm", image).Run()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	st, err := store.Open("file:docker_child_readiness?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "fixture/strata", Name: "model"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("child-node", store.HashToken("child-node"), "child-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "model", Engine: engines.Callable, State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	uuid := fmt.Sprintf("%d-child-readiness", os.Getpid())
	spec := proto.InstanceDesiredState{UUID: uuid, DeploymentID: dep.ID, Engine: engines.Callable, DesiredState: "running", Image: image, APIPort: port, Ports: []int{port}, HostNetwork: true, StartTimeout: 60, ResolvedLaunch: proto.ResolvedLaunch{Argv: []string{"--port", strconv.Itoa(port)}}}
	desired, _ := json.Marshal(spec)
	if _, err := st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstStarting, APIPort: port, DesiredStateJSON: string(desired)}); err != nil {
		t.Fatal(err)
	}
	var reports atomic.Int32
	e := agentengine.NewWithProber(dockerdrv.New(), func(status proto.InstanceStatus) {
		if err := st.UpdateInstanceStatus(status); err != nil {
			t.Errorf("status report: %v", err)
		}
		if status.State == proto.InstRunning {
			reports.Add(1)
		}
	}, nil, t.TempDir(), gpu.NoAccelerator{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.SetDesiredFull(ctx, []proto.InstanceDesiredState{spec})
	go e.RunLivenessMonitor(ctx)
	wait := func(what string, check func() bool) {
		t.Helper()
		deadline := time.Now().Add(45 * time.Second)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timed out: %s", what)
	}
	state := func() string {
		in, _ := st.InstanceByUUID(uuid)
		if in == nil {
			return ""
		}
		return in.State
	}
	wait("initial readiness", func() bool { return state() == proto.InstRunning })
	inst, _ := st.InstanceByUUID(uuid)
	container := inst.ContainerID
	physicalID := func(ref string) string {
		t.Helper()
		out, err := exec.Command("docker", "inspect", "--format", "{{.Id}}", ref).CombinedOutput()
		if err != nil {
			t.Fatalf("inspect %s: %v %s", ref, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	originalID := physicalID(container)
	defer func() {
		e.SetDesiredFull(context.Background(), nil)
		// CI has seen `docker stop` of this container hang past every budget
		// while other containers stop at once; keep the daemon's view of it
		// when that happens.
		defer func() {
			if t.Failed() {
				out, _ := exec.Command("docker", "inspect", "--format", "{{json .State}}", container).CombinedOutput()
				t.Logf("container state: %s", out)
				out, _ = exec.Command("docker", "top", container).CombinedOutput()
				t.Logf("container processes: %s", out)
			}
			exec.Command("docker", "rm", "-f", container).Run()
		}()
		wait("container removed", func() bool { in, _ := st.InstanceByUUID(uuid); return in != nil && in.State == proto.InstStopped })
	}()
	p := proxy.New(storeplane.New(st), childReadinessNetworks{}, childReadinessTunnel{}, nil, 0)
	defer p.Close()
	front := httptest.NewServer(p.AuthenticatedHandler())
	defer front.Close()
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	fixtureState := func() (int, int) {
		resp, err := http.Get(base + "/__test__/state")
		if err != nil {
			return 0, 0
		}
		defer resp.Body.Close()
		var s struct {
			Active int `json:"active"`
			Loads  int `json:"loads"`
		}
		json.NewDecoder(resp.Body).Decode(&s)
		return s.Active, s.Loads
	}
	post := func(path string) {
		resp, err := http.Post(base+path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("fixture %s status=%d", path, resp.StatusCode)
		}
	}
	result := make(chan int, 1)
	requestCtx, requestCancel := context.WithCancel(ctx)
	defer requestCancel()
	go func() {
		req, _ := http.NewRequestWithContext(requestCtx, "POST", front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"long"}],"stream":true}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			result <- 0
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		result <- resp.StatusCode
	}()
	wait("upstream waiting", func() bool { active, _ := fixtureState(); return active == 1 })
	post("/__test__/crash")
	wait("recovery reported", func() bool { return state() == proto.InstStarting })
	select {
	case status := <-result:
		if status != 503 {
			t.Fatalf("waiting request status=%d", status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old request kept waiting after readiness loss")
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[]}`))
	rec := httptest.NewRecorder()
	p.AuthenticatedHandler()(rec, req)
	if rec.Code != 503 {
		t.Fatalf("new recovery request status=%d", rec.Code)
	}
	wait("one controlled load", func() bool { _, loads := fixtureState(); return loads == 1 })
	post("/__test__/ready")
	wait("recovered readiness", func() bool { return state() == proto.InstRunning && reports.Load() == 2 })
	inst, _ = st.InstanceByUUID(uuid)
	if physicalID(inst.ContainerID) != originalID {
		t.Fatalf("container replaced: %s -> %s", container, inst.ContainerID)
	}
	_, loads := fixtureState()
	if loads != 1 {
		t.Fatalf("load calls=%d", loads)
	}
}
