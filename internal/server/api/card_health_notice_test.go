package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/cardhealth"
)

func TestNodeHealthNoticeDistinguishesCapabilityOwnershipAndReports(t *testing.T) {
	owned := proto.GPUInfo{UUID: "GPU-owned", PCIeBDF: "0000:08:00.0", Name: "RTX 3090", Healthy: true,
		Checks: []proto.CardCheck{{ID: accelerator.CheckRuntime, Status: proto.CheckPass}}}
	passed := proto.GPUInfo{UUID: "GPU-passed", PCIeBDF: "0000:09:00.0", Name: "RTX 3090", Ownership: accelerator.CardPassedThrough}
	noDriver := proto.GPUInfo{UUID: "GPU-no-driver", PCIeBDF: "0000:0a:00.0", Ownership: accelerator.CardWithoutDriver}
	held := proto.GPUInfo{UUID: "GPU-held", PCIeBDF: "0000:0b:00.0", Ownership: accelerator.CardHeldElsewhere}
	pending := owned
	pending.Checks = nil
	pending.UUID, pending.PCIeBDF = "GPU-pending", "0000:0c:00.0"
	cases := []struct {
		name, query, code, level string
		cards                    []proto.GPUInfo
		offline, unsupported     bool
		noReport, stale          bool
		wantCards                int
	}{
		{name: "all passthrough", cards: []proto.GPUInfo{passed}, code: "CARD_CHECKS_PASSTHROUGH", level: "info"},
		{name: "old agent with all passthrough", cards: []proto.GPUInfo{passed}, unsupported: true, code: "CARD_CHECKS_PASSTHROUGH", level: "info"},
		{name: "old agent without driver", cards: []proto.GPUInfo{noDriver}, unsupported: true, code: "CARD_CHECKS_NO_DRIVER", level: "info"},
		{name: "old agent with mixed unavailable ownership", cards: []proto.GPUInfo{passed, held}, unsupported: true, code: "CARD_CHECKS_NO_USABLE_CARDS", level: "info"},
		{name: "old agent selected passthrough beside host card", cards: []proto.GPUInfo{pending, passed}, unsupported: true, query: "?pcie_bdf=0000:09:00.0&uuid=GPU-passed", code: "CARD_CHECKS_PASSTHROUGH", level: "info"},
		{name: "without driver", cards: []proto.GPUInfo{noDriver}, code: "CARD_CHECKS_NO_DRIVER", level: "info"},
		{name: "held by another driver", cards: []proto.GPUInfo{held}, code: "CARD_CHECKS_HELD_ELSEWHERE", level: "info"},
		{name: "mixed unavailable ownership", cards: []proto.GPUInfo{passed, noDriver}, code: "CARD_CHECKS_NO_USABLE_CARDS", level: "info"},
		{name: "no GPUs", code: "CARD_CHECKS_NO_CARDS", level: "info"},
		{name: "old agent with cards", cards: []proto.GPUInfo{pending}, unsupported: true, code: "CARD_CHECKS_UNSUPPORTED", level: "info", wantCards: 1},
		{name: "old agent before report", noReport: true, unsupported: true, code: "CARD_CHECKS_UNSUPPORTED", level: "info"},
		{name: "waiting for first report", cards: []proto.GPUInfo{owned}, noReport: true, code: "CARD_CHECKS_WAITING", level: "info"},
		{name: "waiting for card checks", cards: []proto.GPUInfo{pending}, code: "CARD_CHECKS_WAITING", level: "info", wantCards: 1},
		{name: "ready", cards: []proto.GPUInfo{owned}, wantCards: 1},
		{name: "mixed usable and passthrough", cards: []proto.GPUInfo{owned, passed}, wantCards: 1},
		{name: "selected passthrough in mixed node", cards: []proto.GPUInfo{owned, passed}, query: "?pcie_bdf=0000:09:00.0&uuid=GPU-passed", code: "CARD_CHECKS_PASSTHROUGH", level: "info"},
		{name: "selected waiting card beside reporting card", cards: []proto.GPUInfo{owned, pending}, query: "?pcie_bdf=0000:0c:00.0&uuid=GPU-pending", code: "CARD_CHECKS_WAITING", level: "info", wantCards: 1},
		{name: "selected usable in mixed node", cards: []proto.GPUInfo{owned, passed}, query: "?pcie_bdf=0000:08:00.0&uuid=GPU-owned", wantCards: 1},
		{name: "offline even without capability", cards: []proto.GPUInfo{passed}, offline: true, unsupported: true, code: "CARD_CHECKS_OFFLINE", level: "warning"},
		{name: "stale report", cards: []proto.GPUInfo{owned}, stale: true, code: "CARD_CHECKS_STALE", level: "warning", wantCards: 1},
		{name: "old agent stale ownership stays unconfirmed", cards: []proto.GPUInfo{passed}, stale: true, unsupported: true, code: "CARD_CHECKS_UNSUPPORTED", level: "info"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, st, fake, online, caps := cardHealthFixture(t)
			*online = !tc.offline
			if tc.unsupported {
				*caps = nil
			}
			ok := true
			if err := st.UpdateNodeStatus("drain-a", proto.NodeStatus{GPUProbeOK: &ok, GPUs: tc.cards}); err != nil {
				t.Fatal(err)
			}
			node, err := st.NodeWithGPUs("drain-a")
			if err != nil {
				t.Fatal(err)
			}
			tracker := cardhealth.New(nil, nil)
			if tc.noReport {
				delete(fake.nodes, "drain-a")
			} else {
				at := time.Now().UTC()
				if tc.stale {
					at = at.Add(-time.Hour)
				}
				tracker.Observe(node.UUID, node.ID, at, tc.cards)
				snapshot, _ := tracker.Node(node.UUID)
				fake.nodes[node.UUID] = snapshot
			}
			status, body := callNode(t, a.nodeHealthChecks, http.MethodGet, "drain-a", "/health-checks"+tc.query, "")
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, body)
			}
			out := decode(t, body)
			if tc.code == "" {
				if out["notice"] != nil {
					t.Fatalf("ready checks must not have a notice: %s", body)
				}
			} else {
				want := `{"code":"` + tc.code + `","level":"` + tc.level + `"}`
				notice, err := json.Marshal(out["notice"])
				if err != nil || string(notice) != want {
					t.Fatalf("notice=%s want=%s; body=%s", notice, want, body)
				}
				var typed dto.CardHealthChecks
				if err := json.Unmarshal([]byte(body), &typed); err != nil {
					t.Fatal(err)
				}
				roundTrip, err := json.Marshal(typed)
				if err != nil || !strings.Contains(string(roundTrip), `"notice":`+want) {
					t.Fatalf("notice lost in DTO round-trip: %s (%v)", roundTrip, err)
				}
			}
			if got := len(out["cards"].([]any)); got != tc.wantCards {
				t.Fatalf("cards=%d want=%d; body=%s", got, tc.wantCards, body)
			}
		})
	}
}
