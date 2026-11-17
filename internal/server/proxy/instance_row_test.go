package proxy

import (
	"gpuworks.ai/gpuworks/internal/server/store"
)

// updateInstanceRow writes columns of an instance's row the way a test sets
// up a state, and announces the change as the store's own writes do after
// they commit (the gateway trusts its route snapshot while no instance change
// was announced, instanceRowsUnchanged): the new state for a state change,
// "sleep" for a sleep level, "sleep-held" for a hold, "status" otherwise.
func updateInstanceRow(st *store.Store, uuid string, columns map[string]any) error {
	if err := st.DB().Model(&store.Instance{}).Where("uuid = ?", uuid).Updates(columns).Error; err != nil {
		return err
	}
	reason := "status"
	if state, ok := columns["state"].(string); ok {
		reason = state
	} else if _, ok := columns["sleep_level"]; ok {
		reason = "sleep"
	} else if _, ok := columns["sleep_held"]; ok {
		reason = "sleep-held"
	}
	st.Bus.PublishChange("instance", uuid, reason)
	return nil
}
