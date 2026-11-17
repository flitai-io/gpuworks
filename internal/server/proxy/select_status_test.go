package proxy

import "gpuworks.ai/gpuworks/internal/server/store"

// The selection tests read a refusal as the status and English message the
// client receives.

func (p *Proxy) selectStatus(modelName string) (*store.Deployment, target, int, string, bool) {
	dep, tgt, rejection, asleep := p.selectDeploymentInstance(modelName)
	return dep, tgt, rejection.status(), rejection.message(), asleep
}

func (p *Proxy) selectScopedStatus(scope *store.APIKeyIdentity, modelName, affinity string) (*store.Deployment, target, int, string, bool) {
	dep, tgt, rejection, asleep := p.selectScopedDeploymentInstance(scope, modelName, affinity)
	return dep, tgt, rejection.status(), rejection.message(), asleep
}

func (p *Proxy) selectDistributedStatus(dep *store.Deployment, insts []store.Instance, modelName, affinity string) (target, int, string, bool) {
	tgt, rejection, asleep := p.selectDistributedInstance(dep, insts, modelName, affinity)
	return tgt, rejection.status(), rejection.message(), asleep
}
