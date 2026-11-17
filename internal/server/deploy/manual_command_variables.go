package deploy

import (
	"slices"

	placementeval "gpuworks.ai/gpuworks/internal/server/placement"
)

// ManualCommandVariables lists, under their prefixed names and sorted, the
// variables at least one of these launches states: the names a manual command
// shared by them can reference and have replaced. A name only some launches
// state (MASTER_ADDR on a single node, PD_SIDE_CHANNEL_HOST before the address
// is known) is listed too; where a launch does not state it, the shell sees
// the reference unchanged.
func ManualCommandVariables(ranks []ResolveLaunchInput) []string {
	seen := map[string]struct{}{}
	for _, input := range ranks {
		// The model mount is a value, not a name: every launch states
		// MODEL_PATH whatever it is.
		variables := launchVariables(input.Engine, input.Params, DefaultModelMount, input.PlacementVariables, input.AllowUnresolved)
		for name := range placementeval.Prefixed(variables) {
			seen[name] = struct{}{}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
