package api

import "slices"

// kubeLiveSettingFields are the entry settings its agents take live from the
// server instead of from the manifest: editing one needs no apply.
var kubeLiveSettingFields = []string{"engine_ports", "idle_return_seconds", "runtime_class"}

// pushKubeLiveSettings hands an edit of those settings to the entry's online
// agents, once the update has committed. changed names the edited fields
// (kubeChangedFields); agents that are offline get the values when they
// connect.
func (a *apiHandlers) pushKubeLiveSettings(clusterID uint, changed []string) {
	if a.d.NodeSync == nil {
		return
	}
	for _, field := range kubeLiveSettingFields {
		if slices.Contains(changed, field) {
			a.d.NodeSync.PushKubeSettings(clusterID)
			return
		}
	}
}
