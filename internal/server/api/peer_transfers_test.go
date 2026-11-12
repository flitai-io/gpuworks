package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/prepobs"
)

func TestPeerTransferSnapshotRequiresAuthentication(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	if rec := rig.do("", http.MethodGet, "/api/v1/peer-transfers", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated snapshot status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPeerTransferSnapshotReportsUnavailableCapability(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/peer-transfers", "")
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusServiceUnavailable || len(body) != 2 || body["code"] != "PEER_TRANSFER_OBSERVATION_UNAVAILABLE" || body["error"] != "peer transfer observations are unavailable" {
		t.Fatalf("unavailable snapshot status=%d body=%s", rec.Code, rec.Body.String())
	}
}

type peerTransferSnapshotFixture struct {
	NodeStreams
	snapshot      prepobs.PeerTransferSnapshot
	allCalls      int
	filteredCalls []string
}

func (s *peerTransferSnapshotFixture) PeerTransferSnapshotDTO() dto.PeerTransferSnapshot {
	s.allCalls++
	return dto.PeerTransferSnapshotFrom(s.snapshot, "")
}
func (s *peerTransferSnapshotFixture) PeerTransferSnapshotForDTO(id string) dto.PeerTransferSnapshot {
	s.filteredCalls = append(s.filteredCalls, id)
	out := s.snapshot
	out.Active = nil
	for _, item := range s.snapshot.Active {
		if item.TransferID == id {
			out.Active = append(out.Active, item)
		}
	}
	return dto.PeerTransferSnapshotFrom(out, id)
}

func peerTransferRig(t *testing.T) (*tokenRig, *peerTransferSnapshotFixture) {
	t.Helper()
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	at := time.Date(2026, 11, 11, 4, 0, 0, 0, time.UTC)
	source := &peerTransferSnapshotFixture{snapshot: prepobs.PeerTransferSnapshot{
		ObservedAt: at, StartedAt: at.Add(-time.Hour), MaxActive: 2, ActiveCount: 2, WaitingCount: 1,
		Policy: prepobs.PeerRelayPolicy{RateLimitBytesPerSecond: 64 << 20, BurstBytes: 65536, QuantumBytes: 16384, MaxActive: 2, MaxPerNode: 1, NoProgressTimeout: 2 * time.Minute, ConfirmationTimeout: 5 * time.Minute},
		Totals: prepobs.PeerTransferTotals{Started: 3, PeerConfirmed: 1, Busy: 2, SourceToTargetBytes: 1048576, TargetToSourceBytes: 256, RateWaitNS: 2500000},
		Active: []prepobs.PeerTransferObservation{
			{PeerTransferSpec: prepobs.PeerTransferSpec{TransferID: "a/b", Kind: "model", Operation: "copy", SourceNodeUUID: "source", TargetNodeUUID: "target", ResourceKey: "model-file-7"}, Transport: "relay", Phase: "transferring", FallbackReason: "direct_dial_failed", StartedAt: at.Add(-time.Minute), UpdatedAt: at, SourceToTargetBytes: 1048576, TargetToSourceBytes: 256, RateWaitNS: 1500000, EffectiveRateLimitBytesPerSecond: 16 << 20, ServerRateBytesPerSecond: 8 << 20, RateWindowSeconds: 5},
			{PeerTransferSpec: prepobs.PeerTransferSpec{TransferID: "other", Kind: "image", Operation: "probe", SourceNodeUUID: "other-source", TargetNodeUUID: "other-target"}, Phase: "connecting_direct"},
		},
	}}
	signer, err := auth.NewTokenSigner("platform-token-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rig.h = Router(Deps{AuthSigner: signer, Store: rig.st, NodeStreams: source})
	return rig, source
}

func TestPeerTransferSnapshotLiteralWireAndTargetedRead(t *testing.T) {
	rig, source := peerTransferRig(t)
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/peer-transfers?transfer_id=a%2Fb", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%s", rec.Code, rec.Body.String())
	}
	const wire = `{
      "observed_at":"2026-11-11T04:00:00Z","server_started_at":"2026-11-11T03:00:00Z","scope":"server_process",
      "policy":{"rate_limit_bps":67108864,"burst_bytes":65536,"max_active":2,"max_per_node":1,"no_progress_timeout_seconds":120,"confirmation_timeout_seconds":300},
      "active_relay_count":2,"rate_waiting_count":1,
      "totals":{"started":3,"peer_confirmed":1,"failed":0,"canceled":0,"busy":2,"unconfirmed":0,"probe_started":0,"probe_ok":0,"probe_failed":0,"source_to_target_bytes":1048576,"target_to_source_bytes":256,"rate_wait_ms":2.5},
      "active":[{"transfer_id":"a/b","kind":"model","operation":"copy","source_node_uuid":"source","target_node_uuid":"target","resource_key":"model-file-7","transport":"relay","phase":"transferring","fallback_reason":"direct_dial_failed","started_at":"2026-11-11T03:59:00Z","updated_at":"2026-11-11T04:00:00Z","source_to_target_bytes":1048576,"target_to_source_bytes":256,"effective_rate_limit_bps":16777216,"server_rate_bps":8388608,"rate_window_seconds":5,"rate_wait_ms":1.5}]
    }`
	var want, got map[string]any
	if err := json.Unmarshal([]byte(wire), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot wire mismatch\ngot %s\nwant %s", rec.Body.String(), wire)
	}
	var roundtrip dto.PeerTransferSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &roundtrip); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(roundtrip)
	if err != nil {
		t.Fatal(err)
	}
	var again map[string]any
	if err := json.Unmarshal(encoded, &again); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, want) {
		t.Fatalf("snapshot roundtrip=%s", encoded)
	}
	if source.allCalls != 0 || !reflect.DeepEqual(source.filteredCalls, []string{"a/b"}) {
		t.Fatalf("snapshot scanned all operations: all=%d filters=%v", source.allCalls, source.filteredCalls)
	}
}

func TestPeerTransferSnapshotUnknownAndFullViews(t *testing.T) {
	rig, source := peerTransferRig(t)
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/peer-transfers?transfer_id=expired", "")
	var body dto.PeerTransferSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || body.Active == nil || len(body.Active) != 0 || body.ActiveRelayCount != 2 {
		t.Fatalf("unknown observation must stay empty: %d %s", rec.Code, rec.Body.String())
	}
	rec = rig.do(rig.session, http.MethodGet, "/api/v1/peer-transfers", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || len(body.Active) != 2 || source.allCalls != 1 {
		t.Fatalf("full view: %d %s calls=%d", rec.Code, rec.Body.String(), source.allCalls)
	}
}

func TestPeerTransferSnapshotRejectsInvalidFiltersAndMethods(t *testing.T) {
	rig, source := peerTransferRig(t)
	for _, query := range []string{"?transfer_id=", "?transfer_id=a&transfer_id=b", "?other=x", "?transfer_id=" + strings.Repeat("x", 129)} {
		rec := rig.do(rig.session, http.MethodGet, "/api/v1/peer-transfers"+query, "")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"PEER_TRANSFER_QUERY_INVALID"`) {
			t.Errorf("query %s status=%d body=%s", query, rec.Code, rec.Body.String())
		}
	}
	if source.allCalls != 0 || len(source.filteredCalls) != 0 {
		t.Fatal("invalid filters accessed observations")
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/peer-transfers", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("snapshot write method status=%d body=%s", rec.Code, rec.Body.String())
	}
}
