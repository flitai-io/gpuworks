package nerdctl

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/grpcwire"
)

// WithHostRoot resolves the socket relative to the same filesystem as nerdctl.
func (d *Driver) WithHostRoot(root string) *Driver {
	d.hostRoot = root
	return d
}

// nerdctl 1.x and 2.x expose their effective address in the --address flag
// default, after loading nerdctl.toml, rootless defaults and environment overrides.
// This also runs through the kube driver's host wrapper.
func (d *Driver) containerdSocket(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "nerdctl", "--help").Output()
	if err != nil {
		return "", fmt.Errorf("read nerdctl address: %w", err)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--address string ") {
			continue
		}
		_, quoted, ok := strings.Cut(line, "(default ")
		if ok {
			addr, err := strconv.Unquote(strings.TrimSuffix(strings.TrimSpace(quoted), ")"))
			path := strings.TrimPrefix(addr, "unix://")
			if err == nil && filepath.IsAbs(path) {
				return filepath.Join(d.hostRoot, path), nil
			}
		}
	}
	return "", errors.New("cannot resolve nerdctl's configured containerd Unix socket")
}

const containerdImagesService = "/containerd.services.images.v1.Images/"

func (d *Driver) getContainerdImage(ctx context.Context, client *grpcwire.Client, name string) ([]byte, error) {
	var get grpcwire.Message
	return client.UnaryWithHeaders(ctx, containerdImagesService+"Get", get.String(1, name).Encode(), http.Header{"Containerd-Namespace": {d.ns}})
}

// Probe the same endpoint and namespace that will publish the final tag before exporting layers.
func (d *Driver) openImagePublisher(ctx context.Context, ref string) (*grpcwire.Client, error) {
	socket, err := d.containerdSocket(ctx)
	if err != nil {
		return nil, err
	}
	client := grpcwire.Dial(socket, 10*time.Second)
	_, err = d.getContainerdImage(ctx, client, runtime.CanonicalImageRef(ref))
	var status *grpcwire.StatusError
	if err != nil && !(errors.As(err, &status) && status.Code == grpcwire.CodeNotFound) {
		client.Close()
		return nil, err
	}
	return client, nil
}

// containerd.services.images.v1.Images.Create is create-only. Get the staged
// record first on this same socket and namespace, retaining its labels and OCI
// descriptor. The final name can never overwrite another writer's target.
func (d *Driver) publishLocalImage(ctx context.Context, client *grpcwire.Client, stage, ref, configID string) (runtime.ImageInspection, error) {
	response, err := d.getContainerdImage(ctx, client, stage)
	if err != nil {
		return runtime.ImageInspection{}, err
	}
	image, err := renamedContainerdImage(response, runtime.CanonicalImageRef(ref))
	if err != nil {
		return runtime.ImageInspection{}, err
	}
	var create grpcwire.Message
	_, err = client.UnaryWithHeaders(ctx, containerdImagesService+"Create", create.Bytes(1, image).Encode(), http.Header{"Containerd-Namespace": {d.ns}})
	var status *grpcwire.StatusError
	if err != nil && !(errors.As(err, &status) && status.Code == grpcwire.CodeAlreadyExists) {
		return runtime.ImageInspection{}, err
	}
	installed, err := d.InspectImage(ctx, ref)
	if err == nil && installed.ID != configID {
		err = runtime.ErrLocalImageConflict
	}
	return installed, err
}

// GetImageResponse.image is field 1. Copy Image.labels (2) and target (3);
// timestamps and the target name belong to the new record.
func renamedContainerdImage(response []byte, name string) ([]byte, error) {
	var image grpcwire.Message
	image.String(1, name)
	hasTarget := false
	err := grpcwire.Fields(response, func(num, wire int, msg []byte) error {
		if num != 1 || wire != 2 {
			return nil
		}
		return grpcwire.Fields(msg, func(field, typ int, value []byte) error {
			if typ == 2 && (field == 2 || field == 3) {
				image.Bytes(field, value)
				if field == 3 && len(value) > 0 {
					hasTarget = true
				}
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if !hasTarget {
		return nil, errors.New("staged containerd image has no target descriptor")
	}
	return image.Encode(), nil
}
