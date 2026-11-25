//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// memberServiceHandler is the health and admission entry in front of the
// ordinary API router. Public GET /healthz stays the process liveness payload.
// Authenticated GET /api/v1/healthz keeps its token check and adds the three
// member answers plus entry candidacy. Any other route is admitted when the
// member is business-ready, including a healthy follower that is not the scheduler.
func memberServiceHandler(next http.Handler, member *App, refusals *businessRefusals) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if next == nil {
			refusals.refuse(w, r, refusalNoHandler)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/healthz" {
			serveMemberHealth(w, r, next, member, refusals)
			return
		}
		if service := member.memberService(r.Context(), false); !service.BusinessReady {
			reason := service.BusinessRefusal
			if code := member.proofRefusedAs(); code != "" && reason == refusalProofNotEligible {
				// The same code the gate in front gives for this state.
				reason = code
			}
			refusals.refuse(w, r, reason)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// memberService reads the facts this process already has. Scheduling leadership
// is consulted only when the caller is presenting that answer. Business
// admission does not need it, so a follower can serve without a leader query.
func (a *App) memberService(ctx context.Context, scheduling bool) MemberService {
	if a == nil || a.store == nil {
		return EvaluateMemberService(false, false, false, false, false)
	}
	leader := false
	if scheduling {
		local, err := a.store.LocalSchedulingLeader(ctx)
		if err == nil {
			leader = local
		}
	}
	return EvaluateMemberService(true, a.materialsSurviveFault(), leader, a.sessionsRoutable(), a.targetsCurrent())
}

// materialsSurviveFault is true until this process installs a material proof.
// An installed proof counts only while it is still eligible.
func (a *App) materialsSurviveFault() bool {
	if a == nil {
		return false
	}
	a.materialEligibilityMu.Lock()
	eligibility := a.materialEligibility
	a.materialEligibilityMu.Unlock()
	if eligibility == nil {
		return true
	}
	return eligibility.Eligible()
}

// proofRefusedAs is the refusal code of the database's refusal this member's
// last proof ended on, or empty.
func (a *App) proofRefusedAs() string {
	if a == nil {
		return ""
	}
	a.materialEligibilityMu.Lock()
	eligibility := a.materialEligibility
	a.materialEligibilityMu.Unlock()
	return eligibility.refusedAs()
}

// sessionsRoutable is true when this process serves its own sessions. A noted
// membership with no listen address cannot accept session calls.
func (a *App) sessionsRoutable() bool {
	if a == nil || a.store == nil {
		return false
	}
	route, ok := a.store.ClusterSessionRoute()
	if !ok {
		return true
	}
	return strings.TrimSpace(route.Listen) != ""
}

// targetsCurrent is the member-level input. Each resource already carries its
// own target version; this process does not keep a second generation that
// would mark every target stale together.
func (a *App) targetsCurrent() bool { return true }

func serveMemberHealth(w http.ResponseWriter, r *http.Request, next http.Handler, member *App, refusals *businessRefusals) {
	rec := &capturingResponse{}
	next.ServeHTTP(rec, r)
	if member == nil || rec.status != http.StatusOK {
		rec.replay(w)
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.body.Bytes(), &payload); err != nil || payload == nil {
		rec.replay(w)
		return
	}
	service := member.memberService(r.Context(), true)
	payload["live"] = service.Live
	payload["business_ready"] = service.BusinessReady
	payload["schedulable"] = service.Schedulable
	payload["entry_candidate"] = member.entryCandidate(r.Context(), service.BusinessReady)
	if refused := refusals.snapshot(); len(refused) != 0 {
		payload["business_refusals"] = refused
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		rec.replay(w)
		return
	}
	header := rec.Header().Clone()
	header.Set("Content-Type", "application/json")
	header.Set("Cache-Control", "no-store")
	header.Del("Content-Length")
	copyHeader(w.Header(), header)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(encoded, '\n'))
}

type capturingResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *capturingResponse) Header() http.Header {
	if c.header == nil {
		c.header = make(http.Header)
	}
	return c.header
}

func (c *capturingResponse) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

func (c *capturingResponse) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(p)
}

func (c *capturingResponse) replay(w http.ResponseWriter) {
	copyHeader(w.Header(), c.Header())
	status := c.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(c.body.Bytes())
}

func copyHeader(dst, src http.Header) {
	for key, values := range src {
		copied := make([]string, len(values))
		copy(copied, values)
		dst[key] = copied
	}
}
