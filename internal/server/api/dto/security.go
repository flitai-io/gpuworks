package dto

// SecuritySettings are the platform-wide security switches (store.SecurityPolicy).
type SecuritySettings struct {
	// PlaygroundPreviewNetwork lets the playground HTML preview make network
	// requests; off by default.
	PlaygroundPreviewNetwork bool `json:"playground_preview_network"`
	// InferenceOpenAccess lets /v1 accept a request that presents no API key.
	// Off by default: the gateway requires a key unless an administrator
	// explicitly opens it.
	InferenceOpenAccess bool `json:"inference_open_access"`
}
