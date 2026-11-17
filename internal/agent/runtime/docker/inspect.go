package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

func (d *Driver) InspectWorkload(ctx context.Context, id string) (runtime.WorkloadInspection, error) {
	out, err := output(ctx, 10*time.Second, "container", "inspect", "--format", "{{json .}}", id)
	if cliAnsweredNoSuchContainer(err, out) {
		return runtime.WorkloadInspection{}, fmt.Errorf("docker container inspect: %w: %s", runtime.ErrContainerNotFound, strings.TrimSpace(out))
	}
	if cliPermissionDenied(err, out) {
		return runtime.WorkloadInspection{}, fmt.Errorf("docker container inspect: %w: %s", runtime.ErrPermissionDenied, strings.TrimSpace(out))
	}
	if err != nil {
		return runtime.WorkloadInspection{}, err
	}
	return parseWorkloadInspection([]byte(strings.TrimSpace(out)))
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
		return runtime.WorkloadInspection{}, fmt.Errorf("decode docker container inspect: %w", err)
	}
	if strings.TrimSpace(raw.ID) == "" || strings.TrimSpace(raw.Image) == "" {
		return runtime.WorkloadInspection{}, fmt.Errorf("docker container inspect missing immutable identity")
	}
	result := runtime.WorkloadInspection{
		ContainerID: raw.ID, ImageID: raw.Image, Entrypoint: nonNilStrings(raw.Config.Entrypoint),
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
