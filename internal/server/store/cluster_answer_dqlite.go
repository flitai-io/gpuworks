//go:build linux && dqlite

package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"io/fs"
	"math/bits"
	"net"
	"net/url"
	"os"
	"syscall"

	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"
	"gorm.io/gorm"
)

// An error from the cluster is one of three things to its caller: no answer
// (the question never reached anyone who could decide it, or the reply was
// lost), a refusal, or a finding of another kind. This file is the one place
// that says which causes an error carries; a caller states, as an
// AnswerPolicy, which of them it takes for no answer.

// AnswerCause is a set of causes an error can carry.
type AnswerCause uint32

const (
	// CauseAdmissionRejected is a member's admission the database refused.
	CauseAdmissionRejected AnswerCause = 1 << iota
	// CauseIdentityConflict is a cluster identity that is not the
	// database's.
	CauseIdentityConflict
	// CauseMaintenanceNotServing is a maintenance phase that does not serve.
	CauseMaintenanceNotServing
	// CauseBusinessWritesClosed is business writes closed on this member.
	CauseBusinessWritesClosed
	// CausePhaseChanged is a maintenance phase that moved under the caller.
	CausePhaseChanged
	// CauseNoRow is a row that is not there.
	CauseNoRow
	// CauseCanceled is the caller's own cancellation.
	CauseCanceled
	// CauseDeadline is an expired deadline, a network timeout that reports
	// itself as one included.
	CauseDeadline
	// CauseNoLeader is a database with no elected leader to ask.
	CauseNoLeader
	// CauseObservationTimeout is a leader that was not read in time.
	CauseObservationTimeout
	// CauseBadConn is a connection the database driver dropped.
	CauseBadConn
	// CauseEOF and CauseUnexpectedEOF are a stream that ended before or
	// inside the reply.
	CauseEOF
	CauseUnexpectedEOF
	// CauseMembershipChanged is a membership view that moved during a grant.
	CauseMembershipChanged
	// CauseNetwork is an error of the network layer, or any error that
	// reports timeouts the way one does. See network.
	CauseNetwork
	// CauseNetTimeout is such an error that says it timed out.
	CauseNetTimeout
	// CausePeerUnreachable is a connection reset or refused, or a host or
	// network that cannot be reached.
	CausePeerUnreachable
	// CauseBrokenPipe is a write to a connection the peer closed.
	CauseBrokenPipe
	// CauseIODeadline is an expired read or write deadline.
	CauseIODeadline
	// CauseWriterBusy is a statement that never ran because the database's
	// one writer stayed taken.
	CauseWriterBusy
	// CauseFailureResponse is a failure response of the database, which is
	// not told apart from the leftover bytes of a leadership transfer.
	CauseFailureResponse
	// CauseNoMajority is a material manifest too few members vouch for.
	CauseNoMajority
	// CauseFile is a file that could not be opened, read or written: a
	// finding about this member's disk.
	CauseFile
	// CauseFailedRequest is an HTTP request that itself failed, as opposed
	// to a response that was read.
	CauseFailedRequest
)

// What a refusal is, for every caller. A member is refused when its
// admission is rejected or its cluster identity conflicts; whose admission
// it was is RefusesMember's question. The cluster is not serving during a
// maintenance phase or while business writes are closed, which is no verdict
// on any member.
const (
	CauseMemberRefused     = CauseAdmissionRejected | CauseIdentityConflict
	CauseClusterNotServing = CauseMaintenanceNotServing | CauseBusinessWritesClosed
	CauseRefused           = CauseMemberRefused | CauseClusterNotServing
)

// RefusesMember reports whether err is the database's verdict against the
// member with this key: the cluster identity the member holds is not the
// database's, or the admission the database refused is this member's. A
// refusal of another member's key, one that names no key, and a cluster that
// is not serving say nothing against this member. Of several keyed refusals
// in one error only the first is read; a grant returns one.
func RefusesMember(err error, key string) bool {
	if key == "" {
		return false
	}
	if Causes(err, CauseIdentityConflict) != 0 {
		return true
	}
	var refusal *ClusterAdmissionRefusal
	return errors.As(err, &refusal) && refusal.Key == key
}

// CauseLostConnection is the ways a peer's connection goes away under a
// request, as the material paths take them when no file is involved.
const CauseLostConnection = CauseNetTimeout | CausePeerUnreachable | CauseBrokenPipe | CauseIODeadline

// Causes reports which of the causes in among err carries anywhere in it,
// wrapped or joined.
func Causes(err error, among AnswerCause) AnswerCause {
	if err == nil {
		return 0
	}
	var found AnswerCause
	if among&(CauseNetwork|CauseNetTimeout) != 0 {
		found |= network(err) & among
	}
	for rest := among &^ (CauseNetwork | CauseNetTimeout); rest != 0; rest &= rest - 1 {
		if cause := AnswerCause(1) << bits.TrailingZeros32(uint32(rest)); carriesOne(err, cause) {
			found |= cause
		}
	}
	return found
}

func carriesOne(err error, cause AnswerCause) bool {
	switch cause {
	case CauseAdmissionRejected:
		return errors.Is(err, ErrClusterAdmissionRejected)
	case CauseIdentityConflict:
		return errors.Is(err, ErrClusterIdentityConflict)
	case CauseMaintenanceNotServing:
		return errors.Is(err, ErrMaintenanceNotServing)
	case CauseBusinessWritesClosed:
		return errors.Is(err, ErrBusinessWritesClosed)
	case CausePhaseChanged:
		return errors.Is(err, ErrMaintenancePhaseChanged)
	case CauseNoRow:
		return errors.Is(err, gorm.ErrRecordNotFound)
	case CauseCanceled:
		return errors.Is(err, context.Canceled)
	case CauseDeadline:
		return errors.Is(err, context.DeadlineExceeded)
	case CauseNoLeader:
		return errors.Is(err, dqlitedriver.ErrNoAvailableLeader)
	case CauseObservationTimeout:
		return errors.Is(err, ErrClusterMembershipObservationTimeout)
	case CauseBadConn:
		return errors.Is(err, driver.ErrBadConn)
	case CauseEOF:
		return errors.Is(err, io.EOF)
	case CauseUnexpectedEOF:
		return errors.Is(err, io.ErrUnexpectedEOF)
	case CauseMembershipChanged:
		return errors.Is(err, errClusterMembershipChanged)
	case CausePeerUnreachable:
		return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
			errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH)
	case CauseBrokenPipe:
		return errors.Is(err, syscall.EPIPE)
	case CauseIODeadline:
		return errors.Is(err, os.ErrDeadlineExceeded)
	case CauseWriterBusy:
		var failure dqlitedriver.Error
		return errors.As(err, &failure) && failure.Code&0xff == dqlitedriver.ErrBusy
	case CauseFailureResponse:
		// go-dqlite returns driver.Error as a value, which errors.As matches.
		var failure dqlitedriver.Error
		return errors.As(err, &failure)
	case CauseNoMajority:
		return errors.Is(err, ErrMaterialManifestNoMajority)
	case CauseFile:
		var file *fs.PathError
		return errors.As(err, &file)
	case CauseFailedRequest:
		var request *url.Error
		return errors.As(err, &request)
	}
	return false
}

// network reads the first error in err that reports timeouts as a network
// error does. A bare system call error number also does, and a file that
// cannot be opened or written carries one: that is a finding about this
// member's disk and never CauseNetwork. Whether it timed out is reported as
// the error itself says.
func network(err error) AnswerCause {
	var first net.Error
	if !errors.As(err, &first) {
		return 0
	}
	var found AnswerCause
	if _, bare := first.(syscall.Errno); !bare {
		found |= CauseNetwork
	}
	if first.Timeout() {
		found |= CauseNetTimeout
	}
	return found
}

// leafCause is the cause the innermost error of err's single-wrapper chain
// is, compared as the sentinel itself: a network timeout that only reports
// itself as an expired deadline is not CauseDeadline here. A chain that ends
// in a join has no leaf.
func leafCause(err error) AnswerCause {
	for err != nil {
		wrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = wrapped.Unwrap()
	}
	switch err {
	case context.DeadlineExceeded:
		return CauseDeadline
	case ErrClusterMembershipObservationTimeout:
		return CauseObservationTimeout
	case driver.ErrBadConn:
		return CauseBadConn
	case io.EOF:
		return CauseEOF
	case io.ErrUnexpectedEOF:
		return CauseUnexpectedEOF
	}
	return 0
}

// JoinedBranches finds the first multi-cause error along err's
// single-wrapper chain. An empty join is reported with no branches.
func JoinedBranches(err error) ([]error, bool) {
	for cause := err; cause != nil; {
		if joined, multiple := cause.(interface{ Unwrap() []error }); multiple {
			return joined.Unwrap(), true
		}
		wrapped, ok := cause.(interface{ Unwrap() error })
		if !ok {
			return nil, false
		}
		cause = wrapped.Unwrap()
	}
	return nil, false
}

// EveryBranch reports whether err is a join, found along its single-wrapper
// chain, and if so whether it is non-empty and each branch satisfies holds.
func EveryBranch(err error, holds func(error) bool) (held, joined bool) {
	branches, joined := JoinedBranches(err)
	if !joined {
		return false, false
	}
	for _, branch := range branches {
		if branch == nil || !holds(branch) {
			return false, true
		}
	}
	return len(branches) > 0, true
}

// JoinRule is how a policy reads an error with several causes. A join is
// found along the error's single-wrapper chain, so a wrapper around a join
// never changes the answer.
type JoinRule uint8

const (
	// AnyCause looks at the whole error at once: a verdict anywhere in it
	// wins, otherwise one accepted cause anywhere is enough.
	AnyCause JoinRule = iota
	// EveryBranchAlone asks each branch of a join on its own, so one branch
	// without an answer does not excuse another branch's verdict.
	EveryBranchAlone
	// NotJoined never accepts a join; its caller asks about each branch.
	NotJoined
)

// AnswerPolicy is what one caller takes for no answer.
type AnswerPolicy struct {
	// NoAnswer is accepted wherever the error carries it.
	NoAnswer AnswerCause
	// NoAnswerLeaf is accepted only as the innermost error of the wrapper
	// chain, compared as the sentinel itself.
	NoAnswerLeaf AnswerCause
	// UnlessFile is the part of NoAnswer that does not count when the error
	// also carries CauseFile: a file's error number can read as a timeout.
	UnlessFile AnswerCause
	// Requires is what the error must carry as well before NoAnswer and
	// NoAnswerLeaf count at all.
	Requires AnswerCause
	// Verdict wins over every accepted cause.
	Verdict AnswerCause
	Joined  JoinRule
	// also recognises a cause only the caller's package knows; see Also.
	also func(error) bool
}

// Also returns a copy of the policy that takes one more cause for no answer:
// any error for which recognises returns true. It is for a cause the store
// cannot name. recognises is asked wherever the policy's own causes are,
// under the same join rule; Requires does not apply to it, and a verdict
// still wins.
func (p AnswerPolicy) Also(recognises func(error) bool) AnswerPolicy {
	p.also = recognises
	return p
}

// Unanswered reports whether err is no answer under the policy. It is false
// for nil and for an error nothing classifies: an unknown failure is a
// finding.
func (p AnswerPolicy) Unanswered(err error) bool {
	if err == nil {
		return false
	}
	switch p.Joined {
	case EveryBranchAlone:
		if held, joined := EveryBranch(err, p.Unanswered); joined {
			return held
		}
	case NotJoined:
		if _, joined := JoinedBranches(err); joined {
			return false
		}
	}
	if p.Verdict != 0 && Causes(err, p.Verdict) != 0 {
		return false
	}
	return p.accepted(err)
}

// accepted reports whether err, read as one error, carries a cause the
// policy takes for no answer.
func (p AnswerPolicy) accepted(err error) bool {
	if p.also != nil && p.also(err) {
		return true
	}
	if p.Requires != 0 && Causes(err, p.Requires) != p.Requires {
		return false
	}
	accept := p.NoAnswer
	if p.UnlessFile != 0 && Causes(err, CauseFile) != 0 {
		accept &^= p.UnlessFile
	}
	return (accept != 0 && Causes(err, accept) != 0) || leafCause(err)&p.NoAnswerLeaf != 0
}
