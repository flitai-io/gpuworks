package deviceplugin

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/grpcwire"
)

// fakeKubelet is the kubelet's registration service: it records each
// RegisterRequest {version = 1, endpoint = 2, resource_name = 3}.
type fakeKubelet struct {
	mu      sync.Mutex
	regs    [][3]string
	refuse  bool
	stopped func()
}

func (k *fakeKubelet) start(t *testing.T, dir string) {
	t.Helper()
	_ = os.Remove(filepath.Join(dir, kubeletSock))
	stop, err := grpcwire.Listen(context.Background(), filepath.Join(dir, kubeletSock), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != registerPath || r.Header.Get("Content-Type") != "application/grpc" {
			grpcwire.Fail(w, grpcwire.CodeUnimplemented, "unknown")
			return
		}
		msg, err := grpcwire.ReadFrame(r.Body)
		if err != nil {
			grpcwire.Fail(w, grpcwire.CodeInvalidArgument, err.Error())
			return
		}
		var reg [3]string
		_ = grpcwire.Fields(msg, func(num, wire int, payload []byte) error {
			if wire == 2 && num >= 1 && num <= 3 {
				reg[num-1] = string(payload)
			}
			return nil
		})
		k.mu.Lock()
		refuse := k.refuse
		if !refuse {
			k.regs = append(k.regs, reg)
		}
		k.mu.Unlock()
		if refuse {
			grpcwire.Fail(w, grpcwire.CodeInvalidArgument, "resource name already registered")
			return
		}
		grpcwire.StartResponse(w)
		_ = grpcwire.Send(w, []byte{})
		grpcwire.Finish(w, 0, "")
	}))
	if err != nil {
		t.Fatal(err)
	}
	k.stopped = stop
	t.Cleanup(stop)
}

func (k *fakeKubelet) registrations() [][3]string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([][3]string(nil), k.regs...)
}

func shortDir(t *testing.T) string {
	t.Helper()
	// Unix socket paths are limited to 108 bytes; t.TempDir is too long.
	dir, err := os.MkdirTemp("", "dp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var card5 = Resource{Name: "gpuworks.ai/accelerator-5", Units: 2, Devices: []DeviceSpec{{ContainerPath: "/dev/davinci5", HostPath: "/dev/davinci5", Permissions: "rwm"}}}
var shared = Resource{Name: "gpuworks.ai/accelerator-shared", Units: 3, Devices: []DeviceSpec{
	{ContainerPath: "/dev/davinci_manager", HostPath: "/dev/davinci_manager", Permissions: "rwm"},
	{ContainerPath: "/dev/hisi_hdc", HostPath: "/dev/hisi_hdc", Permissions: "rwm"},
}}

func TestRegisterServeAndReregister(t *testing.T) {
	dir := shortDir(t)
	k := &fakeKubelet{}
	k.start(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &Plugin{Dir: dir, Log: slog.New(slog.DiscardHandler), Poll: 20 * time.Millisecond}
	go p.Run(ctx, []Resource{card5, shared})
	waitFor(t, "registration", func() bool { return len(k.registrations()) == 2 })
	regs := k.registrations()
	slices.SortFunc(regs, func(a, b [3]string) int { return strings.Compare(a[2], b[2]) })
	if regs[0] != [3]string{"v1beta1", "gpuworks-gpuworks-ai-accelerator-5.sock", "gpuworks.ai/accelerator-5"} ||
		regs[1] != [3]string{"v1beta1", "gpuworks-gpuworks-ai-accelerator-shared.sock", "gpuworks.ai/accelerator-shared"} {
		t.Fatalf("registrations = %v", regs)
	}

	// The kubelet's side: options, the device list, an allocation.
	plugin := grpcwire.Dial(filepath.Join(dir, "gpuworks-gpuworks-ai-accelerator-shared.sock"), 5*time.Second)
	if opts, err := plugin.Unary(ctx, servicePath+"GetDevicePluginOptions", nil); err != nil || len(opts) != 0 {
		t.Fatalf("options = %x, %v", opts, err)
	}
	var ids []string
	devices := listOnce(t, filepath.Join(dir, "gpuworks-gpuworks-ai-accelerator-shared.sock"))
	_ = grpcwire.Fields(devices, func(num, wire int, d []byte) error {
		var id, health string
		_ = grpcwire.Fields(d, func(num, wire int, v []byte) error {
			switch num {
			case 1:
				id = string(v)
			case 2:
				health = string(v)
			}
			return nil
		})
		ids = append(ids, id+"/"+health)
		return nil
	})
	if !slices.Equal(ids, []string{"accelerator-shared-0/Healthy", "accelerator-shared-1/Healthy", "accelerator-shared-2/Healthy"}) {
		t.Fatalf("devices = %v", ids)
	}
	var container grpcwire.Message
	container.String(1, "accelerator-shared-0").String(1, "accelerator-shared-2")
	var req grpcwire.Message
	req.Bytes(1, container.Encode())
	resp, err := plugin.Unary(ctx, servicePath+"Allocate", req.Encode())
	if err != nil {
		t.Fatal(err)
	}
	var specs []string
	_ = grpcwire.Fields(resp, func(num, wire int, c []byte) error {
		return grpcwire.Fields(c, func(num, wire int, s []byte) error {
			if num != 3 {
				return nil
			}
			var f [3]string
			_ = grpcwire.Fields(s, func(num, wire int, v []byte) error { f[num-1] = string(v); return nil })
			specs = append(specs, strings.Join(f[:], ":"))
			return nil
		})
	})
	if !slices.Equal(specs, []string{"/dev/davinci_manager:/dev/davinci_manager:rwm", "/dev/hisi_hdc:/dev/hisi_hdc:rwm"}) {
		t.Fatalf("allocated = %v (two units give the nodes once)", specs)
	}
	var bad grpcwire.Message
	bad.Bytes(1, new(grpcwire.Message).String(1, "accelerator-9-0").Encode())
	if _, err := plugin.Unary(ctx, servicePath+"Allocate", bad.Encode()); err == nil || !strings.Contains(err.Error(), "grpc-status 3") {
		t.Fatalf("foreign device = %v", err)
	}
	if _, err := plugin.Unary(ctx, servicePath+"Nope", nil); err == nil || !strings.Contains(err.Error(), "grpc-status 12") {
		t.Fatalf("unknown method = %v", err)
	}

	// A kubelet restart deletes the plugin sockets and comes back with a new
	// socket of its own: the plugin registers again.
	k.stopped()
	for _, name := range []string{"gpuworks-gpuworks-ai-accelerator-5.sock", "gpuworks-gpuworks-ai-accelerator-shared.sock"} {
		_ = os.Remove(filepath.Join(dir, name))
	}
	k.start(t, dir)
	waitFor(t, "re-registration", func() bool { return len(k.registrations()) == 4 })
	if _, err := os.Stat(filepath.Join(dir, "gpuworks-gpuworks-ai-accelerator-5.sock")); err != nil {
		t.Fatalf("socket after re-registration: %v", err)
	}
}

func TestRegistrationRefusedIsRetried(t *testing.T) {
	dir := shortDir(t)
	k := &fakeKubelet{refuse: true}
	k.start(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &Plugin{Dir: dir, Log: slog.New(slog.DiscardHandler), Poll: 20 * time.Millisecond}
	go p.Run(ctx, []Resource{card5})
	time.Sleep(100 * time.Millisecond)
	if len(k.registrations()) != 0 {
		t.Fatal("a refused registration was recorded")
	}
	k.mu.Lock()
	k.refuse = false
	k.mu.Unlock()
	waitFor(t, "registration after refusals", func() bool { return len(k.registrations()) == 1 })
}

func TestNoKubeletNoRegistration(t *testing.T) {
	dir := shortDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	p := &Plugin{Dir: dir, Log: slog.New(slog.DiscardHandler), Poll: 10 * time.Millisecond}
	p.Run(ctx, []Resource{card5})
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("sockets without a kubelet: %v", entries)
	}
}

// listOnce reads the first ListAndWatch response, as the kubelet's stream does.
func listOnce(t *testing.T, sock string) []byte {
	t.Helper()
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	client := &http.Client{Transport: &http.Transport{Protocols: &protocols, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost"+servicePath+"ListAndWatch", bytes.NewReader(grpcwire.Frame(nil)))
	req.Header.Set("Content-Type", "application/grpc")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	msg, err := grpcwire.ReadFrame(resp.Body)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	return msg
}
