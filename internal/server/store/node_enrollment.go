package store

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// NodeEnrollmentGuard decides whether one more node may join the fleet. It
// is given the fleet as it is before the node is added and answers nil to
// let the node in, or the refusal the caller is shown.
//
// It runs inside the node insert's transaction on the writer, which is a
// single connection: it must be pure and O(1), work from what it is given
// and its own in-memory state, and never call the Store (that would wait
// on the very transaction it runs in).
type NodeEnrollmentGuard func(c FleetCapacity) *messages.ProductError

// ErrNodeEnrollmentRefused is wrapped, together with the guard's
// *messages.ProductError, by every node insert a guard refused.
var ErrNodeEnrollmentRefused = errors.New("node enrollment refused")

// nodeAdmission is what extensions registered on the store's node writes:
// enrollment guards on the inserts and scheduling holders on the
// scheduling decisions. Both are read on every such write, so each is one
// atomic load of a slice that registration replaces whole.
type nodeAdmission struct {
	mu      sync.Mutex // serializes registration
	guards  atomic.Pointer[[]NodeEnrollmentGuard]
	holders atomic.Pointer[[]SchedulingHolder]
	// cardHolders are the card holders (card_hold.go).
	cardHolders atomic.Pointer[[]CardSchedulingHolder]
}

func (a *nodeAdmission) enrollmentGuards() []NodeEnrollmentGuard {
	if p := a.guards.Load(); p != nil {
		return *p
	}
	return nil
}

// AddNodeEnrollmentGuard registers g on every node insert: CreateNodePending,
// RegisterNode, EnrollKubeNode and EnrollSlurmNode, which all create the
// row through insertNodeTx (TestEveryNodeInsertPathAsksTheGuard proves each;
// TestNoNodeIsCreatedBesideTheGuardedInsert trips on the usual way of
// adding another). Guards run in registration order and the
// first refusal wins. A nil guard is ignored; with none registered an
// insert counts nothing and is never refused.
func (s *Store) AddNodeEnrollmentGuard(g NodeEnrollmentGuard) {
	if g == nil {
		return
	}
	s.admission.mu.Lock()
	defer s.admission.mu.Unlock()
	next := append(append([]NodeEnrollmentGuard(nil), s.admission.enrollmentGuards()...), g)
	s.admission.guards.Store(&next)
}

// CheckNodeEnrollment asks the guards whether a node could join now,
// counting the fleet on the read pool. It is a pre-check for a caller about
// to spend something before the insert (renting a machine, an SSH install):
// it is not authoritative, as another insert may take the room in between,
// and the insert asks the guards again in its own transaction.
func (s *Store) CheckNodeEnrollment() error {
	guards := s.admission.enrollmentGuards()
	if len(guards) == 0 {
		return nil
	}
	c, err := fleetCapacity(s.reader())
	if err != nil {
		return err
	}
	return runEnrollmentGuards(guards, c)
}

// insertNodeTx is the one way a node row is created; no other code in this
// package may create a Node. With guards
// registered, it counts the fleet in tx and asks them first; the writer's
// single connection serializes inserts, so two of them cannot both take
// the last place.
func (s *Store) insertNodeTx(tx *gorm.DB, n *Node) error {
	if guards := s.admission.enrollmentGuards(); len(guards) > 0 {
		c, err := fleetCapacity(tx)
		if err != nil {
			return err
		}
		if err := runEnrollmentGuards(guards, c); err != nil {
			return err
		}
	}
	return tx.Create(n).Error
}

func runEnrollmentGuards(guards []NodeEnrollmentGuard, c FleetCapacity) error {
	for _, guard := range guards {
		if refusal := guard(c); refusal != nil {
			if refusal.Code() == "" {
				fallback := refusal.Error()
				if fallback == "" {
					fallback = "a new node cannot join the fleet now"
				}
				refusal = messages.NewProductError(messages.ErrorNodeEnrollmentRefused, fallback, refusal.Params())
			}
			return fmt.Errorf("%w: %w", ErrNodeEnrollmentRefused, refusal)
		}
	}
	return nil
}

// ReleaseNodeAdmission forgets every enrollment guard and scheduling holder.
// A cluster member's store outlives the business generations assembled on
// it; each generation's modules register again, and one that was withdrawn
// must not keep answering for nodes or refuse its successor's holder.
func (s *Store) ReleaseNodeAdmission() {
	s.admission.mu.Lock()
	defer s.admission.mu.Unlock()
	s.admission.guards.Store(nil)
	s.admission.holders.Store(nil)
}
