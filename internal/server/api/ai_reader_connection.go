package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// NodeConnectionDiagnostics permits one read-only POST, with its target and
// credentials still chosen by the existing administrator-only handler.
func (rd *aiReader) NodeConnectionDiagnostics(ctx context.Context, nodeUUID string) (int, []byte, error) {
	if len(nodeUUID) != 32 {
		return 0, nil, errors.New("node connection diagnostics requires a node UUID")
	}
	for _, ch := range nodeUUID {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return 0, nil, errors.New("node connection diagnostics requires a node UUID")
		}
	}
	if rd.mux == nil {
		return 0, nil, errors.New("api router not wired")
	}
	const pattern = "/api/v1/nodes/{uuid}/connection-diagnostics"
	target := "/api/v1/nodes/" + nodeUUID + "/connection-diagnostics"
	if rd.mux.Find(chi.NewRouteContext(), http.MethodPost, target) != pattern {
		return 0, nil, errors.New("node connection diagnostics is unavailable")
	}
	return rd.readRequest(ctx, http.MethodPost, target, pattern)
}
