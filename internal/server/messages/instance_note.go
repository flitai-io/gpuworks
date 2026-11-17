package messages

// Instance notes: non-blocking facts about a running instance that explain
// what a user may see of it, derived when the instance is read
// (deployment.InstanceNotes), never stored.
const (
	// InstanceCardSharedOutside: the instance's cards {gpu_indexes} are also
	// used by processes outside the platform, which hold up to
	// {foreign_bytes} ({foreign_gib} GiB) on one of them. Memory is
	// isolated; compute and bandwidth are shared.
	InstanceCardSharedOutside = "INSTANCE_CARD_SHARED_OUTSIDE"
	// InstanceCardSharedOutsideUnknown: as InstanceCardSharedOutside, but
	// what those processes hold is not known. Params: gpu_indexes.
	InstanceCardSharedOutsideUnknown = "INSTANCE_CARD_SHARED_OUTSIDE_UNKNOWN"
)
