package api

import (
	"net/http"
	"strings"
	"unicode/utf8"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/outbound"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// aiProxyURLMaxRunes bounds a custom proxy URL before it is sealed; the sealed
// form fits the 1024-character column with room to spare.
const aiProxyURLMaxRunes = 512

// aiPresets answers the preset dictionary (GET /api/v1/ai/presets), so the
// console fills a new service from the same table a create fills from.
func (a *apiHandlers) aiPresets(w http.ResponseWriter, r *http.Request) {
	out := dto.AIPresetDictionary{
		Presets:    make([]dto.AIPreset, 0, len(aihelper.PresetList)),
		ProxyModes: make([]string, 0, len(aihelper.ProxyModes)),
	}
	for _, p := range aihelper.PresetList {
		out.Presets = append(out.Presets, dto.AIPreset{Name: p.Name, BaseURL: p.BaseURL, Model: p.Model, ImageInput: p.ImageInput})
	}
	for _, m := range aihelper.ProxyModes {
		out.ProxyModes = append(out.ProxyModes, string(m))
	}
	writeJSON(w, http.StatusOK, out)
}

// aiProxyChoice is a validated proxy choice: the mode, and for custom the
// plaintext URL - empty when the caller asked to keep the stored one.
type aiProxyChoice struct {
	mode string
	url  string
}

// validateAIProxy checks a mode and a URL as a request names them. An empty
// mode is management. keepStored says an empty URL with custom is fine
// because a stored one will be kept; otherwise custom needs a URL. A URL with
// any other mode is refused rather than silently dropped: the caller believes
// it is in use.
func validateAIProxy(w http.ResponseWriter, mode, rawURL string, keepStored bool) (aiProxyChoice, bool) {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		mode = string(aihelper.ProxyManagement)
	}
	if !aihelper.KnownProxyMode(mode) {
		writeFieldInvalid(w, "proxy_mode", "must be management, direct or custom")
		return aiProxyChoice{}, false
	}
	proxyURL := strings.TrimSpace(rawURL)
	if mode != string(aihelper.ProxyCustom) {
		if proxyURL != "" {
			writeFieldInvalid(w, "proxy_url", "is only used with proxy_mode custom")
			return aiProxyChoice{}, false
		}
		return aiProxyChoice{mode: mode}, true
	}
	if proxyURL == "" {
		if keepStored {
			return aiProxyChoice{mode: mode}, true
		}
		writeFieldInvalid(w, "proxy_url", "is required with proxy_mode custom")
		return aiProxyChoice{}, false
	}
	if !utf8.ValidString(proxyURL) || containsControl(proxyURL) || utf8.RuneCountInString(proxyURL) > aiProxyURLMaxRunes {
		writeFieldInvalid(w, "proxy_url", "must be a proxy address of at most 512 characters")
		return aiProxyChoice{}, false
	}
	// The error never repeats the URL: it may carry the proxy's password.
	if err := aihelper.ValidateProxyURL(proxyURL); err != nil {
		writeFieldInvalid(w, "proxy_url", "must be an http, https, socks5 or socks5h proxy such as http://proxy.example.com:3128")
		return aiProxyChoice{}, false
	}
	return aiProxyChoice{mode: mode, url: proxyURL}, true
}

// sealAIProxyURL is the stored form of a custom proxy URL.
func (a *apiHandlers) sealAIProxyURL(w http.ResponseWriter, plain string) (string, bool) {
	sealed, err := store.EncryptAIKey(a.d.AISecretbox, plain)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return "", false
	}
	return sealed, true
}

// aiServiceRoute is the route of a call to a service with this proxy choice;
// the management proxy is read per call.
func (a *apiHandlers) aiServiceRoute(mode, plainURL string) func() (outbound.Route, error) {
	return aihelper.ProxyRoute(aihelper.ProxyMode(mode), plainURL, func() (proto.NetworkConfig, error) {
		return planning.ControlPlaneNetwork(a.d.Store)
	})
}

// aiConfigView is the public form of a stored service: what the console shows,
// and never the key. A custom proxy is shown with its login masked; one that
// cannot be unsealed is shown as absent rather than failing the list.
func (a *apiHandlers) aiConfigView(c *store.AIConfig) dto.AIConfigView {
	view := dto.AIConfigView{
		ID: c.ID, Name: c.Name, Preset: c.Preset, BaseURL: c.BaseURL, Model: c.Model, IsDefault: c.IsDefault,
		ProxyMode: c.ProxyMode, ImageInput: c.ImageInput,
	}
	if view.ProxyMode == "" {
		view.ProxyMode = string(aihelper.ProxyManagement)
	}
	if c.ProxyMode == string(aihelper.ProxyCustom) && c.ProxyURL != "" {
		if plain, err := store.DecryptAIKey(a.d.AISecretbox, c.ProxyURL); err == nil {
			view.ProxyURL = proto.MaskProxyCredentials(plain)
		}
	}
	return view
}
