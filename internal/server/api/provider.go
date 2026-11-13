package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/community"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func readAll(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 1<<20))
}

func (a *apiHandlers) providerKinds(w http.ResponseWriter, r *http.Request) {
	if a.d.Providers == nil {
		writeJSON(w, http.StatusOK, dto.NewList([]provider.KindInfo{}))
		return
	}
	writeJSON(w, http.StatusOK, dto.NewList(a.d.Providers.AvailableKinds()))
}

func (a *apiHandlers) listProviders(w http.ResponseWriter, r *http.Request) {
	ps, err := a.d.Store.ListProviders()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	type providerDTO struct {
		ID   uint   `json:"id"`
		Name string `json:"name"`
		Kind string `json:"kind"`
		// Category is derived from the kind on every read, never stored.
		Category      provider.Category `json:"category"`
		State         string            `json:"state"`
		InstanceCount int               `json:"instance_count"`
		// Resources are what the account's kind connects (provider.ResourcesOf).
		Resources []provider.KindResource `json:"resources"`
		// NodeCount is the compute nodes that joined through the account, on
		// its instances or through its clusters.
		NodeCount int `json:"node_count"`
		// SyncedAt is when the account's instances were last listed; absent before the first sync.
		SyncedAt  string `json:"synced_at,omitempty"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
	}
	// Every account's counts are read in a few grouped queries, however many accounts and instances there are.
	counts, err := a.d.Store.ProviderInstanceCountsByProvider()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	accounts, err := a.d.Store.KubeClusterAccounts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	byVendor, err := a.d.Store.KubeClusterNodeVendors()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// The nodes that joined through the clusters connected from each account.
	clusterNodes := map[uint]int{}
	for id, vendors := range byVendor {
		if account, ok := accounts[id]; ok {
			n, _ := kubeVendorNodes(vendors)
			clusterNodes[account] += n
		}
	}
	out := make([]providerDTO, 0, len(ps))
	for _, p := range ps {
		// A managed cluster's node is the cluster's, not an instance of the
		// account: it is neither listed nor counted as one, and the node on
		// it is counted with its cluster.
		own := counts[p.ID]
		instances, nodes := own.Instances, own.Nodes+clusterNodes[p.ID]
		out = append(out, providerDTO{
			ID:            p.ID,
			Name:          p.Name,
			Kind:          p.Kind,
			Category:      provider.CategoryOf(p.Kind),
			State:         p.State,
			InstanceCount: instances,
			Resources:     provider.ResourcesOf(p.Kind),
			NodeCount:     nodes,
			SyncedAt:      syncedAt(p.SyncedAt),
			CreatedAt:     p.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt:     p.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, dto.NewList(out))
}

// getProvider exposes an explicit edit projection, never the credential object.
func (a *apiHandlers) getProvider(w http.ResponseWriter, r *http.Request) {
	if a.d.Providers == nil {
		writeErr(w, http.StatusServiceUnavailable, errNoProviders)
		return
	}
	p, err := a.d.Store.GetProvider(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	creds, err := a.d.Providers.EditableCredentials(p)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		ID        uint              `json:"id"`
		Name      string            `json:"name"`
		Kind      string            `json:"kind"`
		Endpoint  string            `json:"endpoint"`
		PublicKey string            `json:"public_key"`
		Extra     map[string]string `json:"extra"`
	}{p.ID, p.Name, p.Kind, creds.Endpoint, creds.PublicKey, creds.Extra})
}

func (a *apiHandlers) addProvider(w http.ResponseWriter, r *http.Request) {
	if a.d.Providers == nil {
		writeErr(w, http.StatusServiceUnavailable, errNoProviders)
		return
	}
	var req struct {
		Kind       string            `json:"kind"`
		Name       string            `json:"name"`
		Endpoint   string            `json:"endpoint"`
		APIKey     string            `json:"api_key"`
		PublicKey  string            `json:"public_key"`
		PrivateKey string            `json:"private_key"`
		Extra      map[string]string `json:"extra"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !validateName(w, req.Name) {
		return
	}
	p, err := a.d.Providers.Add(r.Context(), req.Kind, req.Name, provider.Credentials{
		Endpoint: req.Endpoint, APIKey: req.APIKey, PublicKey: req.PublicKey, PrivateKey: req.PrivateKey, Extra: req.Extra,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": p.ID, "kind": p.Kind, "name": p.Name})
}

func (a *apiHandlers) updateProvider(w http.ResponseWriter, r *http.Request) {
	if a.d.Providers == nil {
		writeErr(w, http.StatusServiceUnavailable, errNoProviders)
		return
	}
	id := parseUint(chi.URLParam(r, "id"))
	p, err := a.d.Store.GetProvider(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var req struct {
		Name       string            `json:"name"`
		Endpoint   string            `json:"endpoint"`
		APIKey     string            `json:"api_key"`
		PublicKey  string            `json:"public_key"`
		PrivateKey string            `json:"private_key"`
		Extra      map[string]string `json:"extra"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !validateName(w, req.Name) {
		return
	}
	updated, err := a.d.Providers.Update(r.Context(), id, firstNonEmpty(strings.TrimSpace(req.Name), p.Name), provider.Credentials{
		Endpoint: req.Endpoint, APIKey: req.APIKey, PublicKey: req.PublicKey, PrivateKey: req.PrivateKey, Extra: req.Extra,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": updated.ID, "kind": updated.Kind, "name": updated.Name})
}

func (a *apiHandlers) syncProvider(w http.ResponseWriter, r *http.Request) {
	if a.d.Providers == nil {
		writeErr(w, http.StatusServiceUnavailable, errNoProviders)
		return
	}
	insts, err := a.d.Providers.Sync(r.Context(), parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, statusProviderFailed, err)
		return
	}
	// What a sync found is what the account's instance list shows.
	listed := make([]provider.Instance, 0, len(insts))
	for _, inst := range insts {
		if inst.Cluster == "" {
			listed = append(listed, inst)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(listed), "instances": listed})
}

// providerInstances lists the account's own instances. The nodes of its
// managed Kubernetes clusters are left out: the vendor lists them as
// instances too, but they join through their cluster and are the cluster's.
func (a *apiHandlers) providerInstances(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	pis, err := a.d.Store.ProviderInstances(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var caps provider.Capabilities
	if a.d.Providers != nil {
		if p, err := a.d.Store.GetProvider(id); err == nil {
			caps, _ = a.d.Providers.KindCapabilities(p.Kind)
		}
	}
	type instanceDTO struct {
		ExternalID     string                   `json:"external_id"`
		Name           string                   `json:"name"`
		GPUModel       string                   `json:"gpu_model"`
		GPUCount       int                      `json:"gpu_count"`
		Power          string                   `json:"power"`
		PricePerHr     float64                  `json:"price_per_hr"`
		Region         string                   `json:"region"`
		PublicIP       string                   `json:"public_ip"`
		SSHPort        int                      `json:"ssh_port"`
		SSHUser        string                   `json:"ssh_user"`
		SSHConnections []provider.SSHConnection `json:"ssh_connections"`
		Kind           string                   `json:"kind"`
		NodeID         uint                     `json:"node_id"`
		// Actions are the operations the instance allows now (power_on |
		// power_off | install | delete). A client offers exactly these.
		Actions []string `json:"actions"`
	}
	out := make([]instanceDTO, 0, len(pis))
	for _, p := range pis {
		if p.ManagedCluster != "" {
			continue
		}
		connections := []provider.SSHConnection{}
		if strings.TrimSpace(p.SSHConnections) != "" {
			_ = json.Unmarshal([]byte(p.SSHConnections), &connections)
		}
		out = append(out, instanceDTO{
			ExternalID: p.ExternalID, Name: p.Name, GPUModel: p.GPUModel, GPUCount: p.GPUCount,
			Power: p.Power, PricePerHr: p.PricePerHr, Region: p.Region, PublicIP: p.PublicIP,
			SSHPort: p.SSHPort, SSHUser: p.SSHUser, SSHConnections: connections, Kind: p.Kind, NodeID: p.NodeID,
			Actions: provider.InstanceActions(caps, p.Power, p.NodeID, p.ManagedCluster),
		})
	}
	writeJSON(w, http.StatusOK, dto.NewList(out))
}

func (a *apiHandlers) createProviderInstance(w http.ResponseWriter, r *http.Request) {
	if a.d.Providers == nil {
		writeErr(w, http.StatusServiceUnavailable, errNoProviders)
		return
	}
	var req provider.CreateInstanceRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !validateName(w, req.Name) {
		return
	}
	inst, err := a.d.Providers.CreateInstance(r.Context(), parseUint(chi.URLParam(r, "id")), req)
	if err != nil {
		writeProviderOpErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, inst)
}

func (a *apiHandlers) deleteProviderInstance(w http.ResponseWriter, r *http.Request) {
	if a.d.Providers == nil {
		writeErr(w, http.StatusServiceUnavailable, errNoProviders)
		return
	}
	err := a.d.Providers.DeleteInstance(r.Context(), parseUint(chi.URLParam(r, "id")), chi.URLParam(r, "externalID"))
	if err != nil {
		writeProviderOpErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (a *apiHandlers) powerProviderInstance(w http.ResponseWriter, r *http.Request) {
	if a.d.Providers == nil {
		writeErr(w, http.StatusServiceUnavailable, errNoProviders)
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var on bool
	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case "on", "power_on", "start", "running":
		on = true
	case "off", "power_off", "stop", "stopped":
		on = false
	default:
		writeProductError(w, http.StatusBadRequest, ErrorProviderActionInvalid, "action must be on or off", nil)
		return
	}
	err := a.d.Providers.Power(r.Context(), parseUint(chi.URLParam(r, "id")), chi.URLParam(r, "externalID"), on)
	if err != nil {
		writeProviderOpErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "accepted"})
}

func (a *apiHandlers) resizeProviderInstance(w http.ResponseWriter, r *http.Request) {
	if a.d.Providers == nil {
		writeErr(w, http.StatusServiceUnavailable, errNoProviders)
		return
	}
	var req provider.ResizeInstanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	err := a.d.Providers.ResizeInstance(r.Context(), parseUint(chi.URLParam(r, "id")), chi.URLParam(r, "externalID"), req)
	if err != nil {
		writeProviderOpErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "accepted"})
}

// statusProviderFailed answers a cloud operation the vendor (its API, or the
// instance over SSH) failed. Not 502: a CDN or tunnel in front of the server
// (Cloudflare does) swaps a 502 or 504 body for its own page, and the body is
// the reason the operator has to act on.
const statusProviderFailed = http.StatusFailedDependency

func writeProviderOpErr(w http.ResponseWriter, err error) {
	var inCluster *provider.InstanceInClusterError
	if errors.As(err, &inCluster) {
		writeErr(w, http.StatusConflict, wrapProductError(ErrorProviderInstanceInCluster, err.Error(), map[string]any{"cluster": inCluster.Cluster}, err))
		return
	}
	if errors.Is(err, provider.ErrUnsupported) {
		writeErr(w, http.StatusNotImplemented, err)
		return
	}
	writeErr(w, statusProviderFailed, err)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ---- settings ----

func (a *apiHandlers) getSettings(w http.ResponseWriter, r *http.Request) {
	m, err := a.d.Store.AllSettings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// Values are JSON strings; return them parsed for convenience.
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		if v == "" {
			continue
		}
		// Settings are persisted as JSON documents. Invalid values indicate a
		// corrupt/obsolete database and are intentionally not translated here;
		// old isolated data can be cleared instead of keeping compatibility code.
		if !json.Valid([]byte(v)) {
			writeProductError(w, http.StatusInternalServerError, ErrorSettingJSONInvalid, fmt.Sprintf("setting %q is not valid JSON", k), map[string]any{"setting": k})
			return
		}
		out[k] = json.RawMessage(v)
	}
	// The telemetry export setting holds encrypted header values; it is read
	// through its own route only.
	delete(out, store.SettingTelemetryExport.String())
	// Branding carries an inline logo; it is read through /branding only.
	delete(out, store.SettingBranding.String())
	// Data retention has its bounds and status beside it on its own route.
	delete(out, store.SettingDataRetention.String())
	out[store.SettingDeploymentPolicy.String()] = a.deploymentPolicyJSON()
	writeJSON(w, http.StatusOK, out)
}

func (a *apiHandlers) putSetting(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if key == "default_model_source" {
		writeProductError(w, http.StatusGone, ErrorSettingRemoved, "default_model_source has been removed; choose model_source explicitly", map[string]any{"setting": key})
		return
	}
	if key == string(store.SettingImageRegistryRewrites) || key == "image_registry_mirrors" || key == "image_registry_presets" {
		// Where images are pulled from is set by pull source rules now.
		writeProductError(w, http.StatusGone, ErrorSettingRemoved, key+" has been removed; configure pull source rules under /api/v1/pull/source-rules", map[string]any{"setting": key, "route": "/api/v1/pull/source-rules"})
		return
	}
	if key == store.SettingNetwork.String() || strings.HasPrefix(key, store.SettingNetworkNodePrefix.String()) {
		// Proxies are entries of the pull settings now, selected by node
		// requirements; URL rewrites became site rules.
		writeProductError(w, http.StatusGone, ErrorSettingRemoved, key+" has been removed; configure proxies under /api/v1/pull/proxies and site rules under /api/v1/pull/source-rules", map[string]any{"setting": key, "route": "/api/v1/pull/proxies"})
		return
	}
	if key == store.SettingTelemetryExport.String() {
		writeProductError(w, http.StatusBadRequest, ErrorSettingHasOwnRoute, "telemetry_export is set through PUT /api/v1/telemetry/export", map[string]any{"setting": key, "route": "/api/v1/telemetry/export"})
		return
	}
	if key == store.SettingBranding.String() {
		writeProductError(w, http.StatusBadRequest, ErrorSettingHasOwnRoute, "branding is set through PATCH /api/v1/branding", map[string]any{"setting": key, "route": "/api/v1/branding"})
		return
	}
	if key == store.SettingDataRetention.String() {
		writeProductError(w, http.StatusBadRequest, ErrorSettingHasOwnRoute, "data_retention is set through PUT /api/v1/data-retention", map[string]any{"setting": key, "route": "/api/v1/data-retention"})
		return
	}
	if key == store.SettingSecurityPolicy.String() {
		// A whole-object replace here would silently clear switches the body
		// does not name — including the one that decides whether /v1 asks for
		// a key — so the partial patch on its own route is the only way in.
		writeProductError(w, http.StatusBadRequest, ErrorSettingHasOwnRoute, "security is set through PATCH /api/v1/security/settings", map[string]any{"setting": key, "route": "/api/v1/security/settings"})
		return
	}
	body, err := readAll(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if key == "image_community_catalog" {
		var cfg community.Catalog
		if err := json.Unmarshal(body, &cfg); err != nil {
			writeProductError(w, http.StatusBadRequest, ErrorCommunityCatalogJSONInvalid, "image_community_catalog must be valid JSON", nil)
			return
		}
		if err := community.ValidateRepos(cfg.Repos); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		// Schema 3 added registry and siblings; a schema 2 document is the
		// same shape without them and stays valid.
		if cfg.SchemaVersion == 0 {
			cfg.SchemaVersion = 3
		}
		if cfg.SchemaVersion != 2 && cfg.SchemaVersion != 3 {
			writeProductError(w, http.StatusBadRequest, ErrorCommunityCatalogSchemaInvalid, "image_community_catalog schema_version must be 3", map[string]any{"required": 3})
			return
		}
		body, err = json.Marshal(cfg)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	if key == store.SettingDeploymentPolicy.String() {
		if body, err = a.deploymentPolicyBody(w, r, body); err != nil {
			return
		}
	}
	if key == store.SettingTerminalPolicy.String() {
		// This route only requires an authenticated user, but these values govern
		// a feature that hands out a root shell on every GPU node: without this
		// check a non-admin could re-enable a terminal an admin switched off, or
		// raise the concurrency limits.
		if !a.requireManagementAdmin(w, r) {
			return
		}
		// Start from the defaults so a partial body keeps documented values
		// instead of silently zeroing a limit.
		cfg := store.DefaultTerminalSettings()
		if err := json.Unmarshal(body, &cfg); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if err := store.ValidateTerminalSettings(cfg); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		body, err = json.Marshal(cfg)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	// The generic settings route carries an arbitrary key from the URL path.
	// This is the one boundary where an untyped key legitimately enters the
	// store; every in-process caller names a store.Setting* constant instead.
	if key == "orphan_cleanup" {
		var enabled bool
		if err := json.Unmarshal(body, &enabled); err != nil {
			writeProductError(w, http.StatusBadRequest, ErrorSettingJSONInvalid, "orphan_cleanup must be a JSON boolean", map[string]any{"setting": key})
			return
		}
		body, err = json.Marshal(enabled)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := a.d.Store.SetSetting(store.SettingKey(key), string(body)); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if key == "orphan_cleanup" {
		a.requestSyncAllNodes()
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

const errNoProviders = staticErr("cloud providers not available")

// syncedAt is an account's last sync time as the list carries it; empty before the first.
func syncedAt(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
