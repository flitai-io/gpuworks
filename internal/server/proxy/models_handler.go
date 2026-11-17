package proxy

import (
	"encoding/json"
	"net/http"
)

// ModelsHandler serves GET /v1/models listing deployed served-names (OpenAI
// shape), only those of deployments the caller's API key may call.
//
// It answers from the route table the gateway routes requests by, not from
// the store: a client polling the list reads no deployment rows, and every
// name it lists is one a request can reach. A name several deployments share
// (created before served names were unique) is listed once, and the list is
// in name order so two polls with no change read the same.
func (p *Proxy) ModelsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, identity, ok := p.authenticate(w, r)
		if !ok {
			return
		}
		type model struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		}
		out := []model{}
		for _, name := range p.routes.servedNames(identity.Allows) {
			out = append(out, model{ID: name, Object: "model", OwnedBy: "gpuworks"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": out})
	}
}
