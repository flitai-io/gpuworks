// Package deviceplugin is a kubelet device plugin (k8s.io/kubelet
// pkg/apis/deviceplugin/v1beta1) for resources whose every unit gives a
// container the same device nodes. The kube runtime registers one per card
// (gpuworks.ai/accelerator-<index>), so an instance pod names the exact cards it
// runs on, and the kubelet hands it their device nodes: Kubernetes has no
// other way to give a pod chosen device nodes without making it privileged.
// It speaks the API through grpcwire.
package deviceplugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/grpcwire"
)

// DefaultDir is the kubelet's device plugin directory.
const DefaultDir = "/var/lib/kubelet/device-plugins"

const (
	apiVersion   = "v1beta1"
	kubeletSock  = "kubelet.sock"
	registerPath = "/v1beta1.Registration/Register"
	servicePath  = "/v1beta1.DevicePlugin/"
)

// DeviceSpec is a device node a container is given.
type DeviceSpec struct {
	ContainerPath, HostPath, Permissions string
}

// Resource is one extended resource: Units interchangeable devices, each
// giving a container Devices.
type Resource struct {
	Name    string
	Units   int
	Devices []DeviceSpec
}

// socketName is the resource's socket in the kubelet's directory.
func (r Resource) socketName() string {
	return "gpuworks-" + strings.NewReplacer("/", "-", ".", "-").Replace(r.Name) + ".sock"
}

func (r Resource) deviceID(i int) string {
	return r.Name[strings.LastIndex(r.Name, "/")+1:] + "-" + fmt.Sprint(i)
}

// Plugin serves resources to the kubelet.
type Plugin struct {
	Dir string
	Log *slog.Logger
	// Poll is how often the kubelet's socket is checked for a restart.
	Poll time.Duration

	mu        sync.Mutex
	resources []Resource
	stops     []func()
	kubelet   uint64 // inode of the kubelet socket registered with
}

// Run registers resources with the kubelet and serves them until ctx ends.
// A kubelet restart removes every plugin socket and forgets its plugins;
// Run notices and registers again.
func (p *Plugin) Run(ctx context.Context, resources []Resource) {
	if p.Dir == "" {
		p.Dir = DefaultDir
	}
	if p.Log == nil {
		p.Log = slog.Default()
	}
	if p.Poll <= 0 {
		p.Poll = 5 * time.Second
	}
	p.mu.Lock()
	p.resources = resources
	p.mu.Unlock()
	defer p.stopAll()
	for {
		if p.needsRegistration() {
			if err := p.registerAll(ctx); err != nil {
				p.Log.Warn("device plugin registration failed; retrying", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.Poll):
		}
	}
}

func inode(path string) uint64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}

// needsRegistration: never registered, the kubelet restarted (its socket is
// new), or one of the plugin's sockets is gone.
func (p *Plugin) needsRegistration() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	current := inode(filepath.Join(p.Dir, kubeletSock))
	if current == 0 {
		return false // no kubelet to register with yet
	}
	if current != p.kubelet || len(p.stops) == 0 {
		return true
	}
	for _, r := range p.resources {
		if _, err := os.Stat(filepath.Join(p.Dir, r.socketName())); err != nil {
			return true
		}
	}
	return false
}

func (p *Plugin) stopAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, stop := range p.stops {
		stop()
	}
	p.stops = nil
}

func (p *Plugin) registerAll(ctx context.Context) error {
	p.stopAll()
	p.mu.Lock()
	resources := append([]Resource(nil), p.resources...)
	p.mu.Unlock()
	kubelet := inode(filepath.Join(p.Dir, kubeletSock))
	var stops []func()
	for _, r := range resources {
		sock := filepath.Join(p.Dir, r.socketName())
		_ = os.Remove(sock)
		stop, err := grpcwire.Listen(ctx, sock, p.handler(r))
		if err != nil {
			for _, s := range stops {
				s()
			}
			return fmt.Errorf("serve %s: %w", r.Name, err)
		}
		stops = append(stops, stop)
	}
	client := grpcwire.Dial(filepath.Join(p.Dir, kubeletSock), 10*time.Second)
	for _, r := range resources {
		var req grpcwire.Message
		req.String(1, apiVersion).String(2, r.socketName()).String(3, r.Name).Bytes(4, []byte{})
		if _, err := client.Unary(ctx, registerPath, req.Encode()); err != nil {
			for _, s := range stops {
				s()
			}
			return fmt.Errorf("register %s: %w", r.Name, err)
		}
	}
	p.mu.Lock()
	p.stops, p.kubelet = stops, kubelet
	p.mu.Unlock()
	names := make([]string, 0, len(resources))
	for _, r := range resources {
		names = append(names, r.Name)
	}
	p.Log.Info("device plugin registered with the kubelet", "resources", names)
	return nil
}

func (p *Plugin) handler(r Resource) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		method, ok := strings.CutPrefix(req.URL.Path, servicePath)
		if !ok {
			grpcwire.Fail(w, grpcwire.CodeUnimplemented, "unknown service")
			return
		}
		body, err := grpcwire.ReadFrame(req.Body)
		if err != nil && !errors.Is(err, io.EOF) {
			grpcwire.Fail(w, grpcwire.CodeInvalidArgument, err.Error())
			return
		}
		switch method {
		case "GetDevicePluginOptions":
			unary(w, []byte{}) // pre-start and preferred allocation both off
		case "ListAndWatch":
			grpcwire.StartResponse(w)
			if err := grpcwire.Send(w, listResponse(r)); err != nil {
				return
			}
			// The devices never change; the stream stays open until the
			// kubelet or the plugin goes away.
			<-req.Context().Done()
			grpcwire.Finish(w, 0, "")
		case "Allocate":
			resp, err := allocate(r, body)
			if err != nil {
				grpcwire.Fail(w, grpcwire.CodeInvalidArgument, err.Error())
				return
			}
			unary(w, resp)
		case "PreStartContainer", "GetPreferredAllocation":
			unary(w, []byte{})
		default:
			grpcwire.Fail(w, grpcwire.CodeUnimplemented, "unknown method "+method)
		}
	})
}

func unary(w http.ResponseWriter, msg []byte) {
	grpcwire.StartResponse(w)
	_ = grpcwire.Send(w, msg)
	grpcwire.Finish(w, 0, "")
}

// listResponse is ListAndWatchResponse{repeated Device devices = 1}, every
// unit healthy.
func listResponse(r Resource) []byte {
	var m grpcwire.Message
	for i := 0; i < r.Units; i++ {
		var d grpcwire.Message
		d.String(1, r.deviceID(i)).String(2, "Healthy")
		m.Bytes(1, d.Encode())
	}
	return m.Encode()
}

// allocate answers AllocateRequest{repeated ContainerAllocateRequest
// container_requests = 1}: each container is given the resource's device
// nodes once, however many of its units it asked for.
func allocate(r Resource, req []byte) ([]byte, error) {
	valid := map[string]bool{}
	for i := 0; i < r.Units; i++ {
		valid[r.deviceID(i)] = true
	}
	var resp grpcwire.Message
	err := grpcwire.Fields(req, func(num, wire int, container []byte) error {
		if num != 1 || wire != 2 {
			return nil
		}
		if err := grpcwire.Fields(container, func(num, wire int, id []byte) error {
			if num == 1 && wire == 2 && !valid[string(id)] {
				return fmt.Errorf("device %q is not one of %s", id, r.Name)
			}
			return nil
		}); err != nil {
			return err
		}
		var c grpcwire.Message
		for _, d := range r.Devices {
			var spec grpcwire.Message
			spec.String(1, d.ContainerPath).String(2, d.HostPath).String(3, d.Permissions)
			c.Bytes(3, spec.Encode())
		}
		resp.Bytes(1, c.Encode())
		return nil
	})
	return resp.Encode(), err
}
