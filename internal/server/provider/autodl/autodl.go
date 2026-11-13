// Package autodl implements the provider.Driver for AutoDL (autodl.com),
// against its "实例(Pro) API" (instance Pro API). AutoDL instances are
// containers on shared machines (no root daemon by default) reached over an
// SSH proxy, so this driver reports Kind="container" (the udocker path).
package autodl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

// defaultBaseURL is the AutoDL open-API host documented at
// https://www.autodl.com/docs/instance_pro_api/.
const defaultBaseURL = provider.AutoDLEndpoint

// listPath is the instance (Pro) list endpoint.
const (
	listPath     = "/api/v1/dev/instance/pro/list"
	statusPath   = "/api/v1/dev/instance/pro/status"
	powerOnPath  = "/api/v1/dev/instance/pro/power_on"
	powerOffPath = "/api/v1/dev/instance/pro/power_off"
)

func init() { provider.Register(New()) }

// Driver is the AutoDL provider driver.
type Driver struct {
	baseURL string
	http    *http.Client
}

// New returns a Driver with production defaults.
func New() *Driver {
	return &Driver{
		baseURL: defaultBaseURL,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// setBaseURL overrides the API host (used by tests).
func (d *Driver) setBaseURL(u string) { d.baseURL = strings.TrimRight(u, "/") }

// Name implements provider.Driver.
func (d *Driver) Name() string { return "autodl" }

// Capabilities implements provider.Driver.
func (d *Driver) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		CanListInstances: true,
		CanPowerControl:  true, // power_on / power_off endpoints exist
		CanInstallAgent:  true, // via SSH proxy
		CanCreate:        true,
		CanDelete:        true,
		HasPricing:       true,
	}
}

// ---- API wire types -------------------------------------------------------

// apiEnvelope is AutoDL's standard response wrapper. On success code == "Success".
type apiEnvelope struct {
	Code string          `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// listData is the payload of the list endpoint.
type listData struct {
	List        []apiInstance `json:"list"`
	MaxPage     int           `json:"max_page"`
	ResultTotal int           `json:"result_total"`
}

// apiInstance is the documented subset returned by the list endpoint. SSH and
// pricing data are returned by the separate snapshot endpoint.
type apiInstance struct {
	UUID        string `json:"uuid"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	GPUName     string `json:"gpu_name"`      // human GPU model, when present
	GPUSpecUUID string `json:"gpu_spec_uuid"` // fallback GPU identifier
	ReqGPUAmt   int    `json:"req_gpu_amount"`
	RegionName  string `json:"region_name"`
	RegionSign  string `json:"region_sign"`
}

// listRequest is the list endpoint body.
type listRequest struct {
	PageIndex int `json:"page_index"`
	PageSize  int `json:"page_size"`
}

// ---- Driver methods -------------------------------------------------------

// ValidateCredentials does a cheap one-item list call; auth failure → error.
func (d *Driver) ValidateCredentials(ctx context.Context, creds provider.Credentials) error {
	_, _, err := d.listPage(ctx, creds, 1, 1)
	return err
}

// ListInstances returns the account's instances, mapped to provider.Instance.
func (d *Driver) ListInstances(ctx context.Context, creds provider.Credentials) ([]provider.Instance, error) {
	raw, err := d.listAll(ctx, creds, 100)
	if err != nil {
		return nil, err
	}
	out := make([]provider.Instance, 0, len(raw))
	for _, in := range raw {
		instance := mapInstance(in)
		// List responses omit SSH proxy credentials. Enrich active instances
		// from the documented snapshot endpoint so one-click install can use
		// the provider inventory directly.
		if instance.Power == provider.PowerRunning {
			if snapshot, snapshotErr := d.Snapshot(ctx, creds, in.UUID); snapshotErr == nil {
				instance = mergeInstance(instance, snapshot.Instance)
			}
		}
		out = append(out, instance)
	}
	return out, nil
}

func mergeInstance(base, detail provider.Instance) provider.Instance {
	if detail.ExternalID == "" {
		detail.ExternalID = base.ExternalID
	}
	if detail.Name == "" {
		detail.Name = base.Name
	}
	if detail.GPUModel == "" {
		detail.GPUModel = base.GPUModel
	}
	if detail.GPUCount == 0 {
		detail.GPUCount = base.GPUCount
	}
	if detail.Power == provider.PowerUnknown {
		detail.Power = base.Power
	}
	if detail.PricePerHr == 0 {
		detail.PricePerHr = base.PricePerHr
	}
	if detail.Region == "" {
		detail.Region = base.Region
	}
	if detail.Kind == "" {
		detail.Kind = base.Kind
	}
	return detail
}

// PowerState returns the current power state of one instance.
func (d *Driver) PowerState(ctx context.Context, creds provider.Credentials, externalID string) (provider.PowerState, error) {
	externalID = strings.TrimSpace(externalID)
	if externalID == "" {
		return provider.PowerUnknown, fmt.Errorf("autodl: instance id is required")
	}
	var status string
	if err := d.call(ctx, creds, http.MethodGet, statusPath, instanceRequest{InstanceUUID: externalID}, &status); err != nil {
		return provider.PowerUnknown, fmt.Errorf("autodl: status instance %q: %w", externalID, err)
	}
	return mapPower(status), nil
}

func (d *Driver) PowerOn(ctx context.Context, creds provider.Credentials, externalID string) error {
	return d.power(ctx, creds, powerOnPath, externalID)
}

func (d *Driver) PowerOff(ctx context.Context, creds provider.Credentials, externalID string) error {
	return d.power(ctx, creds, powerOffPath, externalID)
}

func (d *Driver) CreateInstance(ctx context.Context, creds provider.Credentials, req provider.CreateInstanceRequest) (provider.Instance, error) {
	return d.createInstance(ctx, creds, req)
}

func (d *Driver) DeleteInstance(ctx context.Context, creds provider.Credentials, externalID string) error {
	return d.deleteInstance(ctx, creds, externalID)
}

func (d *Driver) ResizeInstance(ctx context.Context, creds provider.Credentials, externalID string, req provider.ResizeInstanceRequest) error {
	return provider.ErrUnsupported
}

type powerRequest struct {
	InstanceUUID string `json:"instance_uuid"`
	Payload      string `json:"payload,omitempty"`
	StartCommand string `json:"start_command,omitempty"`
}

func (d *Driver) power(ctx context.Context, creds provider.Credentials, path, externalID string) error {
	payload := ""
	if path == powerOnPath {
		payload = "gpu"
	}
	body, err := json.Marshal(powerRequest{InstanceUUID: externalID, Payload: payload})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, creds.EndpointOr(d.baseURL)+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", creds.APIKey)
	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("autodl: power request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("autodl: authentication failed (status %d)", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("autodl: power request unexpected status %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("autodl: decode power response: %w", err)
	}
	if env.Code != "" && env.Code != "Success" {
		return fmt.Errorf("autodl: power request failed code=%s msg=%s", env.Code, env.Msg)
	}
	return nil
}

// InstallAgent injects and starts the agent over the AutoDL SSH proxy.
func (d *Driver) InstallAgent(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) error {
	_, err := d.InstallAgentWithReport(ctx, creds, req)
	return err
}

// InstallAgentWithReport preserves the provider's curl-to-file transport while
// also checking that the started agent exposes the requested server URL.
func (d *Driver) InstallAgentWithReport(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) (provider.InstallReport, error) {
	host := req.Instance.PublicIP
	port := req.Instance.SSHPort
	user := req.Instance.SSHUser
	if user == "" {
		user = "root"
	}
	if host == "" || port == 0 {
		return provider.InstallReport{}, fmt.Errorf("autodl: instance %q missing SSH proxy host/port", req.Instance.ExternalID)
	}

	auth := req.SSHAuth()
	if err := auth.Validate(); err != nil {
		return provider.InstallReport{}, fmt.Errorf("autodl: %w", err)
	}
	// The proxy host key rotates per instance, so it is not pinned.
	client, outcome, err := provision.DialSSH(ctx, user, host, port, auth, 15*time.Second, nil)
	if err != nil {
		return provider.InstallReport{}, provider.RetryableSSHConnectionError("autodl: ssh "+net.JoinHostPort(host, strconv.Itoa(port)), err)
	}
	defer client.Close()
	keyCheck, authorized := provision.AuthorizePlatformKey(ctx, client, auth, outcome)
	report := provider.InstallReport{AuthUsed: outcome.Used, PlatformKeyAuthorized: authorized, InstallDir: req.InstallDir}
	for _, check := range []provision.CheckResult{outcome.Check(), keyCheck} {
		report.Checks = append(report.Checks, provider.InstallCheck{Name: check.Name, Status: check.Status, Detail: check.Detail})
	}

	sess, err := client.NewSession()
	if err != nil {
		return provider.InstallReport{}, provider.RetryableSSHConnectionError("autodl: ssh session", err)
	}
	defer sess.Close()

	cmd := installScriptCommand(req.ServerURL, req.Token, req.ConnectMode, req.ServerDialListen, req.ScriptOptions())
	out, err := sess.CombinedOutput(cmd)
	if err != nil {
		return provider.InstallReport{}, fmt.Errorf("autodl: install agent: %w: %s", err, strings.TrimSpace(string(out)))
	}
	report.Checks = append(report.Checks, agentProcessCheck(string(out), req.InstallDir))
	return report, nil
}

// InstancePassword is the root password AutoDL issued for the instance.
func (d *Driver) InstancePassword(ctx context.Context, creds provider.Credentials, externalID string) (string, error) {
	snapshot, err := d.Snapshot(ctx, creds, externalID)
	if err != nil {
		return "", err
	}
	return snapshot.RootPassword, nil
}

// installScriptCommand is the shared script install run over the SSH proxy,
// with what this provider's images need in front of it, followed by the
// check that the agent runs.
func installScriptCommand(serverURL, token, mode, listen string, opts provision.ScriptOptions) string {
	return autodlRuntimePathPrefix() + loopbackNoProxyPrefix(serverURL) + "\n" + provision.ScriptInstallSteps(serverURL, token, providerInstallConnectMode(mode), providerInstallListen(listen), opts) + "\n" + provision.AgentRunningCheckCommand(opts.InstallDir)
}

func autodlRuntimePathPrefix() string {
	return `for d in /root/miniconda3/bin /opt/conda/bin; do if [ -x "$d/python3" ]; then PATH="$d:$PATH"; break; fi; done; export PATH; `
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

// ---- helpers --------------------------------------------------------------

// listPage performs one POST to the paginated list endpoint.
func (d *Driver) listPage(ctx context.Context, creds provider.Credentials, page, size int) ([]apiInstance, int, error) {
	body, _ := json.Marshal(listRequest{PageIndex: page, PageSize: size})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, creds.EndpointOr(d.baseURL)+listPath, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", creds.APIKey)

	resp, err := d.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("autodl: list request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, 0, fmt.Errorf("autodl: authentication failed (status %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("autodl: list unexpected status %d", resp.StatusCode)
	}

	var env apiEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, 0, fmt.Errorf("autodl: decode response: %w", err)
	}
	if env.Code != "" && env.Code != "Success" {
		// AutoDL signals auth/other failures via a non-Success code.
		return nil, 0, fmt.Errorf("autodl: api error code=%s msg=%s", env.Code, env.Msg)
	}
	var data listData
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &data); err != nil {
			return nil, 0, fmt.Errorf("autodl: decode data: %w", err)
		}
	}
	return data.List, data.MaxPage, nil
}

func (d *Driver) listAll(ctx context.Context, creds provider.Credentials, size int) ([]apiInstance, error) {
	first, maxPage, err := d.listPage(ctx, creds, 1, size)
	if err != nil {
		return nil, err
	}
	all := append([]apiInstance(nil), first...)
	for page := 2; page <= maxPage; page++ {
		rows, _, err := d.listPage(ctx, creds, page, size)
		if err != nil {
			return nil, err
		}
		all = append(all, rows...)
	}
	return all, nil
}

// mapInstance converts an AutoDL instance to provider.Instance.
func mapInstance(in apiInstance) provider.Instance {
	gpu := in.GPUName
	if gpu == "" {
		gpu = in.GPUSpecUUID
	}
	region := in.RegionName
	if region == "" {
		region = in.RegionSign
	}
	return provider.Instance{
		ExternalID: in.UUID,
		Name:       in.Name,
		GPUModel:   gpu,
		GPUCount:   in.ReqGPUAmt,
		Power:      mapPower(in.Status),
		Region:     region,
		Kind:       "container",
	}
}

// mapPower maps AutoDL status strings to provider.PowerState.
func mapPower(status string) provider.PowerState {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running":
		return provider.PowerRunning
	case "shutdown", "stopped":
		return provider.PowerStopped
	default:
		return provider.PowerUnknown
	}
}

// parseSSHCommand extracts host, port, user from an "ssh -p PORT user@host" string.
func parseSSHCommand(s string) (host string, port int, user string) {
	fields := strings.Fields(s)
	for i := 0; i < len(fields); i++ {
		switch {
		case fields[i] == "-p" && i+1 < len(fields):
			port, _ = strconv.Atoi(fields[i+1])
		case strings.Contains(fields[i], "@"):
			at := strings.SplitN(fields[i], "@", 2)
			user, host = at[0], at[1]
		}
	}
	return host, port, user
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
