package nerdctl

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// Recorded by this driver immediately before run, using the original image
// reference. This survives later retags, but cannot detect a concurrent retag
// between inspection and creation by nerdctl.
const imageTargetLabel = "gpuworks.image-target"

// OCI SHA-2 digests use lowercase hex with the algorithm-defined length.
var imageDigestPattern = regexp.MustCompile(`^(sha256:[0-9a-f]{64}|sha384:[0-9a-f]{96}|sha512:[0-9a-f]{128})$`)

func (d *Driver) InspectWorkload(ctx context.Context, id string) (runtime.WorkloadInspection, error) {
	out, err := d.output(ctx, 10*time.Second, "container", "inspect", "--format", "{{json .}}", id)
	if err != nil {
		return runtime.WorkloadInspection{}, err
	}
	line := strings.TrimSpace(out)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return parseWorkloadInspection([]byte(line))
}

func parseWorkloadInspection(data []byte) (runtime.WorkloadInspection, error) {
	var raw struct {
		ID     string   `json:"Id"`
		Image  string   `json:"Image"`
		Path   string   `json:"Path"`
		Args   []string `json:"Args"`
		Config struct {
			Entrypoint []string `json:"Entrypoint"`
			Cmd        []string `json:"Cmd"`
			Env        []string `json:"Env"`

			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		HostConfig struct {
			Runtime     string `json:"Runtime"`
			NetworkMode string `json:"NetworkMode"`
			Devices     []struct {
				PathOnHost        string `json:"PathOnHost"`
				PathInContainer   string `json:"PathInContainer"`
				CgroupPermissions string `json:"CgroupPermissions"`
			} `json:"Devices"`
			DeviceRequests []json.RawMessage `json:"DeviceRequests"`
		} `json:"HostConfig"`
		Mounts []struct {
			Type        string `json:"Type"`
			Source      string `json:"Source"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		} `json:"Mounts"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return runtime.WorkloadInspection{}, fmt.Errorf("decode nerdctl container inspect: %w", err)
	}
	if strings.TrimSpace(raw.ID) == "" || strings.TrimSpace(raw.Image) == "" {
		return runtime.WorkloadInspection{}, fmt.Errorf("nerdctl container inspect missing container id or image")
	}
	result := runtime.WorkloadInspection{
		ImageLookupRef: recordedImageTarget(raw.Image, raw.Config.Labels[imageTargetLabel]),

		ContainerID: raw.ID, Entrypoint: nonNilStrings(raw.Config.Entrypoint),
		Cmd: nonNilStrings(raw.Config.Cmd), Path: raw.Path, Args: nonNilStrings(raw.Args),
		Env: parseEnv(raw.Config.Env), Mounts: make([]runtime.InspectedMount, 0, len(raw.Mounts)),
		Devices:     make([]string, 0, len(raw.HostConfig.Devices)+len(raw.HostConfig.DeviceRequests)),
		RuntimeName: raw.HostConfig.Runtime, NetworkMode: raw.HostConfig.NetworkMode,
	}
	for _, mount := range raw.Mounts {
		result.Mounts = append(result.Mounts, runtime.InspectedMount{
			Type: mount.Type, Source: mount.Source, Destination: mount.Destination, ReadOnly: !mount.RW,
		})
	}
	for _, device := range raw.HostConfig.Devices {
		result.Devices = append(result.Devices, device.PathOnHost+":"+device.PathInContainer+":"+device.CgroupPermissions)
	}
	for _, request := range raw.HostConfig.DeviceRequests {
		result.Devices = append(result.Devices, string(request))
	}
	return result, nil
}

// recordedImageTarget returns the image target recorded at launch, or "" when
// the container cannot prove one: created before the label existed, or with a
// label that is not a digest or disagrees with an immutable raw.Image. The
// container itself is still observed; only its image identity stays unknown.
// Even an immutable raw.Image is a lookup reference, not necessarily a config
// digest, so it is never reported as ImageID directly.
func recordedImageTarget(image, target string) string {
	if !imageDigestPattern.MatchString(target) {
		return ""
	}
	if imageDigestPattern.MatchString(image) && image != target {
		return ""
	}
	return target
}

func parseEnv(entries []string) map[string]string {
	env := make(map[string]string, len(entries))
	for _, kv := range entries {
		if i := strings.IndexByte(kv, '='); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	return env
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
