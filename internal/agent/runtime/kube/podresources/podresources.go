// Package podresources asks the kubelet which devices it gave to which pod:
// the List call of the kubelet's PodResources API (k8s.io/kubelet
// pkg/apis/podresources/v1), over its Unix socket, through grpcwire.
package podresources

import (
	"context"
	"fmt"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/grpcwire"
)

// DefaultSocket is the kubelet's PodResources socket.
const DefaultSocket = "/var/lib/kubelet/pod-resources/kubelet.sock"

const listPath = "/v1.PodResourcesLister/List"

// Allocation is one device the kubelet gave a pod's container.
type Allocation struct {
	Namespace string
	Pod       string
	Container string
	Resource  string // e.g. nvidia.com/gpu
	DeviceID  string // e.g. GPU-8a0f…, or a device plugin's own ID
}

// Client calls one kubelet.
type Client struct{ c *grpcwire.Client }

// New returns a client for the kubelet socket at path.
func New(path string) *Client { return &Client{c: grpcwire.Dial(path, 10*time.Second)} }

// List returns every device the kubelet has allocated, by pod and container.
func (c *Client) List(ctx context.Context) ([]Allocation, error) {
	msg, err := c.c.Unary(ctx, listPath, nil) // an empty ListPodResourcesRequest
	if err != nil {
		return nil, fmt.Errorf("kubelet pod resources: %w", err)
	}
	return decodeList(msg)
}

// decodeList reads ListPodResourcesResponse{repeated PodResources
// pod_resources = 1}.
func decodeList(b []byte) ([]Allocation, error) {
	var out []Allocation
	err := fields(b, func(num int, payload []byte) error {
		if num != 1 {
			return nil
		}
		return decodePod(payload, &out)
	})
	return out, err
}

// decodePod reads PodResources{string name = 1; string namespace = 2;
// repeated ContainerResources containers = 3}.
func decodePod(b []byte, out *[]Allocation) error {
	var name, ns string
	var containers [][]byte
	if err := fields(b, func(num int, payload []byte) error {
		switch num {
		case 1:
			name = string(payload)
		case 2:
			ns = string(payload)
		case 3:
			containers = append(containers, payload)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, c := range containers {
		var cname string
		var devices [][]byte
		if err := fields(c, func(num int, payload []byte) error {
			switch num {
			case 1:
				cname = string(payload)
			case 2:
				devices = append(devices, payload)
			}
			return nil
		}); err != nil {
			return err
		}
		for _, d := range devices {
			var resource string
			var ids []string
			if err := fields(d, func(num int, payload []byte) error {
				switch num {
				case 1:
					resource = string(payload)
				case 2:
					ids = append(ids, string(payload))
				}
				return nil
			}); err != nil {
				return err
			}
			for _, id := range ids {
				*out = append(*out, Allocation{Namespace: ns, Pod: name, Container: cname, Resource: resource, DeviceID: id})
			}
		}
	}
	return nil
}

// fields hands fn the length-delimited fields of a message.
func fields(b []byte, fn func(num int, payload []byte) error) error {
	return grpcwire.Fields(b, func(num, wire int, payload []byte) error {
		if wire != 2 {
			return nil
		}
		return fn(num, payload)
	})
}
