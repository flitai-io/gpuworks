package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/cardhealth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type fakeCardHealth struct {
	nodes     map[string]cardhealth.NodeSnapshot
	live      []cardhealth.Episode
	detail    proto.CardCheckDetail
	detailErr error
	asked     []string
	forgotten []string
}

func (f *fakeCardHealth) Node(uuid string) (cardhealth.NodeSnapshot, bool) {
	n, ok := f.nodes[uuid]
	return n, ok
}

func (f *fakeCardHealth) LiveEpisodes(uint) []cardhealth.Episode { return f.live }

func (f *fakeCardHealth) Detail(_ context.Context, uuid, ident, check string) (proto.CardCheckDetail, error) {
	f.asked = append(f.asked, uuid+"/"+ident+"/"+check)
	return f.detail, f.detailErr
}

func (f *fakeCardHealth) ForgetNode(uuid string, _ uint) { f.forgotten = append(f.forgotten, uuid) }

// cardHealthFixture is node drain-a with two cards in the store and a report
// of their checks in memory: card 0 fine, card 1's PCIe link warning, NVLink
// not applicable to either.
func cardHealthFixture(t *testing.T) (*apiHandlers, *store.Store, *fakeCardHealth, *bool, *[]string) {
	t.Helper()
	a, st, _ := schedulingFixture(t)
	ok := true
	cards := []proto.GPUInfo{
		{Index: 0, UUID: "GPU-a", Name: "H20", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", MemoryTotalBytes: 24 << 30, Healthy: true},
		{Index: 1, UUID: "GPU-b", Name: "H20", Vendor: "nvidia", PCIeBDF: "0000:40:00.0", MemoryTotalBytes: 24 << 30, Healthy: true},
	}
	if err := st.UpdateNodeStatus("drain-a", proto.NodeStatus{GPUProbeOK: &ok, GPUs: cards, Sys: proto.SysInfo{RuntimeDriver: "docker"}}); err != nil {
		t.Fatal(err)
	}
	node, _ := st.NodeByUUID("drain-a")
	received := time.Now().Add(-10 * time.Second).UTC().Truncate(time.Second)
	checks := func(link string, reason string) []proto.CardCheck {
		return []proto.CardCheck{
			{ID: accelerator.CheckVendorHealth, Status: proto.CheckPass},
			{ID: accelerator.CheckPCIeLink, Status: link, Code: reason},
			{ID: accelerator.CheckNVLink, Status: proto.CheckNA},
			{ID: accelerator.CheckRuntime, Status: proto.CheckPass, AgeS: 120, MS: 812},
		}
	}
	fake := &fakeCardHealth{nodes: map[string]cardhealth.NodeSnapshot{"drain-a": {
		NodeID: node.ID, ReceivedAt: received, Online: true, Reports: true,
		Cards: []cardhealth.CardSnapshot{
			{Ident: accelerator.CardIdent("GPU-a", "0000:3f:00.0"), UUID: "GPU-a", PCIeBDF: "0000:3f:00.0", Name: "H20", Index: 0, Checks: checks(proto.CheckPass, "")},
			{Ident: accelerator.CardIdent("GPU-b", "0000:40:00.0"), UUID: "GPU-b", PCIeBDF: "0000:40:00.0", Name: "H20", Index: 1, Checks: checks(proto.CheckWarn, accelerator.HealthPCIeLinkDegraded)},
		},
	}}}
	online := true
	caps := []string{proto.CapCardChecks}
	a.d.CardHealth = fake
	a.d.Online = func(string) bool { return online }
	a.d.NodeCaps = func(string) []string { return caps }
	return a, st, fake, &online, &caps
}

func decode(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return out
}

// The table: only the columns some card has, each card's cells under them,
// what the server worked out (worst, can_recheck, stale) included.
func TestNodeHealthChecks(t *testing.T) {
	a, _, _, online, caps := cardHealthFixture(t)
	code, body := callNode(t, a.nodeHealthChecks, http.MethodGet, "drain-a", "/health-checks", "")
	if code != http.StatusOK {
		t.Fatalf("status %d %s", code, body)
	}
	out := decode(t, body)
	var cols []string
	for _, c := range out["checks"].([]any) {
		cols = append(cols, c.(map[string]any)["id"].(string))
	}
	if strings.Join(cols, ",") != "vendor_health,pcie_link,runtime" {
		t.Fatalf("columns = %v, want the inapplicable NVLink left out", cols)
	}
	if out["checks"].([]any)[2].(map[string]any)["period_seconds"] != float64(900) {
		t.Fatalf("runtime period = %v", out["checks"])
	}
	cards := out["cards"].([]any)
	second := cards[1].(map[string]any)
	if second["worst"] != "warn" || second["can_recheck"] != true || second["pcie_bdf"] != "0000:40:00.0" {
		t.Fatalf("second card = %v", second)
	}
	link := second["cells"].([]any)[1].(map[string]any)
	if link["status"] != "warn" || link["code"] != "PCIE_LINK_DEGRADED" || link["stale"] != false || link["checked_at"] == nil || link["state"] != nil || link["reason"] != nil {
		t.Fatalf("link cell = %v", link)
	}
	runtime := second["cells"].([]any)[2].(map[string]any)
	if runtime["duration_ms"] != float64(812) {
		t.Fatalf("runtime cell = %v", runtime)
	}
	if out["summary"].(map[string]any)["warn"] != float64(1) || out["summary"].(map[string]any)["pass"] != float64(5) {
		t.Fatalf("summary = %v", out["summary"])
	}

	// One card, by its address.
	_, body = callNode(t, a.nodeHealthChecks, http.MethodGet, "drain-a", "/health-checks?pcie_bdf=0000:3F:00.0", "")
	if cards := decode(t, body)["cards"].([]any); len(cards) != 1 || cards[0].(map[string]any)["index"] != float64(0) {
		t.Fatalf("one card = %v", cards)
	}

	// Offline: the last states, stale, and nothing to recheck.
	*online = false
	_, body = callNode(t, a.nodeHealthChecks, http.MethodGet, "drain-a", "/health-checks", "")
	out = decode(t, body)
	first := out["cards"].([]any)[0].(map[string]any)
	if out["online"] != false || out["stale"] != true || first["can_recheck"] != false || first["cells"].([]any)[0].(map[string]any)["stale"] != true {
		t.Fatalf("offline = %v", out)
	}
	*online, *caps = true, nil
	_, body = callNode(t, a.nodeHealthChecks, http.MethodGet, "drain-a", "/health-checks", "")
	if decode(t, body)["cards"].([]any)[0].(map[string]any)["can_recheck"] != false {
		t.Fatal("an old agent offered a recheck")
	}

	if code, _ := callNode(t, a.nodeHealthChecks, http.MethodGet, "nope", "/health-checks", ""); code != http.StatusNotFound {
		t.Fatalf("unknown node = %d", code)
	}
}

// A node that has not reported yet has an empty table, not an error.
func TestNodeHealthChecksBeforeAnyReport(t *testing.T) {
	a, _, _, _, _ := cardHealthFixture(t)
	code, body := callNode(t, a.nodeHealthChecks, http.MethodGet, "drain-b", "/health-checks", "")
	want := `{"node_uuid":"drain-b","online":true,"reports":false,"stale":true,"checks":[],"cards":[],"summary":{},"notice":{"code":"CARD_CHECKS_WAITING","level":"info"}}`
	if code != http.StatusOK || strings.TrimSpace(body) != want {
		t.Fatalf("= %d %s\nwant %s", code, body, want)
	}
}

// The failure records: open first, newest next; what memory holds is newer
// than the row; a card no longer on the node says so.
func TestNodeHealthRecords(t *testing.T) {
	a, st, fake, _, _ := cardHealthFixture(t)
	node, _ := st.NodeByUUID("drain-a")
	started := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	ended := started.Add(30 * time.Minute)
	closed := &store.CardHealthEpisode{NodeID: node.ID, CardIdent: "uuid:GPU-gone", CheckID: "runtime", Level: "fail", Reason: accelerator.HealthRuntimeUnusable,
		StartedAt: started, EndedAt: &ended, LastSeenAt: ended, Count: 3, FirstEvidence: "Detail=cuCtxCreate=999", LatestEvidence: "Detail=cuCtxCreate=999"}
	open := &store.CardHealthEpisode{NodeID: node.ID, CardIdent: accelerator.CardIdent("GPU-b", "0000:40:00.0"), CheckID: "pcie_link", Level: "warn",
		Reason: accelerator.HealthPCIeLinkDegraded, StartedAt: started, LastSeenAt: started, Count: 1}
	if err := st.SaveCardHealthEpisodes([]*store.CardHealthEpisode{closed, open}); err != nil {
		t.Fatal(err)
	}
	fake.live = []cardhealth.Episode{{ID: open.ID, NodeID: node.ID, CardIdent: open.CardIdent, CheckID: open.CheckID, Status: open.Level,
		Code: open.Reason, StartedAt: open.StartedAt, LastSeenAt: open.LastSeenAt, Count: 50, Flapping: true}}

	code, body := callNode(t, a.nodeHealthRecords, http.MethodGet, "drain-a", "/health-records", "")
	if code != http.StatusOK {
		t.Fatalf("status %d %s", code, body)
	}
	out := decode(t, body)
	records := out["records"].([]any)
	if out["total"] != float64(2) || out["retention_days"] != float64(30) || len(records) != 2 {
		t.Fatalf("page = %v", out)
	}
	first, second := records[0].(map[string]any), records[1].(map[string]any)
	if first["ongoing"] != true || first["count"] != float64(50) || first["flapping"] != true || first["card_index"] != float64(1) || first["removed"] != false {
		t.Fatalf("open record = %v", first)
	}
	if first["status"] != "warn" || first["code"] != "PCIE_LINK_DEGRADED" || first["level"] != nil || first["reason"] != nil {
		t.Fatalf("open record result = %v", first)
	}
	if second["status"] != "fail" || second["code"] != "RUNTIME_UNUSABLE" {
		t.Fatalf("closed record = %v", second)
	}
	if second["removed"] != true || second["duration_seconds"] != float64(1800) ||
		second["first_evidence"].([]any)[0].(map[string]any)["value"] != "cuCtxCreate=999" {
		t.Fatalf("closed record = %v", second)
	}

	_, body = callNode(t, a.nodeHealthRecords, http.MethodGet, "drain-a", "/health-records?check=runtime&page_size=1", "")
	if out := decode(t, body); out["total"] != float64(1) || out["page_size"] != float64(1) {
		t.Fatalf("one check = %v", out)
	}
	if code, body := callNode(t, a.nodeHealthRecords, http.MethodGet, "drain-a", "/health-records?check=bogus", ""); code != http.StatusBadRequest || !strings.Contains(body, ErrorFieldInvalid) {
		t.Fatalf("bogus check = %d %s", code, body)
	}
	if code, _ := callNode(t, a.nodeHealthRecords, http.MethodGet, "drain-a", "/health-records?page=0", ""); code != http.StatusBadRequest {
		t.Fatalf("page 0 = %d", code)
	}
	// A page whose offset overflows an int is past the end, not page one.
	code, body = callNode(t, a.nodeHealthRecords, http.MethodGet, "drain-a", "/health-records?page=9223372036854775807&page_size=200", "")
	if out := decode(t, body); code != http.StatusOK || out["total"] != float64(2) || len(out["records"].([]any)) != 0 {
		t.Fatalf("huge page = %d %v", code, out)
	}
}

// The detail asks the agent through the bounded fetcher, and is refused with
// the reason when the agent cannot be asked.
func TestNodeHealthCheckDetail(t *testing.T) {
	a, _, fake, online, caps := cardHealthFixture(t)
	fake.detail = proto.CardCheckDetail{Ident: "uuid:GPU-a", Check: "runtime", AgentStartedAt: "2026-11-01T00:00:00Z",
		Last: &proto.CardCheckRun{Status: "pass", At: "2026-11-01T01:00:00Z", DurationMS: 812, Probe: "python3 -c ...", Evidence: []proto.DoctorField{{Name: "Detail", Value: "ok"}}}}
	code, body := callNode(t, a.nodeHealthCheckDetail, http.MethodGet, "drain-a", "/health-checks/detail?uuid=GPU-a&check=runtime", "")
	want := `{"ident":"uuid:GPU-a","check":"runtime","last":{"status":"pass","at":"2026-11-01T01:00:00Z","duration_ms":812,"probe":"python3 -c ...","evidence":[{"name":"Detail","value":"ok"}]},"agent_started_at":"2026-11-01T00:00:00Z"}`
	if code != http.StatusOK || strings.TrimSpace(body) != want {
		t.Fatalf("= %d %s\nwant %s", code, body, want)
	}
	if len(fake.asked) != 1 || fake.asked[0] != "drain-a/"+accelerator.CardIdent("GPU-a", "")+"/runtime" {
		t.Fatalf("asked %v", fake.asked)
	}

	for name, tc := range map[string]struct {
		query string
		setup func()
		code  int
		error string
	}{
		"no card":     {query: "check=runtime", code: http.StatusBadRequest, error: ErrorFieldInvalid},
		"bogus check": {query: "uuid=GPU-a&check=bogus", code: http.StatusBadRequest, error: ErrorFieldInvalid},
		"offline":     {query: "uuid=GPU-a&check=runtime", setup: func() { *online = false }, code: http.StatusConflict, error: ErrorNodeOffline},
		"old agent":   {query: "uuid=GPU-a&check=runtime", setup: func() { *caps = nil }, code: http.StatusConflict, error: ErrorCardChecksUnsupported},
		"busy":        {query: "uuid=GPU-a&check=runtime", setup: func() { fake.detailErr = cardhealth.ErrBusy }, code: http.StatusTooManyRequests},
		"agent error": {query: "uuid=GPU-a&check=runtime", setup: func() { fake.detailErr = errors.New("rpc timeout") }, code: http.StatusBadGateway},
	} {
		*online, *caps, fake.detailErr = true, []string{proto.CapCardChecks}, nil
		fake.asked = nil
		if tc.setup != nil {
			tc.setup()
		}
		code, body := callNode(t, a.nodeHealthCheckDetail, http.MethodGet, "drain-a", "/health-checks/detail?"+tc.query, "")
		if code != tc.code || (tc.error != "" && !strings.Contains(body, tc.error)) {
			t.Fatalf("%s = %d %s", name, code, body)
		}
		if (name == "offline" || name == "old agent") && len(fake.asked) != 0 {
			t.Fatalf("%s asked the agent anyway", name)
		}
	}
}

// A recheck calls the agent's RPC with the card's identity and returns what
// it answered.
func TestRecheckCard(t *testing.T) {
	a, _, _, online, _ := cardHealthFixture(t)
	var calls []string
	a.d.CallNode = func(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
		raw, _ := json.Marshal(params)
		calls = append(calls, uuid+" "+method+" "+string(raw))
		// An agent that predates check statuses: its skip reason in words.
		return json.RawMessage(`{"ident":"uuid:GPU-b","check":"runtime","last":{"state":"skip","reason":"busy","at":"2026-11-01T01:00:00Z"},"agent_started_at":"2026-11-01T00:00:00Z"}`), nil
	}
	code, body := callNode(t, a.recheckCard, http.MethodPost, "drain-a", "/gpus/recheck", `{"uuid":"GPU-b"}`)
	if code != http.StatusOK || !strings.Contains(body, `"last":{"status":"skip","code":"CARD_CHECK_SKIPPED_BUSY","at":"2026-11-01T01:00:00Z"`) {
		t.Fatalf("= %d %s", code, body)
	}
	if len(calls) != 1 || calls[0] != `drain-a node.card_recheck {"ident":"uuid:GPU-b"}` {
		t.Fatalf("calls = %v", calls)
	}
	if code, _ := callNode(t, a.recheckCard, http.MethodPost, "drain-a", "/gpus/recheck", `{}`); code != http.StatusBadRequest {
		t.Fatalf("no card = %d", code)
	}
	if code, _ := callNode(t, a.recheckCard, http.MethodPost, "drain-a", "/gpus/recheck", `{"uuid":"GPU-b","extra":1}`); code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", code)
	}
	*online = false
	if code, body := callNode(t, a.recheckCard, http.MethodPost, "drain-a", "/gpus/recheck", `{"uuid":"GPU-b"}`); code != http.StatusConflict || !strings.Contains(body, ErrorNodeOffline) {
		t.Fatalf("offline = %d %s", code, body)
	}
	*online = true
	a.d.CallNode = func(context.Context, string, string, any) (json.RawMessage, error) {
		return nil, errors.New("agent gone")
	}
	if code, _ := callNode(t, a.recheckCard, http.MethodPost, "drain-a", "/gpus/recheck", `{"uuid":"GPU-b"}`); code != http.StatusConflict {
		t.Fatalf("rpc error = %d", code)
	}
	if code, _ := callNode(t, a.recheckCard, http.MethodPost, "nope", "/gpus/recheck", `{"uuid":"GPU-b"}`); code != http.StatusNotFound {
		t.Fatalf("unknown node = %d", code)
	}
}
