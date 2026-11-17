package deploy

// EngineServedName preserves an explicitly cleared alias while older
// deployments continue to use their public routing name.
func (p Params) EngineServedName() string {
	if p.Quick.ServedModelName != nil {
		return *p.Quick.ServedModelName
	}
	return p.ServedName
}

// launchServedName is the --served-model-name a launch emits, or "" for none.
// A deployment with no alias is named after its model mount (ModelPath): the
// name the engine would default to inside a container and the one the
// gateway sends. Spelling it out keeps it on a runtime that translates the
// model path to the host's (a host-process agent), so the gateway, the
// agent's port identity check and the engine agree on one name. A headless
// rank serves no API and gets no name.
func (p Params) launchServedName() string {
	if p.Headless {
		return ""
	}
	if name := p.EngineServedName(); name != "" {
		return name
	}
	if p.ImplicitServedName {
		return ""
	}
	return p.ModelPath
}
