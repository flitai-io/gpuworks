package proto

// The levels of the partition safety contract an agent states in
// SysInfo.PartitionSafetyProtocol.
const (
	// PartitionSafetyOwnership: the agent keeps a partition's native state
	// and owner until its container is confirmed gone, which a backend
	// without a behavioural probe relies on (partitioncap).
	PartitionSafetyOwnership = 1
	// PartitionSafetyForeignTracked: the agent keeps reporting the processes
	// outside the platform it found on a card after one of the platform's
	// instances joins them there, for as long as that instance stays (at
	// their last figure once they are no longer listed: they may have
	// restarted under a new PID). Without that, the first partition placed
	// beside them would make the card read as free, so the server shares a
	// held card by memory only with an agent at this level (ADR 0068).
	// Rolling an agent back below it stops new sharing only: partitions
	// already beside such processes lose their cover, since the older agent
	// stops reporting them.
	PartitionSafetyForeignTracked = 2
	// PartitionSafetyForeignTrackedPartitioned: besides, the agent reads a
	// card that holds only memory-limited partitions of the platform by
	// how many processes their engines keep there, so processes outside the
	// platform that start on it after them are reported too (and the server
	// notes them on the instances there). A card that holds a whole-card
	// instance is not read so: its engine keeps no fixed number of
	// processes, and processes that join it later are not reported.
	PartitionSafetyForeignTrackedPartitioned = 3
	// PartitionSafetyCurrent is the level this build's agent keeps.
	PartitionSafetyCurrent = PartitionSafetyForeignTrackedPartitioned
)
