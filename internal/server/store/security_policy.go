package store

import "encoding/json"

// SecurityPolicy holds the platform-wide security switches an administrator
// sets. Every field defaults to the restrictive choice, so a missing row is
// the safe policy.
type SecurityPolicy struct {
	// PlaygroundPreviewNetwork lets the playground's HTML preview make network
	// requests: fetch/XHR/WebSocket and plain-http subresources. The preview
	// runs model output inside the user's browser, which can reach the
	// intranet, so it stays off unless an administrator turns it on. The
	// preview sandbox itself does not change either way (ADR 0037).
	PlaygroundPreviewNetwork bool `json:"playground_preview_network"`
	// InferenceOpenAccess lets the /v1 gateway accept a request that presents
	// no API key. Off by default, so the gateway authenticates every request
	// unless an administrator says otherwise: how many keys exist is no
	// longer what decides it.
	InferenceOpenAccess bool `json:"inference_open_access"`
}

// SecurityPolicy returns restrictive defaults when the setting is absent,
// corrupt or unreadable. Display and read-modify-write paths must instead use
// ReadSecurityPolicy so storage failures cannot masquerade as saved defaults.
func (s *Store) SecurityPolicy() SecurityPolicy {
	policy, _ := s.ReadSecurityPolicy()
	return policy
}

// InferenceOpenAccess reports whether /v1 accepts a request that presents no
// API key. An error means the setting could not be read; the caller must then
// require a key, never treat the failure as permission to open the gateway.
//
// A missing row and a corrupt value are not errors: both are the default,
// which is closed.
func (s *Store) InferenceOpenAccess() (bool, error) {
	policy, err := s.ReadSecurityPolicy()
	if err != nil {
		return false, err
	}
	return policy.InferenceOpenAccess, nil
}

// ReadSecurityPolicy preserves storage failures for callers that display or
// update the policy. They must not replace an unreadable policy with defaults.
// Missing and corrupt values retain the existing restrictive-default behavior.
func (s *Store) ReadSecurityPolicy() (SecurityPolicy, error) {
	var policy SecurityPolicy
	raw, err := s.LookupSetting(SettingSecurityPolicy)
	if err != nil {
		return policy, err
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &policy); err != nil {
			// A corrupt value is the restrictive policy, not a storage
			// failure: fall back to the zero policy, which is closed.
			policy = SecurityPolicy{}
		}
	}
	return policy, nil
}

// SetSecurityPolicy replaces the policy.
func (s *Store) SetSecurityPolicy(policy SecurityPolicy) error {
	b, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	return s.SetSetting(SettingSecurityPolicy, string(b))
}
