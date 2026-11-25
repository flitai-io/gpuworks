package cluster

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// captured collects slog records with their attributes as written.
type captured struct {
	mu      sync.Mutex
	records []capturedRecord
}

type capturedRecord struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

func (c *captured) Enabled(context.Context, slog.Level) bool { return true }
func (c *captured) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *captured) WithGroup(string) slog.Handler            { return c }
func (c *captured) Handle(_ context.Context, r slog.Record) error {
	rec := capturedRecord{level: r.Level, msg: r.Message, attrs: map[string]any{}}
	r.Attrs(func(a slog.Attr) bool { rec.attrs[a.Key] = a.Value.Any(); return true })
	c.mu.Lock()
	c.records = append(c.records, rec)
	c.mu.Unlock()
	return nil
}

func (c *captured) named(msg string) []capturedRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []capturedRecord
	for _, r := range c.records {
		if r.msg == msg {
			out = append(out, r)
		}
	}
	return out
}

func trackerFor(t *testing.T) (*roleTracker, *captured) {
	t.Helper()
	sink := &captured{}
	logger := slog.New(sink)
	return newRoleTracker(func() *slog.Logger { return logger }), sink
}

func TestParseRoleRecord(t *testing.T) {
	cases := []struct {
		record string
		want   []roleField
	}{
		{"at=1700000000000000000 event=step_down reason=no_majority_contact peer_ack_age_ms=2:voter:812,3:voter:-1",
			[]roleField{{"at", "1700000000000000000"}, {"event", "step_down"}, {"reason", "no_majority_contact"}, {"peer_ack_age_ms", "2:voter:812,3:voter:-1"}}},
		{`at=1 event=app level=warn text_key=0a0b0c0d text="adjust roles: no \"leader\" = found"`,
			[]roleField{{"at", "1"}, {"event", "app"}, {"level", "warn"}, {"text_key", "0a0b0c0d"}, {"text", `adjust roles: no "leader" = found`}}},
		{"  at=1   event=state  ", []roleField{{"at", "1"}, {"event", "state"}}},
		{"word at=1 tail", []roleField{{"text", "word"}, {"at", "1"}, {"text", "tail"}}},
		{`at=1 text="unterminated`, []roleField{{"at", "1"}, {"text", `"unterminated`}}},
		{"empty= next=1", []roleField{{"empty", ""}, {"next", "1"}}},
		{"", nil},
	}
	for _, tc := range cases {
		if got := parseRoleRecord(tc.record); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseRoleRecord(%q)\n got %v\nwant %v", tc.record, got, tc.want)
		}
	}
}

// Every record is filed with its fields as attributes, at the level of its
// event, and with the time it happened instead of the raw number.
func TestRoleRecordsAreFiledWithTheirFieldsAndLevel(t *testing.T) {
	tracker, sink := trackerFor(t)
	tracker.record("at=1700000000123456789 event=step_down reason=no_majority_contact term=7 contacts=1 voters=3 peer_ack_age_ms=2:voter:812,3:voter:1304")
	tracker.record("at=1700000000223456789 event=vote_reply candidate=2 granted=0 reason=has_leader pre_vote=1")
	tracker.record(`at=1700000000323456789 event=app level=error text_key=1 text="proxy: reset"`)
	tracker.record("at=1700000000423456789 event=never_heard_of x=1")
	got := sink.named("cluster database role event")
	if len(got) != 4 {
		t.Fatalf("filed %d records", len(got))
	}
	want := map[string]any{
		"event": "step_down", "reason": "no_majority_contact", "term": "7", "contacts": "1", "voters": "3",
		"peer_ack_age_ms": "2:voter:812,3:voter:1304",
		"observed_at":     time.Unix(0, 1700000000123456789).Format(time.RFC3339Nano),
	}
	if !reflect.DeepEqual(got[0].attrs, want) || got[0].level != slog.LevelWarn {
		t.Fatalf("step_down filed at %v as %v", got[0].level, got[0].attrs)
	}
	if got[1].level != slog.LevelInfo || got[2].level != slog.LevelError || got[3].level != slog.LevelInfo {
		t.Fatalf("levels %v %v %v", got[1].level, got[2].level, got[3].level)
	}
	if got[2].attrs["text"] != "proxy: reset" || got[3].attrs["event"] != "never_heard_of" {
		t.Fatalf("attrs %v %v", got[2].attrs, got[3].attrs)
	}
}

// at is the time field of a record this many milliseconds after a fixed start.
func at(ms int64) string {
	return "at=" + strconv.FormatInt(time.Unix(1700000000, 0).Add(time.Duration(ms)*time.Millisecond).UnixNano(), 10)
}

// The summary names the leader before and after, the reason as this member
// saw it and the time this member had no leader, for each way a leader ends.
func TestLeaderChangeSummary(t *testing.T) {
	cases := []struct {
		name    string
		records []string
		want    []map[string]any
	}{
		{
			name: "a follower's leader goes silent and the follower wins",
			records: []string{
				at(0) + " event=state old=UNAVAILABLE new=FOLLOWER term=3 id=2 address=b:9001",
				at(100) + " event=following leader=1 leader_address=a:9001 term=3",
				at(5000) + " event=election_start reason=election_timeout term=3 leader=1 since_leader_contact_ms=1180 timeout_ms=1100",
				at(5001) + " event=state old=FOLLOWER new=CANDIDATE term=3 id=2 address=b:9001",
				at(5030) + " event=pre_vote_won term=3 next_term=4",
				at(5060) + " event=election_won term=4 votes=2 voters=3",
				at(5061) + " event=state old=CANDIDATE new=LEADER term=4 id=2 address=b:9001",
			},
			want: []map[string]any{
				{"old_leader": "none", "new_leader": "1", "term": "3", "this_member": "2", "leads": false,
					"reason": "first leader this member has seen since its database process started", "without_leader_ms": int64(100)},
				{"old_leader": "1", "new_leader": "2", "term": "4", "this_member": "2", "leads": true,
					"reason":            "this member started an election: election_timeout since_leader_contact_ms=1180 timeout_ms=1100",
					"without_leader_ms": int64(61)},
			},
		},
		{
			name: "a leader that reaches no majority steps down and follows the next",
			records: []string{
				at(0) + " event=state old=UNAVAILABLE new=FOLLOWER term=1 id=1 address=a:9001",
				at(10) + " event=state old=FOLLOWER new=CANDIDATE term=1 id=1 address=a:9001",
				at(20) + " event=state old=CANDIDATE new=LEADER term=2 id=1 address=a:9001",
				at(9000) + " event=step_down reason=no_majority_contact term=2 contacts=1 voters=3 election_timeout_ms=750 since_check_ms=751 peer_ack_age_ms=2:voter:812,3:voter:1304",
				at(9000) + " event=state old=LEADER new=FOLLOWER term=2 id=1 address=a:9001",
				at(9400) + " event=term_bump term=2 new_term=3 from=3 from_address=c:9001",
				at(9900) + " event=following leader=3 leader_address=c:9001 term=3",
			},
			want: []map[string]any{
				{"old_leader": "none", "new_leader": "1", "term": "2", "this_member": "1", "leads": true,
					"reason": "first leader this member has seen since its database process started", "without_leader_ms": int64(20)},
				{"old_leader": "1", "new_leader": "3", "term": "3", "this_member": "1", "leads": false,
					"reason":            "this member stepped down: no_majority_contact contacts=1 voters=3 peer_ack_age_ms=2:voter:812,3:voter:1304",
					"without_leader_ms": int64(900)},
			},
		},
		{
			name: "a bystander votes for a candidate that was asked to take over",
			records: []string{
				at(0) + " event=state old=UNAVAILABLE new=FOLLOWER term=5 id=3 address=c:9001",
				at(1) + " event=following leader=1 leader_address=a:9001 term=5",
				at(7000) + " event=term_bump term=5 new_term=6 from=2 from_address=b:9001",
				at(7000) + " event=vote_reply candidate=2 granted=1 reason=granted pre_vote=0 disrupt_leader=1 request_term=6 term=6",
				at(7040) + " event=following leader=2 leader_address=b:9001 term=6",
			},
			want: []map[string]any{
				{"old_leader": "none", "new_leader": "1", "term": "5", "this_member": "3", "leads": false,
					"reason": "first leader this member has seen since its database process started", "without_leader_ms": int64(1)},
				{"old_leader": "1", "new_leader": "2", "term": "6", "this_member": "3", "leads": false,
					"reason": "another member raised the term from=2 new_term=6", "without_leader_ms": int64(40)},
			},
		},
		{
			name: "the leader is asked to hand over",
			records: []string{
				at(0) + " event=state old=UNAVAILABLE new=FOLLOWER term=1 id=1 address=a:9001",
				at(20) + " event=state old=CANDIDATE new=LEADER term=2 id=1 address=a:9001",
				at(4000) + " event=transfer_start to=2 to_address=b:9001 term=2 target_up_to_date=1",
				at(4030) + " event=step_down reason=higher_term was=leader term=2 new_term=3 from=2 from_address=b:9001",
				at(4030) + " event=state old=LEADER new=FOLLOWER term=3 id=1 address=a:9001",
				at(4031) + " event=transfer_end to=2 result=leadership_left term=3 after_ms=31",
				at(4100) + " event=following leader=2 leader_address=b:9001 term=3",
			},
			want: []map[string]any{
				{"old_leader": "none", "new_leader": "1", "term": "2", "this_member": "1", "leads": true,
					"reason": "first leader this member has seen since its database process started", "without_leader_ms": int64(20)},
				{"old_leader": "1", "new_leader": "2", "term": "3", "this_member": "1", "leads": false,
					"reason": "this member stepped down: higher_term from=2 new_term=3", "without_leader_ms": int64(70)},
			},
		},
		{
			name: "this member steps down and wins again",
			records: []string{
				at(0) + " event=state old=UNAVAILABLE new=FOLLOWER term=1 id=1 address=a:9001",
				at(20) + " event=election_won term=2 votes=2 voters=3",
				at(20) + " event=state old=CANDIDATE new=LEADER term=2 id=1 address=a:9001",
				at(9000) + " event=step_down reason=no_majority_contact term=2 contacts=1 voters=3 election_timeout_ms=750 since_check_ms=751 peer_ack_age_ms=2:voter:812,3:voter:1304",
				at(9000) + " event=state old=LEADER new=FOLLOWER term=2 id=1 address=a:9001",
				at(10100) + " event=election_start reason=election_timeout term=2 leader=0 since_leader_contact_ms=1100 timeout_ms=1000",
				at(10100) + " event=state old=FOLLOWER new=CANDIDATE term=2 id=1 address=a:9001",
				at(10150) + " event=election_won term=3 votes=2 voters=3",
				at(10150) + " event=state old=CANDIDATE new=LEADER term=3 id=1 address=a:9001",
			},
			want: []map[string]any{
				{"old_leader": "none", "new_leader": "1", "term": "2", "this_member": "1", "leads": true,
					"reason": "first leader this member has seen since its database process started", "without_leader_ms": int64(20)},
				{"old_leader": "1", "new_leader": "1", "term": "3", "old_term": "2", "this_member": "1", "leads": true,
					"reason":            "this member started an election: election_timeout since_leader_contact_ms=1100 timeout_ms=1000",
					"without_leader_ms": int64(1150)},
			},
		},
		{
			name: "the same leader after a re-election this member only heard of late",
			records: []string{
				at(0) + " event=state old=UNAVAILABLE new=FOLLOWER term=5 id=3 address=c:9001",
				at(1) + " event=following leader=1 leader_address=a:9001 term=5",
				at(60000) + " event=term_bump term=5 new_term=7 from=1 from_address=a:9001",
				at(60010) + " event=following leader=1 leader_address=a:9001 term=7",
				at(200000) + " event=following leader=2 leader_address=b:9001 term=9",
			},
			want: []map[string]any{
				{"old_leader": "none", "new_leader": "1", "term": "5", "this_member": "3", "leads": false,
					"reason": "first leader this member has seen since its database process started", "without_leader_ms": int64(1)},
				// The leader lost the role and won it again: a change,
				// although the same member leads.
				{"old_leader": "1", "new_leader": "1", "term": "7", "old_term": "5", "this_member": "3", "leads": false,
					"reason": "another member raised the term from=1 new_term=7", "without_leader_ms": int64(10)},
				{"old_leader": "1", "new_leader": "2", "term": "9", "this_member": "3", "leads": false,
					"reason": "not seen by this member; read the other members' role events around this time"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tracker, sink := trackerFor(t)
			for _, record := range tc.records {
				tracker.record(record)
			}
			// A leader kept after a campaign is filed under its own
			// message, so that a search for leader changes finds only
			// changes.
			var got []capturedRecord
			sink.mu.Lock()
			for _, r := range sink.records {
				switch r.msg {
				case "cluster database leader changed":
					// The same member twice is a change only across terms.
					if r.attrs["old_leader"] == r.attrs["new_leader"] && (r.attrs["old_term"] == nil || r.attrs["old_term"] == r.attrs["term"]) {
						t.Fatalf("a leader change names the same leader in the same term: %v", r.attrs)
					}
					got = append(got, r)
				case "cluster database leader kept after a campaign":
					r.attrs["kept"] = true
					got = append(got, r)
				}
			}
			sink.mu.Unlock()
			if len(got) != len(tc.want) {
				t.Fatalf("%d summaries, want %d: %v", len(got), len(tc.want), got)
			}
			for i, want := range tc.want {
				if _, ok := got[i].attrs["observed_at"].(string); !ok {
					t.Fatalf("summary %d has no time: %v", i, got[i].attrs)
				}
				delete(got[i].attrs, "observed_at")
				if !reflect.DeepEqual(got[i].attrs, want) {
					t.Errorf("summary %d\n got %v\nwant %v", i, got[i].attrs, want)
				}
			}
		})
	}
}

// Role records leave the process' standard error for the log; every other
// byte passes through unchanged and in order, however the writes are cut.
func TestRoleLineWriterSeparatesRecordsFromOtherOutput(t *testing.T) {
	input := "starting\n" + roleLinePrefix + "at=1 event=state old=UNAVAILABLE new=FOLLOWER term=1 id=9 address=x\n" +
		"panic: something\n\tgoroutine 1\n" + roleLinePrefix + "at=2 event=following leader=4 leader_address=y term=1\n" +
		"not a " + roleLinePrefix + "record\nlast line without newline"
	for _, chunk := range []int{1, 3, 7, 64, len(input)} {
		tracker, sink := trackerFor(t)
		var rest bytes.Buffer
		w := &roleLineWriter{tracker: tracker, rest: &rest}
		for i := 0; i < len(input); i += chunk {
			end := min(i+chunk, len(input))
			if n, err := w.Write([]byte(input[i:end])); n != end-i || err != nil {
				t.Fatalf("Write = %d, %v", n, err)
			}
		}
		if got, want := rest.String(), "starting\npanic: something\n\tgoroutine 1\nnot a "+roleLinePrefix+"record\n"; got != want {
			t.Fatalf("chunk %d: passed on %q, want %q", chunk, got, want)
		}
		w.flush()
		if !strings.HasSuffix(rest.String(), "\nlast line without newline") {
			t.Fatalf("chunk %d: the unfinished line was lost: %q", chunk, rest.String())
		}
		if got := sink.named("cluster database role event"); len(got) != 2 || got[1].attrs["leader"] != "4" {
			t.Fatalf("chunk %d: filed %v", chunk, got)
		}
		if got := sink.named("cluster database leader changed"); len(got) != 1 || got[0].attrs["new_leader"] != "4" {
			t.Fatalf("chunk %d: summaries %v", chunk, got)
		}
	}
}

// A line that never ends is passed on instead of being kept without bound.
func TestRoleLineWriterDoesNotHoldAnEndlessLine(t *testing.T) {
	tracker, _ := trackerFor(t)
	var rest bytes.Buffer
	w := &roleLineWriter{tracker: tracker, rest: &rest}
	long := strings.Repeat("x", maxRoleLine+10)
	_, _ = w.Write([]byte(long))
	if rest.Len() != len(long) || len(w.partial) != 0 {
		t.Fatalf("passed on %d of %d bytes, kept %d", rest.Len(), len(long), len(w.partial))
	}
}

// A member that campaigns in vain and returns to its leader, as one whose loop
// was late does, leaves no "leader changed" line.
func TestAVainCampaignIsNotALeaderChange(t *testing.T) {
	tracker, sink := trackerFor(t)
	for _, record := range []string{
		at(0) + " event=state old=UNAVAILABLE new=FOLLOWER term=4 id=2 address=b:9001",
		at(600) + " event=following leader=3 leader_address=c:9001 term=4",
		at(3000) + " event=loop_late late_ms=2152 period_ms=75",
		at(3000) + " event=election_start reason=election_timeout term=4 leader=3 since_leader_contact_ms=2221 timeout_ms=1143",
		at(3000) + " event=state old=FOLLOWER new=CANDIDATE term=4 id=2 address=b:9001",
		at(3006) + " event=state old=CANDIDATE new=FOLLOWER term=4 id=2 address=b:9001",
		at(3007) + " event=following leader=3 leader_address=c:9001 term=4",
	} {
		tracker.record(record)
	}
	if changes := sink.named("cluster database leader changed"); len(changes) != 1 || changes[0].attrs["new_leader"] != "3" {
		t.Fatalf("leader changes: %v", changes)
	}
	kept := sink.named("cluster database leader kept after a campaign")
	if len(kept) != 1 {
		t.Fatalf("kept lines: %v", kept)
	}
	delete(kept[0].attrs, "observed_at")
	want := map[string]any{"leader": "3", "term": "4", "this_member": "2", "leads": false,
		"reason": "this member started an election: election_timeout since_leader_contact_ms=2221 timeout_ms=1143", "without_leader_ms": int64(7)}
	if !reflect.DeepEqual(kept[0].attrs, want) {
		t.Fatalf("kept line\n got %v\nwant %v", kept[0].attrs, want)
	}
}

// A database process that writes without limit fills at most its budget of a
// minute; the rest is counted and reported, and a change of leader inside the
// flood is still written.
func TestRoleRecordsHaveABudgetPerMinute(t *testing.T) {
	tracker, sink := trackerFor(t)
	clock := time.Unix(1700000000, 0)
	tracker.now = func() time.Time { return clock }
	tracker.record(at(0) + " event=state old=UNAVAILABLE new=FOLLOWER term=1 id=2 address=b:9001")
	for i := 0; i < 3*roleRecordsPerMinute; i++ {
		tracker.record(at(int64(i)) + " event=vote_reply candidate=9 granted=0 reason=has_leader pre_vote=1")
	}
	tracker.record(at(5000) + " event=following leader=3 leader_address=c:9001 term=1")
	// The budget, and the record that names the new leader outside it.
	if filed := len(sink.named("cluster database role event")); filed != roleRecordsPerMinute+1 {
		t.Fatalf("filed %d records in one minute, want %d and the leader's", filed, roleRecordsPerMinute)
	}
	if changes := sink.named("cluster database leader changed"); len(changes) != 1 {
		t.Fatalf("the leader change inside the flood was not written: %v", changes)
	}
	if early := sink.named("cluster database role events suppressed"); len(early) != 0 {
		t.Fatalf("reported before the minute ended: %v", early)
	}
	clock = clock.Add(time.Minute)
	tracker.record(at(60000) + " event=snapshot_take index=1 status=ok took_ms=300")
	report := sink.named("cluster database role events suppressed")
	if len(report) != 1 || report[0].attrs["suppressed"] != int64(2*roleRecordsPerMinute+1) || report[0].level != slog.LevelWarn {
		t.Fatalf("report %v", report)
	}
	if filed := len(sink.named("cluster database role event")); filed != roleRecordsPerMinute+2 {
		t.Fatalf("the next minute's record was not filed: %d", filed)
	}
}

// A leader that loses the majority steps down and nobody can be elected, so
// no summary follows. With the minute's budget spent, the records of that
// step-down are still filed.
func TestAStepDownWithoutASuccessorIsFiledPastTheBudget(t *testing.T) {
	tracker, sink := trackerFor(t)
	clock := time.Unix(1700000000, 0)
	tracker.now = func() time.Time { return clock }
	tracker.record(at(0) + " event=state old=UNAVAILABLE new=FOLLOWER term=1 id=1 address=a:9001")
	tracker.record(at(10) + " event=election_won term=2 votes=2 voters=3")
	tracker.record(at(10) + " event=state old=CANDIDATE new=LEADER term=2 id=1 address=a:9001")
	for i := 0; i < 2*roleRecordsPerMinute; i++ {
		tracker.record(at(20+int64(i)) + " event=vote_reply candidate=9 granted=0 reason=has_leader pre_vote=1")
	}
	before := len(sink.named("cluster database role event"))
	tracker.record(at(9000) + " event=step_down reason=no_majority_contact term=2 contacts=1 voters=3 peer_ack_age_ms=2:voter:812,3:voter:1304")
	tracker.record(at(9000) + " event=state old=LEADER new=FOLLOWER term=2 id=1 address=a:9001")
	tracker.record(at(9100) + " event=health trigger=role_change election_timeout_ms=750 loop_late_max_ms=1")
	tracker.record(at(9200) + " event=dropped lines=3 reason=reader_behind")
	filed := sink.named("cluster database role event")[before:]
	var events []string
	for _, r := range filed {
		events = append(events, r.attrs["event"].(string))
	}
	// health is not decisive and stays inside the spent budget.
	if strings.Join(events, ",") != "step_down,state,dropped" {
		t.Fatalf("filed past the budget: %v", events)
	}
	if filed[0].attrs["peer_ack_age_ms"] != "2:voter:812,3:voter:1304" || filed[0].level != slog.LevelWarn {
		t.Fatalf("step-down record: %v at %v", filed[0].attrs, filed[0].level)
	}
	if changes := sink.named("cluster database leader changed"); len(changes) != 1 {
		t.Fatalf("summaries without a successor: %v", changes)
	}
}

// The time without a leader is measured on the database process' monotonic
// clock when its records carry it, so a wall clock set backwards or forwards
// between the loss and the next leader does not show in it. Records without
// that clock fall back to the wall clock, never below zero.
func TestWithoutLeaderIsMeasuredOnTheMonotonicClock(t *testing.T) {
	cases := []struct {
		name    string
		lost    string
		found   string
		want    int64
		comment string
	}{
		{"wall clock set back 10 s in between", at(20000) + " mono=5000", at(10700) + " mono=5700", 700, ""},
		{"wall clock set forward an hour in between", at(20000) + " mono=5000", at(3620000) + " mono=5700", 700, ""},
		{"no monotonic clock, clock set back", at(20000), at(10700), 0, "clamped"},
		{"no monotonic clock", at(20000), at(20700), 700, ""},
		{"only the later record has it", at(20000), at(20700) + " mono=9", 700, "wall clock"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tracker, sink := trackerFor(t)
			tracker.record(at(0) + " mono=1 event=state old=UNAVAILABLE new=FOLLOWER term=1 id=2 address=b:9001")
			tracker.record(at(10) + " mono=11 event=following leader=1 leader_address=a:9001 term=1")
			tracker.record(tc.lost + " event=election_start reason=election_timeout term=1 leader=1 since_leader_contact_ms=900 timeout_ms=800")
			tracker.record(tc.found + " event=following leader=3 leader_address=c:9001 term=2")
			changes := sink.named("cluster database leader changed")
			if len(changes) != 2 {
				t.Fatalf("changes: %v", changes)
			}
			if got := changes[1].attrs["without_leader_ms"]; got != tc.want {
				t.Fatalf("without_leader_ms = %v, want %d", got, tc.want)
			}
			for _, r := range sink.named("cluster database role event") {
				if _, filed := r.attrs["mono"]; filed {
					t.Fatalf("the raw monotonic stamp was filed: %v", r.attrs)
				}
			}
		})
	}
}

// Records that end or begin a leadership have a budget of their own. A
// database process that writes them without limit fills at most that budget
// in a minute, the first of each kind is filed even then, and the number left
// out is reported under its own kind.
func TestDecisiveRecordsHaveTheirOwnBudget(t *testing.T) {
	tracker, sink := trackerFor(t)
	clock := time.Unix(1700000000, 0)
	tracker.now = func() time.Time { return clock }
	tracker.record(at(0) + " event=state old=UNAVAILABLE new=FOLLOWER term=1 id=1 address=a:9001")
	const flood = roleDecisivePerMinute + 500
	for i := 0; i < flood; i++ {
		tracker.record(at(int64(i)) + " event=step_down reason=higher_term was=candidate term=1 new_term=2 from=2 from_address=b:9001")
	}
	// Another kind, first in this minute: filed although the budget is spent.
	tracker.record(at(5000) + " event=peer_io trigger=step_down peers=2:send=10:sent=10:recv=900:out=up/5000:in=1/5000")
	tracker.record(at(5001) + " event=peer_io trigger=step_down peers=2:send=10:sent=10:recv=901:out=up/5000:in=1/5000")
	// An ordinary record is still filed: the two budgets are separate.
	tracker.record(at(5002) + " event=snapshot_take index=1 status=ok took_ms=300")
	count := func(event string) (n int) {
		for _, r := range sink.named("cluster database role event") {
			if r.attrs["event"] == event {
				n++
			}
		}
		return n
	}
	if got := count("step_down"); got != roleDecisivePerMinute {
		t.Fatalf("filed %d step-down records in a minute, want %d", got, roleDecisivePerMinute)
	}
	if got := count("peer_io"); got != 1 {
		t.Fatalf("filed %d peer_io records past the budget, want the first of the kind", got)
	}
	if got := count("snapshot_take"); got != 1 {
		t.Fatalf("an ordinary record was held back by the decisive budget: %d", got)
	}
	clock = clock.Add(time.Minute)
	tracker.record(at(60000) + " event=snapshot_take index=2 status=ok took_ms=300")
	reports := sink.named("cluster database role events suppressed")
	if len(reports) != 1 || reports[0].attrs["suppressed"] != int64(500+1) || reports[0].attrs["kind"] != "leadership" || reports[0].attrs["per_minute"] != int64(roleDecisivePerMinute) {
		t.Fatalf("reports: %v", reports)
	}
	// A transport record that is not beside a role change is ordinary.
	if roleDecisive(parseRoleRecord("at=1 event=tcp trigger=interval peers=x")) != "" || roleDecisive(parseRoleRecord("at=1 event=tcp trigger=role_change peers=x")) != "tcp" {
		t.Fatal("which transport records are decisive")
	}
}
