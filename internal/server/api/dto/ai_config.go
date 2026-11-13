package dto

// AIConfigView is one AI service as the console lists it. The upstream API key
// is never part of it: it is written once and never read back.
type AIConfigView struct {
	ID        uint   `json:"id"`
	Name      string `json:"name"`
	Preset    string `json:"preset"`
	BaseURL   string `json:"base_url"`
	Model     string `json:"model"`
	IsDefault bool   `json:"is_default"`
	// ProxyMode is management, direct or custom; ProxyURL is the custom proxy
	// with any login masked, present only for custom.
	ProxyMode  string `json:"proxy_mode"`
	ProxyURL   string `json:"proxy_url,omitempty"`
	ImageInput bool   `json:"image_input"`
}

// AIConfigRequest adds an AI service (POST /api/v1/ai/configs). A known preset
// fills base_url and model when they are left empty. An empty api_key is
// allowed: a model served inside the installation often asks for none. An
// empty proxy_mode is management; proxy_url is required for custom and refused
// otherwise. A missing image_input takes the preset's value, false without one.
type AIConfigRequest struct {
	Name       string `json:"name"`
	Preset     string `json:"preset"`
	BaseURL    string `json:"base_url"`
	APIKey     string `json:"api_key"`
	Model      string `json:"model"`
	IsDefault  bool   `json:"is_default"`
	ProxyMode  string `json:"proxy_mode,omitempty"`
	ProxyURL   string `json:"proxy_url,omitempty"`
	ImageInput *bool  `json:"image_input,omitempty"`
}

// AIConfigPatch changes an AI service (PATCH /api/v1/ai/configs/{id}). Only the
// fields present are changed, so a form that leaves the key alone cannot drop
// it. api_key is the one field where an empty string means "keep the stored
// key": an operator editing a name must not have to retype a secret the
// platform never showed them. proxy_url is the same: the console only ever
// saw it masked, so an empty one keeps the stored proxy while the mode stays
// custom. A mode other than custom drops the stored proxy.
type AIConfigPatch struct {
	Name       *string `json:"name,omitempty"`
	Preset     *string `json:"preset,omitempty"`
	BaseURL    *string `json:"base_url,omitempty"`
	APIKey     *string `json:"api_key,omitempty"`
	Model      *string `json:"model,omitempty"`
	IsDefault  *bool   `json:"is_default,omitempty"`
	ProxyMode  *string `json:"proxy_mode,omitempty"`
	ProxyURL   *string `json:"proxy_url,omitempty"`
	ImageInput *bool   `json:"image_input,omitempty"`
}

// AIConfigTest asks whether an AI service answers (POST /api/v1/ai/configs/test),
// about the values a form holds rather than about a stored service: nothing is
// saved, so a wrong address costs nothing to try. ID names the stored service
// an edit dialog started from; with an empty api_key the stored key is used,
// but only while base_url still matches the stored address - a probe must not
// be a way to send a stored key to an address of the caller's choosing.
// The call goes the way proxy_mode says; an empty proxy_url with custom uses
// the stored service's proxy. check_image also sends a small image once the
// text call succeeds, to tell whether the model accepts images.
type AIConfigTest struct {
	ID         uint   `json:"id,omitempty"`
	BaseURL    string `json:"base_url"`
	APIKey     string `json:"api_key,omitempty"`
	Model      string `json:"model"`
	ProxyMode  string `json:"proxy_mode,omitempty"`
	ProxyURL   string `json:"proxy_url,omitempty"`
	CheckImage bool   `json:"check_image,omitempty"`
}

// AIConfigTestResult is what the probe established. Outcome is a stable code
// the client words in its own language; Detail is the endpoint's own short
// answer (never the key) and Status its HTTP status when it spoke at all.
// BaseURL is the address the probe actually called, normalization applied.
type AIConfigTestResult struct {
	Outcome   string `json:"outcome"`
	Status    int    `json:"status,omitempty"`
	Detail    string `json:"detail,omitempty"`
	BaseURL   string `json:"base_url"`
	LatencyMS int64  `json:"latency_ms"`
	// Image is the image check's answer - accepted, rejected or unknown -
	// present only when one was asked for and the text call succeeded;
	// ImageStatus and ImageDetail are the endpoint's words about it.
	Image       string `json:"image,omitempty"`
	ImageStatus int    `json:"image_status,omitempty"`
	ImageDetail string `json:"image_detail,omitempty"`
}

// AIPresetDictionary is GET /api/v1/ai/presets: the providers the console
// offers, in order and ending with custom, with the address, starting model
// and image switch each fills in, and the proxy modes a service may use.
type AIPresetDictionary struct {
	Presets    []AIPreset `json:"presets"`
	ProxyModes []string   `json:"proxy_modes"`
}

// AIPreset is one provider of the dictionary. Name is the stable key the client
// labels in its own language.
type AIPreset struct {
	Name       string `json:"name"`
	BaseURL    string `json:"base_url"`
	Model      string `json:"model"`
	ImageInput bool   `json:"image_input"`
}
