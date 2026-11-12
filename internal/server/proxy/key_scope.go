package proxy

import (
	"context"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// keyScopeKey carries a restricted API key's identity through a /v1 request,
// so every deployment selection on its path (first pick, wake hold, replay)
// stays inside the deployments the key may call.
type keyScopeKey struct{}

func withKeyScope(ctx context.Context, identity store.APIKeyIdentity) context.Context {
	if !identity.Restricted {
		return ctx
	}
	return context.WithValue(ctx, keyScopeKey{}, &identity)
}

// keyScopeFrom is the request's key scope; nil when it may call every
// deployment (an unrestricted key, the playground, or a request the gateway
// accepted without a key because an administrator opened it).
func keyScopeFrom(ctx context.Context) *store.APIKeyIdentity {
	identity, _ := ctx.Value(keyScopeKey{}).(*store.APIKeyIdentity)
	return identity
}

// selectForRequest selects a deployment instance within the request's key
// scope.
func (p *Proxy) selectForRequest(ctx context.Context, modelName, affinity string) (*store.Deployment, target, int, string, bool) {
	return p.selectScopedDeploymentInstance(keyScopeFrom(ctx), modelName, affinity)
}
