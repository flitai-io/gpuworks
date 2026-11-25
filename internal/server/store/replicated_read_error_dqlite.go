//go:build linux && dqlite

package store

// ReplicatedReadInterrupted reports a dqlite server failure response that
// interrupted a read. During a leadership transfer a multi-part result can
// end in a failure frame whose code and text are leftover stream bytes (for
// example code 3 "key", or "EOF"); the next read on a fresh request
// succeeds. Callers may retry only side-effect-free reads on it.
// go-dqlite returns driver.Error as a value, which errors.As matches here.
func ReplicatedReadInterrupted(err error) bool {
	return Causes(err, CauseFailureResponse) != 0
}

// ReplicatedLeaderUnavailable reports a dial that found no elected leader.
// go-dqlite returns this sentinel as the root cause when a connection cannot
// be opened during an election.
func ReplicatedLeaderUnavailable(err error) bool {
	return Causes(err, CauseNoLeader) != 0
}

// ReplicatedWriterBusy reports a dqlite failure whose primary code is
// SQLITE_BUSY: the statement did not run because another connection held the
// database's one writer for the node's whole busy timeout (or a checkpoint
// was in progress). It says nothing about the data the statement would have
// read or written.
func ReplicatedWriterBusy(err error) bool {
	return Causes(err, CauseWriterBusy) != 0
}

func (replicatedRuntimeDatabase) leaderUnavailable(err error) bool {
	return ReplicatedLeaderUnavailable(err)
}

func (replicatedRuntimeDatabase) readInterrupted(err error) bool {
	return ReplicatedReadInterrupted(err)
}
