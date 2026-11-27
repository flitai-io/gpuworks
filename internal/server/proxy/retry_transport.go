package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

type tunnelOpenError struct{ err error }

func (e *tunnelOpenError) Error() string { return "open proxy tunnel: " + e.err.Error() }
func (e *tunnelOpenError) Unwrap() error { return e.err }

type routeAttempt struct {
	candidates []candidate
	current    *lease
	tried      map[string]bool
	started    time.Time
	attempts   int
	// modelName is the name the request body asks the engine for: the
	// first replica's, then each retried replica's (empty: the public name).
	modelName string
	// renamed releases the bodies a retry rewrote for another replica's
	// name; serveTarget calls it once the reply is copied.
	renamed      []func()
	policy       string
	affinity     string
	deploymentID uint
	accountant   *Accountant
	// admission is the request's, which every replica it tries is leased
	// for.
	admission admission
	// prioritized is whether the first replica schedules by priority
	// (candidate.prioritized). A resend goes only to a replica that does
	// the same: the priority written into the body, and the
	// X-GPUWorks-Priority-Scheduling header set for the first replica, stay
	// true of the one that answers.
	prioritized bool
}

type retryTransport struct {
	base     http.RoundTripper
	balancer *balancer
	hub      Tunneler
	// awake reports whether a leased instance may still take the request
	// (Proxy.awake); nil takes every lease.
	awake func(*lease) bool
	guard *instanceGuard
	// maxBody bounds a body a retry rewrites for another replica's name.
	maxBody int64
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	a, _ := req.Context().Value(attemptKey).(*routeAttempt)
	if a == nil || a.current == nil {
		tgt, _ := req.Context().Value(targetKey).(target)
		return t.guard.roundTrip(t.base, req, candidate{instanceUUID: tgt.instanceUUID, port: tgt.port})
	}
	if a.started.IsZero() {
		a.started = time.Now()
	}
	for {
		resp, err := t.guard.roundTrip(t.base, req, a.current.c)
		if err == nil {
			return resp, nil
		}
		if a.current != nil {
			outcome := outcomeConnFailure
			if req.Context().Err() != nil || errors.Is(err, context.Canceled) {
				outcome = outcomeCanceled
			}
			a.current.release(outcome)
			a.current = nil
		}
		if !t.canRetry(req, a, err) {
			return nil, err
		}
		publicName := publicNameFor(req.Context())
		next := t.pickNext(a, publicName != "")
		if next == nil {
			return nil, err
		}
		a.current = next
		if a.accountant != nil {
			a.accountant.RecordRouted(a.deploymentID, next.c.instanceUUID)
		}
		if a.accountant != nil {
			reason := "no_response"
			var tunnelErr *tunnelOpenError
			if errors.As(err, &tunnelErr) {
				reason = "tunnel_open"
			}
			a.accountant.RecordRetry(a.deploymentID, reason)
		}
		a.tried[next.c.instanceUUID] = true
		a.attempts++
		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			next.release(outcomeCanceled)
			a.current = nil
			return nil, bodyErr
		}
		clone := req.Clone(req.Context())
		clone.Body = body
		clone.ContentLength = req.ContentLength
		clone.GetBody = req.GetBody
		if next.c.modelName != a.modelName {
			if err := t.rename(clone, a, next.c.modelName, publicName); err != nil {
				next.release(outcomeCanceled)
				a.current = nil
				return nil, err
			}
		}
		req = clone
	}
}

// rename rewrites a retry's body for a replica whose engine answers under
// another name than the one the body asks for: replicas of one deployment on
// host-process nodes answer under their own nodes' model paths (ADR 0044).
func (t *retryTransport) rename(clone *http.Request, a *routeAttempt, engineName, publicName string) error {
	name := engineName
	if name == "" {
		name = publicName
	}
	release, err := rewriteModel(clone, name, t.maxBody)
	if err != nil {
		return err
	}
	a.renamed = append(a.renamed, release)
	a.modelName = engineName
	return nil
}

// releaseRenamed frees the bodies retries rewrote; the request is done.
func (a *routeAttempt) releaseRenamed() {
	for _, release := range a.renamed {
		release()
	}
	a.renamed = nil
}

// pickNext leases the next replica to retry on. One whose engine answers
// under another name is taken only when the body can be renamed for it,
// which needs the public name the client asked for, and one that schedules
// otherwise than the first replica is not taken at all. A resend is never
// refused for being busy: a replica already took the request.
func (t *retryTransport) pickNext(a *routeAttempt, rename bool) *lease {
	adm := a.admission.waiting()
	return leaseUntil(func(excluded map[string]bool) *lease {
		l, _ := t.balancer.pickBy(a.policy, a.candidates, a.affinity, excluded, t.hub.Online, adm)
		return l
	}, a.tried, func(l *lease) bool {
		if (l.c.modelName != a.modelName && !rename) || l.c.prioritized != a.prioritized {
			return false
		}
		return t.awake == nil || t.awake(l)
	})
}

func (t *retryTransport) canRetry(req *http.Request, a *routeAttempt, err error) bool {
	if req.Context().Err() != nil || a.attempts >= retryMaxAttempts || req.GetBody == nil {
		return false
	}
	if a.started.IsZero() {
		return true
	}
	var tunnelErr *tunnelOpenError
	return !errors.Is(err, context.Canceled) && (errors.As(err, &tunnelErr) || time.Since(a.started) < retryWindow)
}

func (t *retryTransport) String() string { return fmt.Sprintf("retryTransport(%T)", t.base) }
