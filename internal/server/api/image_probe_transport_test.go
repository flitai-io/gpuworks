package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestProbeImageTransport(t *testing.T) {
	for _, transport := range []string{"direct", "relay"} {
		t.Run(transport, func(t *testing.T) {
			h := apiHandlers{d: Deps{PeerSync: PeerSyncHooks{ProbeImage: func(context.Context, uint, string, string) error {
				t.Fatal("legacy hook must not replace transport probe")
				return nil
			}, ProbeImageResult: func(_ context.Context, id uint, target, source string) (proto.ImageSyncCopyResult, error) {
				if id != 19 || target != "dst" || source != "src" {
					t.Fatalf("probe args=%d/%s/%s", id, target, source)
				}
				return proto.ImageSyncCopyResult{Transport: transport}, nil
			}}}}
			resp := postJSON(t, h.probeImageToNode, "/", `{"source_node":"src"}`, map[string]string{"id": "19", "uuid": "dst"})
			if resp.Code != http.StatusOK || strings.TrimSpace(resp.Body.String()) != `{"state":"ready","transport":"`+transport+`"}` {
				t.Fatalf("response=%d %s", resp.Code, resp.Body.String())
			}
		})
	}
}

func TestProbeImageTransportFailureAndLegacy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		hooks  PeerSyncHooks
		body   string
		status int
		wire   string
	}{
		{"legacy", PeerSyncHooks{ProbeImage: func(context.Context, uint, string, string) error { return nil }}, `{"source_node":"src"}`, 200, `{"state":"ready"}`},
		{"failure", PeerSyncHooks{ProbeImageResult: func(context.Context, uint, string, string) (proto.ImageSyncCopyResult, error) {
			return proto.ImageSyncCopyResult{}, errors.New("relay unavailable")
		}}, `{"source_node":"src"}`, 409, "relay unavailable"},
		{"missing source", PeerSyncHooks{ProbeImageResult: func(context.Context, uint, string, string) (proto.ImageSyncCopyResult, error) {
			t.Fatal("must not probe")
			return proto.ImageSyncCopyResult{}, nil
		}}, `{}`, 400, "source_node"},
		{"unavailable", PeerSyncHooks{}, `{"source_node":"src"}`, 503, "not configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := apiHandlers{d: Deps{PeerSync: tc.hooks}}
			resp := postJSON(t, h.probeImageToNode, "/", tc.body, map[string]string{"id": "19", "uuid": "dst"})
			if resp.Code != tc.status || !strings.Contains(resp.Body.String(), tc.wire) {
				t.Fatalf("response=%d %s", resp.Code, resp.Body.String())
			}
		})
	}
}
