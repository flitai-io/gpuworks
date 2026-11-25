package cluster

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// roleLinePrefix starts every line the database process writes about its Raft
// role. The lines are no part of the versioned contract with that process: a
// process that does not write them, and a line this server does not know, are
// both fine.
const roleLinePrefix = "gpuworks-raft: "

// roleRecordsPerMinute bounds the records filed from one database process in a
// minute; the rest are counted and the count is filed once the minute is over.
// The process limits its own repeats; this is the server's own guard against
// a process that does not. Records that end or begin a leadership are filed
// outside the budget.
const roleRecordsPerMinute = 240

// roleDecisivePerMinute bounds the records that end or begin a leadership.
// They have a budget of their own, far above what a cluster in trouble
// writes (a member alone campaigns about once a second), so that a database
// process gone wrong cannot fill the log through them either. The first
// record of each kind in a minute is filed whatever the count.
const roleDecisivePerMinute = 1200

// roleReasonFresh is how long a reason this member observed is attributed to
// the next change of leader.
const roleReasonFresh = 30 * time.Second

// roleField is one key=value field of a role record, in the order written.
type roleField struct{ key, value string }

// parseRoleRecord splits a record into its fields. A value may be quoted as Go
// quotes a string; a part without "=" is kept under the key "text".
func parseRoleRecord(record string) []roleField {
	var fields []roleField
	rest := strings.TrimSpace(record)
	for rest != "" {
		eq := strings.IndexByte(rest, '=')
		sp := strings.IndexByte(rest, ' ')
		if eq < 0 || (sp >= 0 && sp < eq) {
			word := rest
			if sp >= 0 {
				word, rest = rest[:sp], strings.TrimLeft(rest[sp:], " ")
			} else {
				rest = ""
			}
			fields = append(fields, roleField{"text", word})
			continue
		}
		key := rest[:eq]
		rest = rest[eq+1:]
		var value string
		if strings.HasPrefix(rest, `"`) {
			quoted, err := strconv.QuotedPrefix(rest)
			if err == nil {
				value, _ = strconv.Unquote(quoted)
				rest = rest[len(quoted):]
			} else {
				value, rest = rest, ""
			}
		} else if sp := strings.IndexByte(rest, ' '); sp >= 0 {
			value, rest = rest[:sp], rest[sp:]
		} else {
			value, rest = rest, ""
		}
		rest = strings.TrimLeft(rest, " ")
		fields = append(fields, roleField{key, value})
	}
	return fields
}

func roleValue(fields []roleField, key string) string {
	for _, f := range fields {
		if f.key == key {
			return f.value
		}
	}
	return ""
}

// roleLevel is the level a record is filed at: what ends a leadership or
// explains why one may end is a warning, the rest is information.
func roleLevel(fields []roleField) slog.Level {
	switch roleValue(fields, "event") {
	case "step_down", "election_start", "election_retry", "transfer_timeout", "loop_late",
		"slow_log_write", "slow_step", "peer_disconnected", "peer_connect_failed", "peer_send_failed", "dropped", "health":
		return slog.LevelWarn
	case "app":
		if roleValue(fields, "level") == "error" {
			return slog.LevelError
		}
		return slog.LevelWarn
	}
	return slog.LevelInfo
}

// roleTracker follows one database process' role records and adds, at every
// change of the leader this member knows, one record that says who led before,
// who leads now, why as far as this member saw it, and how long this member
// had no leader.
type roleTracker struct {
	log func() *slog.Logger

	mu        sync.Mutex
	now       func() time.Time
	windowAt  time.Time // start of the current minute of the budgets
	filed     int       // records filed in it
	skipped   int       // records left out of it
	decisive  int       // decisive records filed in it
	held      int       // decisive records left out of it
	kinds     map[string]bool
	lostMono  int64 // the database process' monotonic clock when the leader was lost, in ms; -1 without one
	self      string
	leader    string    // member id of the leader this member follows or is; "" when none
	lostAt    time.Time // when this member last stopped having a leader; zero while it has one
	reason    string
	reasonAt  time.Time
	hadLeader bool
	term      string // term of the leader this member has
	ledBefore bool   // this member led and stopped leading since the last summary
}

func newRoleTracker(log func() *slog.Logger) *roleTracker {
	if log == nil {
		log = slog.Default
	}
	return &roleTracker{log: log, now: time.Now, lostMono: -1}
}

// roleDecisive names the kind of a record that says a leadership ended or
// began, what the connections to the peers were doing at that moment, or that
// records were lost; "" for any other record. Those are filed outside the
// ordinary budget: a leader that steps down while no successor can be elected
// leaves nothing else.
func roleDecisive(fields []roleField) string {
	switch event := roleValue(fields, "event"); event {
	case "step_down", "election_won", "following", "dropped":
		return event
	case "peer_io", "tcp":
		if trigger := roleValue(fields, "trigger"); trigger == "step_down" || trigger == "election_start" || trigger == "role_change" {
			return event
		}
	case "state":
		if roleValue(fields, "old") == "LEADER" || roleValue(fields, "new") == "LEADER" {
			return "state"
		}
	}
	return ""
}

// admit applies the budgets. It reports whether this record is filed and how
// many ordinary and decisive records were left out of the minute that has
// just ended.
func (t *roleTracker) admit(fields []roleField) (file bool, leftOut, decisiveLeftOut int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if now.Sub(t.windowAt) >= time.Minute {
		leftOut, t.skipped = t.skipped, 0
		decisiveLeftOut, t.held = t.held, 0
		t.windowAt, t.filed, t.decisive, t.kinds = now, 0, 0, nil
	}
	if kind := roleDecisive(fields); kind != "" {
		first := !t.kinds[kind]
		if t.kinds == nil {
			t.kinds = map[string]bool{}
		}
		t.kinds[kind] = true
		if !first && t.decisive >= roleDecisivePerMinute {
			t.held++
			return false, leftOut, decisiveLeftOut
		}
		t.decisive++
		return true, leftOut, decisiveLeftOut
	}
	if t.filed >= roleRecordsPerMinute {
		t.skipped++
		return false, leftOut, decisiveLeftOut
	}
	t.filed++
	return true, leftOut, decisiveLeftOut
}

// record files one record and, when it names a new leader, the summary.
func (t *roleTracker) record(record string) {
	fields := parseRoleRecord(record)
	if len(fields) == 0 {
		return
	}
	at := time.Now()
	attrs := make([]any, 0, 2*len(fields))
	for _, f := range fields {
		if f.key == "at" {
			if ns, err := strconv.ParseInt(f.value, 10, 64); err == nil {
				at = time.Unix(0, ns)
				// The process queues its records and this server reads
				// them a moment later; this is when the event happened.
				attrs = append(attrs, "observed_at", at.Format(time.RFC3339Nano))
			}
			continue
		}
		if f.key == "mono" {
			// Read below to measure between two records; of no use to a
			// reader as a number.
			continue
		}
		attrs = append(attrs, f.key, f.value)
	}
	log := t.log()
	file, leftOut, decisiveLeftOut := t.admit(fields)
	if leftOut > 0 {
		log.Warn("cluster database role events suppressed", "suppressed", leftOut, "per_minute", roleRecordsPerMinute)
	}
	if decisiveLeftOut > 0 {
		log.Warn("cluster database role events suppressed", "suppressed", decisiveLeftOut, "per_minute", roleDecisivePerMinute, "kind", "leadership")
	}
	if file {
		log.Log(context.Background(), roleLevel(fields), "cluster database role event", attrs...)
	}
	// The leader is followed through every record, filed or not, and a
	// change of leader is always written.
	mono := int64(-1)
	if ms, err := strconv.ParseInt(roleValue(fields, "mono"), 10, 64); err == nil && ms >= 0 {
		mono = ms
	}
	if summary, changed := t.follow(fields, at, mono); summary != nil {
		if changed {
			log.Info("cluster database leader changed", summary...)
		} else {
			log.Info("cluster database leader kept after a campaign", summary...)
		}
	}
}

// follow returns the summary to write when this record names the leader this
// member has, and whether the leadership changed. It did not when this member
// campaigned in vain and returned to its leader in the same term. It did,
// with the same leader before and after, when that leader lost the role and
// won it again: the term is then another, or this member itself stopped
// leading in between.
func (t *roleTracker) follow(fields []roleField, at time.Time, mono int64) ([]any, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	lose := func() {
		if t.lostAt.IsZero() && (t.leader != "" || !t.hadLeader) {
			t.lostAt, t.lostMono = at, mono
		}
	}
	why := func(text string, replace bool) {
		if replace || t.reason == "" || at.Sub(t.reasonAt) > roleReasonFresh {
			t.reason, t.reasonAt = text, at
		}
	}
	join := func(head string, keys ...string) string {
		var b strings.Builder
		b.WriteString(head)
		for _, key := range keys {
			if value := roleValue(fields, key); value != "" {
				b.WriteString(" " + key + "=" + value)
			}
		}
		return b.String()
	}
	var newLeader string
	switch roleValue(fields, "event") {
	case "state":
		if id := roleValue(fields, "id"); id != "" {
			t.self = id
		}
		if roleValue(fields, "old") == "UNAVAILABLE" {
			// The process has just started and knows no leader yet.
			lose()
		}
		if roleValue(fields, "old") == "LEADER" {
			why("this member stopped leading", false)
			lose()
			t.ledBefore = true
		}
		if roleValue(fields, "new") == "LEADER" {
			newLeader = t.self
		}
	case "step_down":
		why(join("this member stepped down: "+roleValue(fields, "reason"), "contacts", "voters", "peer_ack_age_ms", "from", "new_term"), true)
		lose()
	case "election_start":
		why(join("this member started an election: "+roleValue(fields, "reason"), "since_leader_contact_ms", "timeout_ms", "from"), true)
		lose()
	case "term_bump":
		why(join("another member raised the term", "from", "new_term"), false)
		lose()
	case "vote_reply":
		if roleValue(fields, "granted") == "1" && roleValue(fields, "pre_vote") == "0" {
			why(join("this member voted for another", "candidate", "disrupt_leader"), false)
			lose()
		}
	case "transfer_start":
		why(join("this member was asked to hand the leadership over", "to"), true)
	case "following":
		newLeader = roleValue(fields, "leader")
	}
	if newLeader == "" || (newLeader == t.leader && t.lostAt.IsZero()) {
		return nil, false
	}
	term := roleValue(fields, "term")
	changed := newLeader != t.leader || t.ledBefore || (term != "" && t.term != "" && term != t.term)
	old := t.leader
	if old == "" {
		old = "none"
	}
	reason := t.reason
	switch {
	case !t.hadLeader:
		reason = "first leader this member has seen since its database process started"
	case reason == "" || at.Sub(t.reasonAt) > roleReasonFresh:
		reason = "not seen by this member; read the other members' role events around this time"
	}
	summary := []any{
		"old_leader", old, "new_leader", newLeader, "term", roleValue(fields, "term"),
		"this_member", t.self, "leads", newLeader == t.self, "reason", reason,
		"observed_at", at.Format(time.RFC3339Nano),
	}
	if !t.lostAt.IsZero() {
		// Measured on the database process' monotonic clock when both
		// records carry it: the wall clock may have been set in between.
		// Without it the wall clock is used and a step backwards counts
		// as no time.
		without := max(at.Sub(t.lostAt).Milliseconds(), 0)
		if mono >= 0 && t.lostMono >= 0 && mono >= t.lostMono {
			without = mono - t.lostMono
		}
		summary = append(summary, "without_leader_ms", without)
	}
	switch {
	case !changed:
		// "leader" instead of the pair: nothing changed hands.
		summary = append([]any{"leader", newLeader}, summary[4:]...)
	case newLeader == t.leader:
		// The same member again: the terms tell the two leaderships apart.
		summary = append(summary, "old_term", t.term)
	}
	t.leader, t.lostAt, t.lostMono, t.reason, t.hadLeader = newLeader, time.Time{}, -1, "", true
	t.term, t.ledBefore = term, false
	return summary, changed
}

// roleLineWriter separates the role records from the rest of a database
// process' standard error: records go to the tracker, every other line to
// rest, unchanged. Lines are passed on whole.
type roleLineWriter struct {
	tracker *roleTracker
	rest    io.Writer

	mu      sync.Mutex
	partial []byte
}

// maxRoleLine bounds a line without a newline; a longer one is passed on as
// it is.
const maxRoleLine = 8 << 10

func (w *roleLineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.partial = append(w.partial, p...)
	for {
		end := bytes.IndexByte(w.partial, '\n')
		if end < 0 {
			if len(w.partial) > maxRoleLine {
				_, _ = w.rest.Write(w.partial)
				w.partial = w.partial[:0]
			}
			break
		}
		line := w.partial[:end]
		if record, ok := bytes.CutPrefix(line, []byte(roleLinePrefix)); ok {
			w.tracker.record(string(record))
		} else {
			_, _ = w.rest.Write(w.partial[:end+1])
		}
		w.partial = w.partial[end+1:]
	}
	if len(w.partial) == 0 {
		w.partial = nil
	}
	return len(p), nil
}

// flush passes on a last line that had no newline.
func (w *roleLineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.partial) > 0 {
		_, _ = w.rest.Write(w.partial)
		w.partial = nil
	}
}
