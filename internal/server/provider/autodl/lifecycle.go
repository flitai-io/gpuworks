package autodl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

const (
	createPath   = "/api/v1/dev/instance/pro/create"
	snapshotPath = "/api/v1/dev/instance/pro/snapshot"
	releasePath  = "/api/v1/dev/instance/pro/release"
)

// Snapshot is the install-relevant subset of an AutoDL Pro instance detail.
// Password remains separate from provider.Instance so it is never persisted by
// the generic provider inventory sync.
type Snapshot struct {
	Instance     provider.Instance
	RootPassword string
}

type createRequest struct {
	DataCenterList     []string `json:"data_center_list,omitempty"`
	ReqGPUAmount       int      `json:"req_gpu_amount"`
	ExpandSystemDiskGB int      `json:"expand_system_disk_by_gb"`
	GPUSpecUUID        string   `json:"gpu_spec_uuid"`
	ImageUUID          string   `json:"image_uuid"`
	CUDAFrom           int      `json:"cuda_v_from"`
	InstanceName       string   `json:"instance_name,omitempty"`
	StartCommand       string   `json:"start_command,omitempty"`
}

type instanceRequest struct {
	InstanceUUID string `json:"instance_uuid"`
}

type snapshotData struct {
	UUID                 string `json:"uuid"`
	Name                 string `json:"name"`
	Status               string `json:"status"`
	RegionSign           string `json:"region_sign"`
	PaygPrice            int64  `json:"payg_price"`
	SnapshotGPUAliasName string `json:"snapshot_gpu_alias_name"`
	ReqGPUAmount         int    `json:"req_gpu_amount"`
	SSHCommand           string `json:"ssh_command"`
	ProxyHost            string `json:"proxy_host"`
	RootPassword         string `json:"root_password"`
	SSHPort              int    `json:"ssh_port"`
}

func (d *Driver) createInstance(ctx context.Context, creds provider.Credentials, req provider.CreateInstanceRequest) (provider.Instance, error) {
	gpuCount := req.GPUCount
	if gpuCount <= 0 {
		gpuCount = 1
	}
	if gpuCount > 4 {
		return provider.Instance{}, fmt.Errorf("autodl: GPU count must be between 1 and 4")
	}
	gpuSpec := strings.TrimSpace(req.Extra["gpu_spec_uuid"])
	if gpuSpec == "" {
		gpuSpec = gpuSpecUUID(req.GPUModel)
	}
	if gpuSpec == "" {
		return provider.Instance{}, fmt.Errorf("autodl: gpu_spec_uuid is required for GPU model %q", req.GPUModel)
	}
	imageUUID := strings.TrimSpace(req.Image)
	if imageUUID == "" {
		imageUUID = strings.TrimSpace(req.Extra["image_uuid"])
	}
	if imageUUID == "" {
		return provider.Instance{}, errors.New("autodl: image UUID is required")
	}
	diskGB, err := boundedInt(req.Extra["expand_system_disk_by_gb"], 0, 0, 500)
	if err != nil {
		return provider.Instance{}, fmt.Errorf("autodl: %w", err)
	}
	cudaFrom, err := boundedInt(req.Extra["cuda_v_from"], 113, 1, 999)
	if err != nil {
		return provider.Instance{}, fmt.Errorf("autodl: %w", err)
	}
	dataCenters := splitNonEmpty(req.Region)
	if raw := strings.TrimSpace(req.Extra["data_center_list"]); raw != "" {
		dataCenters = splitNonEmpty(raw)
	}
	body := createRequest{
		DataCenterList: dataCenters, ReqGPUAmount: gpuCount, ExpandSystemDiskGB: diskGB,
		GPUSpecUUID: gpuSpec, ImageUUID: imageUUID, CUDAFrom: cudaFrom,
		InstanceName: strings.TrimSpace(req.Name), StartCommand: strings.TrimSpace(req.Extra["start_command"]),
	}
	var externalID string
	if err := d.call(ctx, creds, http.MethodPost, createPath, body, &externalID); err != nil {
		return provider.Instance{}, fmt.Errorf("autodl: create instance: %w", err)
	}
	if strings.TrimSpace(externalID) == "" {
		return provider.Instance{}, errors.New("autodl: create instance returned an empty id")
	}
	return provider.Instance{
		ExternalID: externalID, Name: strings.TrimSpace(req.Name), GPUModel: firstNonEmpty(req.GPUModel, gpuSpec),
		GPUCount: gpuCount, Power: provider.PowerUnknown, Region: strings.Join(dataCenters, ","), Kind: "container",
	}, nil
}

// Snapshot returns current SSH and pricing details for one instance.
func (d *Driver) Snapshot(ctx context.Context, creds provider.Credentials, externalID string) (Snapshot, error) {
	externalID = strings.TrimSpace(externalID)
	if externalID == "" {
		return Snapshot{}, errors.New("autodl: instance id is required")
	}
	var data snapshotData
	if err := d.call(ctx, creds, http.MethodGet, snapshotPath, instanceRequest{InstanceUUID: externalID}, &data); err != nil {
		return Snapshot{}, fmt.Errorf("autodl: snapshot instance %q: %w", externalID, err)
	}
	host, port, user := data.ProxyHost, data.SSHPort, "root"
	if (host == "" || port == 0) && data.SSHCommand != "" {
		parsedHost, parsedPort, parsedUser := parseSSHCommand(data.SSHCommand)
		if host == "" {
			host = parsedHost
		}
		if port == 0 {
			port = parsedPort
		}
		if parsedUser != "" {
			user = parsedUser
		}
	}
	status := data.Status
	if status == "" {
		status = "running"
	}
	inst := provider.Instance{
		ExternalID: externalID, Name: data.Name, GPUModel: data.SnapshotGPUAliasName, GPUCount: data.ReqGPUAmount,
		// AutoDL represents hourly prices as thousandths of CNY in snapshot data.
		Power: mapPower(status), PricePerHr: float64(data.PaygPrice) / 1000, Region: data.RegionSign,
		PublicIP: host, Kind: "container",
	}
	if host != "" && port > 0 {
		inst.SSHConnections = []provider.SSHConnection{{
			ID: "proxy", Type: "proxy", Label: provider.SSHConnectionLabel("proxy"), Host: host, Port: port, User: user, InstallSupported: true,
		}}
		provider.SetPreferredSSHConnection(&inst)
	}
	return Snapshot{Instance: inst, RootPassword: data.RootPassword}, nil
}

func (d *Driver) deleteInstance(ctx context.Context, creds provider.Credentials, externalID string) error {
	externalID = strings.TrimSpace(externalID)
	if externalID == "" {
		return errors.New("autodl: instance id is required")
	}
	state, err := d.PowerState(ctx, creds, externalID)
	if err != nil {
		if errors.Is(err, errAPINotFound) {
			return nil
		}
		return err
	}
	if state != provider.PowerStopped {
		if err := d.PowerOff(ctx, creds, externalID); err != nil {
			return fmt.Errorf("autodl: power off before release: %w", err)
		}
		for {
			select {
			case <-ctx.Done():
				return fmt.Errorf("autodl: wait for shutdown before release: %w", ctx.Err())
			case <-time.After(2 * time.Second):
			}
			state, err = d.PowerState(ctx, creds, externalID)
			if err != nil {
				return err
			}
			if state == provider.PowerStopped {
				break
			}
		}
	}
	if err := d.call(ctx, creds, http.MethodPost, releasePath, instanceRequest{InstanceUUID: externalID}, nil); err != nil {
		return fmt.Errorf("autodl: release instance %q: %w", externalID, err)
	}
	return nil
}

func (d *Driver) call(ctx context.Context, creds provider.Credentials, method, path string, body, result any) error {
	endpoint := creds.EndpointOr(d.baseURL) + path
	var requestBody io.Reader
	if method == http.MethodGet {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return err
		}
		query := parsed.Query()
		switch payload := body.(type) {
		case instanceRequest:
			query.Set("instance_uuid", payload.InstanceUUID)
		default:
			return fmt.Errorf("unsupported GET request payload %T", body)
		}
		parsed.RawQuery = query.Encode()
		endpoint = parsed.String()
	} else {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		requestBody = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, requestBody)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", creds.APIKey)
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("authentication failed (status %d)", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		response, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return classifyAnswer(fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(response))), resp.StatusCode, string(response))
	}
	var env apiEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if env.Code != "" && env.Code != "Success" {
		return classifyAnswer(fmt.Errorf("api error code=%s msg=%s", env.Code, env.Msg), resp.StatusCode, env.Code+" "+env.Msg)
	}
	if result != nil && len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, result); err != nil {
			return fmt.Errorf("decode data: %w", err)
		}
	}
	return nil
}

func gpuSpecUUID(model string) string {
	compact := strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "_", "", "NVIDIA", "", "GEFORCE", "").Replace(strings.TrimSpace(model)))
	switch compact {
	case "H80080G", "H800":
		return "h800"
	case "409048G", "RTX4090", "4090":
		return "v-48g"
	case "PRO600096G", "RTXPRO6000", "PRO6000":
		return "pro6000-p"
	case "4080(S)32G", "RTX4080S", "4080S", "RTX4080", "4080":
		return "v-32g-p"
	case "309048G", "RTX3090", "3090":
		return "v-48g-350w"
	case "509032G", "RTX5090", "5090":
		return "5090-p"
	case "4090D", "RTX4090D":
		return "4090D"
	default:
		return ""
	}
}

func boundedInt(raw string, fallback, minimum, maximum int) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("value %q must be between %d and %d", raw, minimum, maximum)
	}
	return value, nil
}

func splitNonEmpty(raw string) []string {
	var out []string
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
