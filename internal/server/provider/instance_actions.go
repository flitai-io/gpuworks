package provider

// The operations a client may offer on a stored instance (InstanceActions).
const (
	ActionPowerOn  = "power_on"
	ActionPowerOff = "power_off"
	ActionInstall  = "install"
	ActionDelete   = "delete"
)

// InstanceActions are the operations the instance allows now, which a client
// offers as they are and does not derive itself. A node of a managed
// Kubernetes cluster allows none: it joins through the cluster and its
// lifecycle is the cluster's node pool's (Service.refuseClusterNode refuses
// them too). Otherwise: power off while the vendor says it runs, power on
// while it does not, when the kind controls power at all; installing the
// agent until the instance has a node; releasing it when the kind can.
func InstanceActions(caps Capabilities, power string, nodeID uint, managedCluster string) []string {
	actions := []string{}
	if managedCluster != "" {
		return actions
	}
	if caps.CanPowerControl {
		if power == string(PowerRunning) {
			actions = append(actions, ActionPowerOff)
		} else {
			actions = append(actions, ActionPowerOn)
		}
	}
	if nodeID == 0 {
		actions = append(actions, ActionInstall)
	}
	if caps.CanDelete {
		actions = append(actions, ActionDelete)
	}
	return actions
}
