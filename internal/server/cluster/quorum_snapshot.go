package cluster

import (
	"context"
	"database/sql"
	"errors"
	"slices"
)

// ErrQuorumReadNotLeader means the member asked does not lead the database:
// only the leader reads a quorum snapshot, so the asker goes to the leader.
var ErrQuorumReadNotLeader = errors.New("quorum snapshot requires the local native leader")

// WithLocalQuorumReadSnapshot brackets one local leader SQL snapshot with fresh
// barriers. The internal reader must use only fixed SELECT/schema queries and
// close its rows before returning. TxOptions.ReadOnly is not enforced by the
// native driver. Publish captured facts only after this method succeeds.
func (d *Database) WithLocalQuorumReadSnapshot(ctx context.Context, read func(context.Context, *sql.Tx, QuorumProof) error) (QuorumProof, error) {
	if err := ctx.Err(); err != nil {
		return QuorumProof{}, err
	}
	if d == nil || d.quorumSnapshot == nil {
		return QuorumProof{}, ErrUnavailable
	}
	if read == nil || d.closed.Load() {
		return QuorumProof{}, errors.New("local quorum snapshot requires an open owner and reader")
	}
	proof, err := d.quorumSnapshot(ctx, read)
	err = errors.Join(err, ctx.Err())
	if d.closed.Load() {
		err = errors.Join(err, errors.New("database is closed"))
	}
	if err != nil {
		return QuorumProof{}, err
	}
	return proof, nil
}

func readLocalQuorumSnapshot(ctx context.Context, d *Database, barrier func(context.Context) (QuorumProof, error), open func(context.Context) (*sql.DB, func() error, error), read func(context.Context, *sql.Tx, QuorumProof) error) (QuorumProof, error) {
	first, err := barrier(ctx)
	if err != nil {
		return QuorumProof{}, err
	}
	if !localQuorumLeader(d, first) {
		return QuorumProof{}, ErrQuorumReadNotLeader
	}
	if err := readQuorumSQL(ctx, first, open, read); err != nil {
		return QuorumProof{}, err
	}
	if err := ctx.Err(); err != nil {
		return QuorumProof{}, err
	}
	last, err := barrier(ctx)
	if err != nil {
		return QuorumProof{}, err
	}
	if !localQuorumLeader(d, last) || last.Index <= first.Index || !sameQuorumConfiguration(first, last) {
		return QuorumProof{}, errors.New("local quorum configuration changed across SQL snapshot")
	}
	return last, ctx.Err()
}

func readQuorumSQL(ctx context.Context, first QuorumProof, open func(context.Context) (*sql.DB, func() error, error), read func(context.Context, *sql.Tx, QuorumProof) error) (result error) {
	pool, cleanup, err := open(ctx)
	if cleanup != nil {
		defer func() { result = errors.Join(result, cleanup()) }()
	}
	if err != nil {
		return err
	}
	if pool == nil || cleanup == nil {
		return errors.New("quorum snapshot requires an owned SQL pool")
	}
	conn, err := pool.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, conn.Close()) }()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() {
		err := tx.Rollback()
		// Cancellation can start database/sql's background rollback. ErrTxDone
		// is not its completion; Conn.Close and the owned socket cleanup join it.
		// Its hidden rollback error cannot be recovered from the public API.
		if errors.Is(err, sql.ErrTxDone) && ctx.Err() != nil {
			err = nil
		}
		result = errors.Join(result, err, ctx.Err())
	}()
	first.Members = slices.Clone(first.Members)
	return read(ctx, tx, first)
}

func localQuorumLeader(d *Database, proof QuorumProof) bool {
	if d.ID == 0 || d.Address == "" || proof.LeaderID != d.ID {
		return false
	}
	for _, member := range proof.Members {
		if member.ID == d.ID {
			return member.Address == d.Address && member.Role == "voter"
		}
	}
	return false
}

func sameQuorumConfiguration(a, b QuorumProof) bool {
	if a.Term != b.Term || a.ConfigurationIndex != b.ConfigurationIndex || a.LeaderID != b.LeaderID || len(a.Members) != len(b.Members) {
		return false
	}
	members := make(map[uint64]Member, len(a.Members))
	for _, member := range a.Members {
		members[member.ID] = member
	}
	for _, member := range b.Members {
		if members[member.ID] != member {
			return false
		}
	}
	return true
}
