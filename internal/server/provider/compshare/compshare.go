// Package compshare implements the 优云智算 (compshare, a UCloud product)
// provider driver. compshare exposes a UCloud-style HTTP API gateway whose
// requests are authenticated with a PublicKey + PrivateKey signature.
package compshare

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// defaultBaseURL is the compshare / UCloud POST form API gateway.
const defaultBaseURL = provider.CompShareEndpoint

const listAction = "DescribeCompShareInstance"

const (
	defaultRegion = "cn-wlcb"
	defaultZone   = "cn-wlcb-01"
)

func init() { provider.Register(New()) }

// Driver is the compshare provider driver.
type Driver struct {
	baseURL       string
	hc            *http.Client
	hostKeys      HostKeyStore
	zoneDiscovery bool
}

// New returns a compshare driver. Host keys live in memory until the server
// hands the driver its data dir (UseDataDir).
func New() *Driver {
	return &Driver{
		baseURL:       defaultBaseURL,
		hc:            &http.Client{Timeout: 30 * time.Second},
		hostKeys:      newMemoryHostKeys(),
		zoneDiscovery: true,
	}
}

// UseDataDir persists trusted SSH host keys in the server's data dir. The
// server calls it once at startup, before any instance is installed.
func (d *Driver) UseDataDir(dir string) error {
	file, err := newFileHostKeys(filepath.Join(dir, "compshare-hostkeys.json"))
	if err != nil {
		return err
	}
	d.hostKeys = file
	return nil
}

// setBaseURL overrides the API endpoint (used by tests). A fake endpoint has
// no zone catalog unless the test serves one, so discovery is switched off;
// a test that serves DescribeCompShareSupportZone sets zoneDiscovery again.
func (d *Driver) setBaseURL(u string) {
	d.baseURL = u
	d.zoneDiscovery = false
}

func (d *Driver) Name() string { return "compshare" }

func (d *Driver) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		CanListInstances: true,
		CanPowerControl:  true,
		CanInstallAgent:  true,
		CanCreate:        true,
		CanDelete:        true,
		HasPricing:       true,
	}
}

// sign computes the UCloud signature: sort params by key, concatenate as
// key1value1key2value2..., append the PrivateKey, SHA1 hex.
func sign(params map[string]string, privateKey string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(params[k])
	}
	b.WriteString(privateKey)
	sum := sha1.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// publicPrivate resolves the credential pair, honouring the combined APIKey
// fallback ("public.private" or "public:private").
func publicPrivate(creds provider.Credentials) (pub, priv string) {
	pub, priv = creds.PublicKey, creds.PrivateKey
	if pub == "" || priv == "" {
		if creds.APIKey != "" {
			sep := "."
			if strings.Contains(creds.APIKey, ":") {
				sep = ":"
			}
			if p := strings.SplitN(creds.APIKey, sep, 2); len(p) == 2 {
				pub, priv = p[0], p[1]
			}
		}
	}
	return pub, priv
}

// ucloudResp is the common UCloud response envelope.
type ucloudResp struct {
	RetCode          int                 `json:"RetCode"`
	Message          string              `json:"Message"`
	Action           string              `json:"Action"`
	TotalCount       int                 `json:"TotalCount"`
	UHostSet         []ucloudInstance    `json:"UHostSet"`
	UHostIds         []string            `json:"UHostIds"`
	UHostId          string              `json:"UHostId"`
	Specs            []capacitySpec      `json:"Specs"`
	ZoneInfo         []supportZone       `json:"ZoneInfo"`
	CompShareImageID string              `json:"CompShareImageId"`
	Process          float64             `json:"Process"`
	ImageSet         []customImageRecord `json:"ImageSet"`
	TcpForwards      []tcpForward        `json:"TcpForwards"`
}

type ucloudInstance struct {
	UHostId         string        `json:"UHostId"`
	Name            string        `json:"Name"`
	State           string        `json:"State"`
	Region          string        `json:"Region"`
	Zone            string        `json:"Zone"`
	GPU             int           `json:"GPU"`
	GpuType         string        `json:"GpuType"`
	SshLoginCommand string        `json:"SshLoginCommand"`
	Password        string        `json:"Password"`
	InstanceType    string        `json:"InstanceType"`
	MachineType     string        `json:"MachineType"`
	InstancePrice   float64       `json:"InstancePrice"`
	IPSet           []compshareIP `json:"IPSet"`
	Ports           openPorts     `json:"Ports"`
	TcpForwards     []tcpForward  `json:"TcpForwards"`
}

type supportZone struct {
	Region string `json:"Region"`
	Zone   string `json:"Zone"`
	IsPod  bool   `json:"IsPod"`
}

// SupportedZones returns the zones currently exposed by CompShare.
func (d *Driver) SupportedZones(ctx context.Context, creds provider.Credentials) ([]provider.Zone, error) {
	var resp *ucloudResp
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err = d.call(ctx, creds, map[string]string{"Action": "DescribeCompShareSupportZone"})
		if err == nil || !isCreateTransientError(err) || attempt == 2 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(2*(attempt+1)) * time.Second):
		}
	}
	if err != nil {
		return nil, err
	}
	out := make([]provider.Zone, 0, len(resp.ZoneInfo))
	for _, z := range resp.ZoneInfo {
		out = append(out, provider.Zone{Region: z.Region, Zone: z.Zone, IsPod: z.IsPod})
	}
	return out, nil
}

type compshareIP struct {
	Type string `json:"Type"` // "International" / "Private" etc.
	IP   string `json:"IP"`
}

type capacitySpec struct {
	GPU            int  `json:"Gpu"`
	CPU            int  `json:"Cpu"`
	MemoryGB       int  `json:"Mem"`
	ResourceEnough bool `json:"ResourceEnough"`
}

// call issues a signed request and decodes the UCloud envelope.
func (d *Driver) call(ctx context.Context, creds provider.Credentials, params map[string]string) (*ucloudResp, error) {
	pub, priv := publicPrivate(creds)
	if pub == "" || priv == "" {
		return nil, fmt.Errorf("compshare: missing PublicKey/PrivateKey credentials")
	}
	signedParams := make(map[string]string, len(params)+2)
	for key, value := range params {
		if key != "PublicKey" && key != "Signature" {
			signedParams[key] = value
		}
	}
	signedParams["PublicKey"] = pub
	signedParams["Signature"] = sign(signedParams, priv)

	form := url.Values{}
	for key, value := range signedParams {
		form.Set(key, value)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, creds.EndpointOr(d.baseURL), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := d.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("compshare: request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("compshare: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out ucloudResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("compshare: decode response: %w", err)
	}
	if out.RetCode != 0 {
		return nil, fmt.Errorf("compshare: API RetCode %d: %s", out.RetCode, out.Message)
	}
	return &out, nil
}

func (d *Driver) ValidateCredentials(ctx context.Context, creds provider.Credentials) error {
	_, err := d.call(ctx, creds, map[string]string{
		"Action": listAction,
		"Region": defaultRegion,
		"Limit":  "1",
	})
	return err
}

func (d *Driver) PowerOn(ctx context.Context, creds provider.Credentials, externalID string) error {
	inst, err := d.GetInstance(ctx, creds, externalID)
	if err != nil {
		return err
	}
	_, err = d.call(ctx, creds, map[string]string{"Action": "StartCompShareInstance", "Region": inst.Region, "Zone": inst.Zone, "UHostId": externalID})
	return err
}

func (d *Driver) PowerOff(ctx context.Context, creds provider.Credentials, externalID string) error {
	inst, err := d.GetInstance(ctx, creds, externalID)
	if err != nil {
		return err
	}
	_, err = d.call(ctx, creds, map[string]string{"Action": "StopCompShareInstance", "Region": inst.Region, "Zone": inst.Zone, "UHostId": externalID})
	return err
}

func (d *Driver) CreateInstance(ctx context.Context, creds provider.Credentials, req provider.CreateInstanceRequest) (provider.Instance, error) {
	if strings.TrimSpace(req.GPUModel) == "" {
		return provider.Instance{}, fmt.Errorf("compshare: GPU model is required")
	}
	if strings.TrimSpace(req.Image) == "" {
		return provider.Instance{}, fmt.Errorf("compshare: CompShare image id is required")
	}
	gpuCount := req.GPUCount
	if gpuCount <= 0 {
		gpuCount = 1
	}
	diskGB := positiveIntString(req.Extra["disk_gb"], 100)
	cpu, cpuExplicit := optionalPositiveInt(req.Extra["cpu"])
	memoryMB, memoryExplicit := optionalPositiveInt(req.Extra["memory_mb"])
	region := firstNonEmpty(req.Region, req.Extra["region"], defaultRegion)
	zone := firstNonEmpty(req.Extra["zone"], defaultZone)
	instanceKind := strings.ToLower(firstNonEmpty(req.Extra["instance_kind"], "vm"))
	placements := []provider.Zone{{Region: region, Zone: zone}}
	if d.zoneDiscovery && (req.Region == "" || req.Extra["region"] == "" || req.Extra["zone"] == "") {
		if zones, err := d.SupportedZones(ctx, creds); err == nil {
			wantPod := instanceKind == "pod" || instanceKind == "container"
			placements = placements[:0]
			for _, candidate := range zones {
				if candidate.IsPod != wantPod {
					continue
				}
				if requestedRegion := firstNonEmpty(req.Region, req.Extra["region"]); requestedRegion != "" && candidate.Region != requestedRegion {
					continue
				}
				if requestedZone := req.Extra["zone"]; requestedZone != "" && candidate.Zone != requestedZone {
					continue
				}
				placements = append(placements, candidate)
			}
			if len(placements) == 0 {
				placements = []provider.Zone{{Region: region, Zone: zone}}
			}
		}
	}
	cpuPlatform := firstNonEmpty(req.Extra["cpu_platform"], "Auto")
	params := map[string]string{
		"Action": "CreateCompShareInstance", "Region": region, "Zone": zone,
		"MachineType": "G", "CompShareImageId": strings.TrimSpace(req.Image),
		"GPU": strconv.Itoa(gpuCount), "GpuType": normalizeGPUType(req.GPUModel),
		"MinimalCpuPlatform": cpuPlatform,
		"Disks.0.IsBoot":     "true", "Disks.0.Size": strconv.Itoa(diskGB), "Disks.0.Type": "CLOUD_SSD",
		"ChargeType": firstNonEmpty(req.Extra["charge_type"], "Postpay"),
	}
	if name := strings.TrimSpace(req.Name); name != "" {
		params["Name"] = name
	}
	if password := strings.TrimSpace(req.Extra["password"]); password != "" {
		params["LoginMode"] = "Password"
		params["Password"] = base64.StdEncoding.EncodeToString([]byte(password))
	}
	var placementErrs placementErrors
	for _, placement := range placements {
		region, zone = placement.Region, placement.Zone
		params["Region"], params["Zone"] = region, zone
		capacity, capacityErr := d.call(ctx, creds, capacityParams(resourceCapacityParams{
			gpuModel: params["GpuType"], region: region, zone: zone, image: params["CompShareImageId"],
			diskGB: diskGB, chargeType: params["ChargeType"], cpuPlatform: cpuPlatform,
		}))
		if capacityErr != nil {
			placementErrs = append(placementErrs, fmt.Errorf("compshare: check resource capacity in %s/%s: %w", region, zone, capacityErr))
			continue
		}
		selected, ok := selectCapacitySpec(capacity.Specs, gpuCount, cpu, memoryMB, cpuExplicit, memoryExplicit)
		if !ok {
			placementErrs = append(placementErrs, fmt.Errorf("compshare: requested capacity unavailable in %s/%s: gpu=%s x%d", region, zone, params["GpuType"], gpuCount))
			continue
		}
		params["CPU"] = strconv.Itoa(selected.CPU)
		params["Memory"] = strconv.Itoa(selected.MemoryGB * 1024)
		var resp *ucloudResp
		var createErr error
		for attempt := 0; attempt < 3; attempt++ {
			resp, createErr = d.call(ctx, creds, params)
			if createErr == nil || !isCreateTransientError(createErr) || attempt == 2 {
				break
			}
			backoff := time.Duration(5*(1<<attempt)) * time.Second
			select {
			case <-ctx.Done():
				return provider.Instance{}, ctx.Err()
			case <-time.After(backoff):
			}
		}
		if createErr != nil {
			placementErrs = append(placementErrs, fmt.Errorf("compshare: create in %s/%s: %w", region, zone, createErr))
			continue
		}
		if len(resp.UHostIds) == 0 {
			placementErrs = append(placementErrs, fmt.Errorf("compshare: create in %s/%s returned no instance id", region, zone))
			continue
		}
		kind := "vm"
		if instanceKind == "pod" || instanceKind == "container" {
			kind = "container"
		}
		port := 22
		if kind == "container" {
			port = 0
		}
		return provider.Instance{ExternalID: resp.UHostIds[0], Name: req.Name, GPUModel: normalizeGPUType(req.GPUModel), GPUCount: gpuCount, Power: provider.PowerUnknown, Region: region, Zone: zone, SSHPort: port, SSHUser: "root", Kind: kind}, nil
	}
	if len(placementErrs) == 1 {
		return provider.Instance{}, placementErrs[0]
	}
	if len(placementErrs) > 1 {
		return provider.Instance{}, placementErrs
	}
	return provider.Instance{}, fmt.Errorf("compshare: no supported placement for %s", req.GPUModel)
}

// placementErrors is why every candidate zone refused a create, in the order
// the zones were tried. Reporting only the last zone's refusal hid the others:
// a container whose 4090 capacity sat in an earlier zone looked like it had
// only ever been tried in the last one. Unwrap exposes each cause so callers'
// errors.Is/As checks and substring matches see them all.
type placementErrors []error

func (e placementErrors) Error() string {
	parts := make([]string, len(e))
	for i, err := range e {
		parts[i] = err.Error()
	}
	return fmt.Sprintf("compshare: no placement could create the instance (%d zones tried): %s", len(e), strings.Join(parts, "; "))
}

func (e placementErrors) Unwrap() []error { return e }

func isCreateTransientError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	// 8433 is CompShare's "Service error, please retry" capacity/backend
	// blip: it was observed (2026-09 CI) to make a whole zone's capacity
	// answers vanish for a query round, so it must ride the same retry.
	return strings.Contains(s, "retcode 226645") || strings.Contains(s, "retcode 8433") || strings.Contains(s, "service unavailable") || strings.Contains(s, "http 5")
}

type resourceCapacityParams struct {
	gpuModel, region, zone, image string
	diskGB                        int
	chargeType, cpuPlatform       string
}

func capacityParams(req resourceCapacityParams) map[string]string {
	return map[string]string{
		"Action": "CheckCompShareResourceCapacity", "Region": req.region, "Zone": req.zone,
		"GpuType": normalizeGPUType(req.gpuModel), "MachineType": "G", "MinimalCpuPlatform": req.cpuPlatform,
		"CompShareImageId": req.image, "ChargeType": req.chargeType,
		"Disks.0.IsBoot": "true", "Disks.0.Size": strconv.Itoa(req.diskGB), "Disks.0.Type": "CLOUD_SSD",
	}
}

func (d *Driver) DeleteInstance(ctx context.Context, creds provider.Credentials, externalID string) error {
	externalID = strings.TrimSpace(externalID)
	if externalID == "" {
		return fmt.Errorf("compshare: instance id is required")
	}
	inst, err := d.GetInstance(ctx, creds, externalID)
	if err != nil {
		if isNotFoundError(err) {
			return nil
		}
		return err
	}
	if inst.Power != provider.PowerStopped {
		if _, err := d.call(ctx, creds, map[string]string{"Action": "StopCompShareInstance", "Region": inst.Region, "Zone": inst.Zone, "UHostId": externalID}); err != nil {
			return err
		}
		inst, err = d.GetInstance(ctx, creds, externalID)
		if err != nil {
			return err
		}
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for inst.Power != provider.PowerStopped {
			select {
			case <-ctx.Done():
				return fmt.Errorf("compshare: wait for instance stop: %w", ctx.Err())
			case <-ticker.C:
			}
			inst, err = d.GetInstance(ctx, creds, externalID)
			if err != nil {
				return err
			}
		}
	}
	_, err = d.call(ctx, creds, map[string]string{"Action": "TerminateCompShareInstance", "Region": inst.Region, "Zone": inst.Zone, "UHostId": externalID, "ReleaseUDisk": "true"})
	if isNotFoundError(err) {
		return nil
	}
	return err
}

func positiveIntString(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func optionalPositiveInt(raw string) (int, bool) {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	return value, err == nil && value > 0
}

func selectCapacitySpec(specs []capacitySpec, gpuCount, cpu, memoryMB int, cpuExplicit, memoryExplicit bool) (capacitySpec, bool) {
	candidates := make([]capacitySpec, 0, len(specs))
	for _, candidate := range specs {
		if !candidate.ResourceEnough || candidate.GPU != gpuCount {
			continue
		}
		if cpuExplicit && candidate.CPU != cpu {
			continue
		}
		if memoryExplicit && candidate.MemoryGB*1024 != memoryMB {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		return capacitySpec{}, false
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].CPU != candidates[j].CPU {
			return candidates[i].CPU < candidates[j].CPU
		}
		return candidates[i].MemoryGB < candidates[j].MemoryGB
	})
	return candidates[0], true
}

func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "not found") || strings.Contains(message, "not exist") || strings.Contains(message, "does not exist") || strings.Contains(message, "不存在") || strings.Contains(message, "已删除")
}

func normalizeGPUType(model string) string {
	model = strings.TrimSpace(model)
	upper := strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "_", "").Replace(model))
	switch upper {
	case "RTX4090", "4090", "NVIDIAGEFORCERTX4090":
		return "4090"
	case "RTX3090", "3090", "NVIDIAGEFORCERTX3090":
		return "3090"
	case "RTX3080TI", "3080TI", "NVIDIAGEFORCERTX3080TI":
		return "3080Ti"
	default:
		return model
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (d *Driver) ResizeInstance(ctx context.Context, creds provider.Credentials, externalID string, req provider.ResizeInstanceRequest) error {
	return provider.ErrUnsupported
}
