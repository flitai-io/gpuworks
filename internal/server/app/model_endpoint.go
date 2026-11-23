package app

import (
	"fmt"
	"slices"

	"gpuworks.ai/gpuworks/internal/server/sourceurl"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// ensureModelEndpointRules sends every model download and metadata request
// for Hugging Face and ModelScope to endpoint, as the server's
// --model-endpoint flag asks: one site rule per hub, for every node, that
// never reaches the hub itself, read by the control plane as well. It exists
// for in-process harnesses and for the experience console. Its offline hub
// binds a new loopback port each run, so renew its own saved endpoints.
func ensureModelEndpointRules(st *store.Store, endpoint string, experience bool) error {
	normalized, err := sourceurl.Normalize(endpoint)
	if err != nil {
		return fmt.Errorf("model endpoint: %w", err)
	}
	rules, err := st.PullSourceRules()
	if err != nil {
		return err
	}
	for _, site := range []string{modelrepo.BaseURL(modelrepo.SourceHF, ""), modelrepo.BaseURL(modelrepo.SourceMS, "")} {
		spec := store.PullSourceRuleSpec{
			Match: site, Endpoints: []string{normalized}, AllowUpstream: false, ControlPlane: true, Note: "--model-endpoint",
		}
		configured := false
		for _, rule := range rules {
			if rule.Kind() == store.PullRuleURL && rule.Match == site {
				if experience && rule.Note == spec.Note && !slices.Equal(rule.Endpoints, spec.Endpoints) {
					rule.Endpoints = spec.Endpoints
					if _, err := st.UpdatePullSourceRule(rule.ID, rule); err != nil {
						return err
					}
				}
				configured = true
				break
			}
		}
		if configured {
			continue
		}
		if _, err := st.CreatePullSourceRule(spec); err != nil {
			return err
		}
	}
	return nil
}
