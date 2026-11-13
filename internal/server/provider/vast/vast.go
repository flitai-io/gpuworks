// Package vast implements the vast.ai cloud-provider driver.
//
// vast.ai exposes a REST API (base https://console.vast.ai/api/v0) authenticated
// with a bearer API key. vast.ai instances are docker containers, so agent
// installation uses the udocker path (Kind="container").
package vast

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

const defaultBaseURL = provider.VastEndpoint

func init() {
	provider.Register(New())
}

// Driver is the vast.ai provider driver.
type Driver struct {
	baseURL string
	http    *http.Client
	// keyAttachWait replaces keyAttachWait in tests.
	keyAttachWait time.Duration
}

// New returns a vast.ai driver pointed at the real API.
func New() *Driver {
	return &Driver{
		baseURL: defaultBaseURL,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Name implements provider.Driver.
func (d *Driver) Name() string { return "vast" }

// Capabilities implements provider.Driver.
func (d *Driver) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		CanListInstances: true,
		CanPowerControl:  false,
		CanInstallAgent:  true,
		CanCreate:        true,
		CanDelete:        true,
		HasPricing:       true,
	}
}

// Offer is a rentable vast.ai machine returned by the offer search API.
type Offer struct {
	ID          json.Number `json:"id"`
	GPUName     string      `json:"gpu_name"`
	NumGPUs     int         `json:"num_gpus"`
	GPURAMMB    int         `json:"gpu_ram"`
	DPHTotal    float64     `json:"dph_total"`
	Geolocation string      `json:"geolocation"`
	Reliability float64     `json:"reliability2"`
}

// SearchOffersRequest bounds an on-demand offer search. SearchOffers always
// restricts results to verified, currently rentable, non-external machines.
type SearchOffersRequest struct {
	GPUCount       int
	MinGPURAMMB    int
	MinReliability float64
	MinInetDown    float64
	DiskGB         int
	Limit          int
}

// AttachSSHKey explicitly associates a public key with an existing contract.
// Vast normally does this as part of runtype=ssh creation, but exposing the
// operation lets acceptance tooling repair a delayed or image-specific key
// injection without guessing at private provider endpoints.
func (d *Driver) AttachSSHKey(ctx context.Context, creds provider.Credentials, externalID, publicKey string) error {
	externalID = strings.TrimSpace(externalID)
	publicKey = strings.TrimSpace(publicKey)
	if externalID == "" || publicKey == "" {
		return errors.New("vast: instance id and SSH public key are required")
	}
	if _, err := strconv.ParseUint(externalID, 10, 64); err != nil {
		return fmt.Errorf("vast: invalid instance id %q", externalID)
	}
	resp, err := d.doJSON(ctx, creds, http.MethodPost, "/instances/"+url.PathEscape(externalID)+"/ssh/", map[string]string{"ssh_key": publicKey})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusConflict {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("vast: attach SSH key: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var result struct {
		Success bool   `json:"success"`
		Message string `json:"msg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("vast: decode attach SSH key response: %w", err)
	}
	if !result.Success && !strings.Contains(strings.ToLower(result.Message), "already associated") {
		return fmt.Errorf("vast: attach SSH key failed: %s", firstNonEmpty(result.Message, "provider rejected request"))
	}
	return nil
}

// instance is the tolerant on-the-wire shape of a vast.ai instance. All fields
// are optional; missing fields decode to their zero values.
type instance struct {
	ID           json.Number     `json:"id"`
	MachineID    json.Number     `json:"machine_id"`
	Label        string          `json:"label"`
	GPUName      string          `json:"gpu_name"`
	NumGPUs      int             `json:"num_gpus"`
	DPHTotal     float64         `json:"dph_total"`
	ActualStatus string          `json:"actual_status"`
	CurState     string          `json:"cur_state"`
	PublicIPAddr string          `json:"public_ipaddr"`
	SSHHost      string          `json:"ssh_host"`
	SSHPort      int             `json:"ssh_port"`
	Ports        json.RawMessage `json:"ports"`
	Geolocation  string          `json:"geolocation"`
}

// status returns the best available status string.
func (in *instance) status() string {
	if in.ActualStatus != "" {
		return in.ActualStatus
	}
	return in.CurState
}

func (in *instance) power() provider.PowerState {
	switch strings.ToLower(in.status()) {
	case "running":
		return provider.PowerRunning
	case "stopped", "exited":
		return provider.PowerStopped
	default:
		return provider.PowerUnknown
	}
}

// name resolves the display name from the label, falling back to the GPU name.
func (in *instance) name() string {
	if in.Label != "" {
		return in.Label
	}
	return in.GPUName
}

// doJSON performs an authenticated request against the vast.ai API.
func (d *Driver) doJSON(ctx context.Context, creds provider.Credentials, method, path string, body any) (*http.Response, error) {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("vast: encode request: %w", err)
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, creds.EndpointOr(d.baseURL)+path, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+creds.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return d.http.Do(req)
}

func (d *Driver) do(ctx context.Context, creds provider.Credentials, method, path string) (*http.Response, error) {
	return d.doJSON(ctx, creds, method, path, nil)
}

// SearchOffers returns verified on-demand offers satisfying the requested
// GPU, VRAM, and disk bounds. Callers remain responsible for an explicit
// price ceiling before creating an instance.
func (d *Driver) SearchOffers(ctx context.Context, creds provider.Credentials, req SearchOffersRequest) ([]Offer, error) {
	gpuCount := req.GPUCount
	if gpuCount <= 0 {
		gpuCount = 1
	}
	diskGB := req.DiskGB
	if diskGB <= 0 {
		diskGB = 20
	}
	limit := req.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query := map[string]any{
		"verified":          map[string]any{"eq": true},
		"external":          map[string]any{"eq": false},
		"rentable":          map[string]any{"eq": true},
		"rented":            map[string]any{"eq": false},
		"num_gpus":          map[string]any{"eq": gpuCount},
		"limit":             limit,
		"allocated_storage": diskGB,
		"type":              "on-demand",
	}
	if req.MinGPURAMMB > 0 {
		query["gpu_ram"] = map[string]any{"gte": req.MinGPURAMMB}
	}
	if req.MinReliability > 0 {
		query["reliability2"] = map[string]any{"gte": req.MinReliability}
	}
	if req.MinInetDown > 0 {
		query["inet_down"] = map[string]any{"gte": req.MinInetDown}
	}
	resp, err := d.doJSON(ctx, creds, http.MethodPost, "/bundles/", query)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("vast: search offers: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var result struct {
		Offers []Offer `json:"offers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("vast: decode offers: %w", err)
	}
	return result.Offers, nil
}

// ValidateCredentials implements provider.Driver.
func (d *Driver) ValidateCredentials(ctx context.Context, creds provider.Credentials) error {
	resp, err := d.do(ctx, creds, http.MethodGet, "/instances/")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("vast: invalid credentials (status %d)", resp.StatusCode)
	default:
		return fmt.Errorf("vast: validate credentials: unexpected status %d", resp.StatusCode)
	}
}

// ListInstances implements provider.Driver.
func (d *Driver) ListInstances(ctx context.Context, creds provider.Credentials) ([]provider.Instance, error) {
	resp, err := d.do(ctx, creds, http.MethodGet, "/instances/")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("vast: list instances: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	insts, err := decodeInstances(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("vast: decode instances: %w", err)
	}
	out := make([]provider.Instance, 0, len(insts))
	for i := range insts {
		out = append(out, insts[i].toInstance())
	}
	return out, nil
}

// decodeInstances tolerates either a {"instances":[...]} envelope or a bare
// array. Any other shape is an error rather than an empty list: sync removes
// rows for instances it does not list.
func decodeInstances(r io.Reader) ([]instance, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) > 0 && data[0] == '[' {
		var insts []instance
		if err := json.Unmarshal(data, &insts); err != nil {
			return nil, err
		}
		return insts, nil
	}
	var env struct {
		Instances *[]instance `json:"instances"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	if env.Instances == nil {
		return nil, errors.New("response has neither an instance array nor an instances envelope")
	}
	return *env.Instances, nil
}

// PowerState implements provider.Driver.
func (d *Driver) PowerState(ctx context.Context, creds provider.Credentials, externalID string) (provider.PowerState, error) {
	// vast.ai has no cheap single-instance GET, so list and match by ID.
	resp, err := d.do(ctx, creds, http.MethodGet, "/instances/")
	if err != nil {
		return provider.PowerUnknown, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return provider.PowerUnknown, fmt.Errorf("vast: power state: status %d", resp.StatusCode)
	}
	insts, err := decodeInstances(resp.Body)
	if err != nil {
		return provider.PowerUnknown, fmt.Errorf("vast: decode instances: %w", err)
	}
	for i := range insts {
		if insts[i].ID.String() == externalID {
			return insts[i].power(), nil
		}
	}
	return provider.PowerUnknown, fmt.Errorf("vast: instance %q not found", externalID)
}

func (d *Driver) PowerOn(ctx context.Context, creds provider.Credentials, externalID string) error {
	return provider.ErrUnsupported
}

func (d *Driver) PowerOff(ctx context.Context, creds provider.Credentials, externalID string) error {
	return provider.ErrUnsupported
}

func (d *Driver) CreateInstance(ctx context.Context, creds provider.Credentials, req provider.CreateInstanceRequest) (provider.Instance, error) {
	offerID := strings.TrimSpace(req.Extra["offer_id"])
	if offerID == "" {
		return provider.Instance{}, errors.New("vast: offer_id is required; search offers and select an explicit price first")
	}
	if _, err := strconv.ParseUint(offerID, 10, 64); err != nil {
		return provider.Instance{}, fmt.Errorf("vast: invalid offer_id %q", offerID)
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "gpuworks"
	}
	image := strings.TrimSpace(req.Image)
	if image == "" {
		image = "vastai/base-image:cuda-12.8.1-auto"
	}
	diskGB := 20
	if raw := strings.TrimSpace(req.Extra["disk_gb"]); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return provider.Instance{}, fmt.Errorf("vast: invalid disk_gb %q", raw)
		}
		diskGB = parsed
	}
	// Vast maps an instance's ports when it is created, never later: map the
	// agent's default port too, so the server can dial an agent installed
	// on it (the API's form of docker's -p, as the vast CLI sends it).
	agentPort := strconv.Itoa(provider.PortOf(provision.DefaultServerDialListen))
	body := map[string]any{
		"client_id": "me", "image": image, "env": map[string]string{"-p " + agentPort + ":" + agentPort: "1"},
		"price": nil, "disk": diskGB, "label": name, "extra": nil,
		"onstart": nil, "image_login": nil, "python_utf8": false,
		"lang_utf8": false, "use_jupyter_lab": false, "jupyter_dir": nil,
		"force": false, "cancel_unavail": true, "template_hash_id": nil,
		"user": nil, "runtype": "ssh",
	}
	resp, err := d.doJSON(ctx, creds, http.MethodPut, "/asks/"+url.PathEscape(offerID)+"/", body)
	if err != nil {
		return provider.Instance{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return provider.Instance{}, fmt.Errorf("vast: create instance: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var result struct {
		Success     bool        `json:"success"`
		NewContract json.Number `json:"new_contract"`
		Message     string      `json:"msg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return provider.Instance{}, fmt.Errorf("vast: decode created instance: %w", err)
	}
	if !result.Success || result.NewContract.String() == "" {
		return provider.Instance{}, fmt.Errorf("vast: create instance failed: %s", firstNonEmpty(result.Message, "provider returned no contract id"))
	}
	return provider.Instance{
		ExternalID: result.NewContract.String(), Name: name, GPUModel: req.GPUModel,
		GPUCount: req.GPUCount, Power: provider.PowerUnknown, Kind: "container",
	}, nil
}

func (d *Driver) DeleteInstance(ctx context.Context, creds provider.Credentials, externalID string) error {
	externalID = strings.TrimSpace(externalID)
	if externalID == "" {
		return errors.New("vast: instance id is required")
	}
	if _, err := strconv.ParseUint(externalID, 10, 64); err != nil {
		return fmt.Errorf("vast: invalid instance id %q", externalID)
	}
	resp, err := d.doJSON(ctx, creds, http.MethodDelete, "/instances/"+url.PathEscape(externalID)+"/", map[string]any{})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("vast: delete instance %q: status %d: %s", externalID, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var result struct {
		Success *bool  `json:"success"`
		Message string `json:"msg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("vast: decode delete response: %w", err)
	}
	if result.Success != nil && !*result.Success {
		return fmt.Errorf("vast: delete instance %q failed: %s", externalID, firstNonEmpty(result.Message, "provider rejected request"))
	}
	return nil
}

func (d *Driver) ResizeInstance(ctx context.Context, creds provider.Credentials, externalID string, req provider.ResizeInstanceRequest) error {
	return provider.ErrUnsupported
}

// InstallAgent implements provider.Driver. It connects over SSH and runs the
// gpuworks install script.
func (d *Driver) InstallAgent(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) error {
	_, err := d.InstallAgentWithReport(ctx, creds, req)
	return err
}

// InstallAgentWithReport keeps the curl-to-file transport but surfaces a
// separate process verification check to the provider install API.
func (d *Driver) InstallAgentWithReport(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) (provider.InstallReport, error) {
	if req.Instance.PublicIP == "" {
		return provider.InstallReport{}, fmt.Errorf("vast: no ssh access for instance %q; install manually", req.Instance.ExternalID)
	}
	if req.ConnectMode == "server_dial" {
		return d.installServerDial(ctx, creds, req)
	}
	user := req.Instance.SSHUser
	if user == "" {
		user = "root"
	}
	port := req.Instance.SSHPort
	if port == 0 {
		port = 22
	}
	auth := req.SSHAuth()
	if err := auth.Validate(); err != nil {
		return provider.InstallReport{}, fmt.Errorf("vast: %w", err)
	}
	// Vast proxy endpoints change host keys between instances; not pinned.
	// The platform key was attached a moment ago: wait for Vast to apply it.
	client, outcome, err := d.dialFresh(ctx, user, req.Instance.PublicIP, port, auth)
	if err != nil {
		return provider.InstallReport{}, provider.RetryableSSHConnectionError("vast: ssh "+net.JoinHostPort(req.Instance.PublicIP, fmt.Sprint(port)), err)
	}
	defer client.Close()
	keyCheck, authorized := provision.AuthorizePlatformKey(ctx, client, auth, outcome)
	report := provider.InstallReport{AuthUsed: outcome.Used, PlatformKeyAuthorized: authorized, InstallDir: req.InstallDir}
	for _, check := range []provision.CheckResult{outcome.Check(), keyCheck} {
		report.Checks = append(report.Checks, provider.InstallCheck{Name: check.Name, Status: check.Status, Detail: check.Detail})
	}

	session, err := client.NewSession()
	if err != nil {
		return provider.InstallReport{}, provider.RetryableSSHConnectionError("vast: ssh session", err)
	}
	defer session.Close()

	cmd := installScriptCommand(req.ServerURL, req.Token, req.ConnectMode, req.ServerDialListen, req.ScriptOptions())
	out, err := session.CombinedOutput(cmd)
	if err != nil {
		return provider.InstallReport{}, fmt.Errorf("vast: install command failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	report.Checks = append(report.Checks, agentProcessCheck(string(out), req.InstallDir))
	return report, nil
}

// AuthorizePublicKey attaches publicKey to the instance; Vast writes it into
// the container's authorized_keys. Before an instance exists there is nothing
// to attach to: Vast keys are per instance, and the install attaches it.
func (d *Driver) AuthorizePublicKey(ctx context.Context, creds provider.Credentials, externalID, publicKey string) (string, error) {
	if strings.TrimSpace(externalID) == "" {
		return "", nil
	}
	if err := d.AttachSSHKey(ctx, creds, externalID, publicKey); err != nil {
		return "", err
	}
	return "attached to the Vast instance", nil
}

// installScriptCommand is the shared script install run over the SSH proxy,
// with what this provider's images need in front of it, followed by the
// check that the agent runs.
func installScriptCommand(serverURL, token, mode, listen string, opts provision.ScriptOptions) string {
	return loopbackNoProxyPrefix(serverURL) + "\n" + provision.ScriptInstallSteps(serverURL, token, providerInstallConnectMode(mode), providerInstallListen(listen), opts) + "\n" + provision.AgentRunningCheckCommand(opts.InstallDir)
}

func loopbackNoProxyPrefix(serverURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(serverURL))
	if err != nil {
		return ""
	}
	host := strings.Trim(strings.TrimSpace(parsed.Hostname()), "[]")
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return ""
	}
	return `export NO_PROXY="${NO_PROXY:+$NO_PROXY,}127.0.0.1,localhost"; export no_proxy="${no_proxy:+$no_proxy,}127.0.0.1,localhost"; `
}

func agentProcessCheck(output, dir string) provider.InstallCheck {
	check := provision.AgentRunningCheck(output, dir)
	return provider.InstallCheck{Name: check.Name, Status: check.Status, Detail: check.Detail}
}

func providerInstallConnectMode(mode string) string {
	if mode == "server_dial" {
		return "server_dial"
	}
	return "agent_dial"
}

func providerInstallListen(listen string) string {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return provision.DefaultServerDialListen
	}
	return listen
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
