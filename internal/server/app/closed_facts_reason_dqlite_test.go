//go:build linux && dqlite

package app

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

// A member that does not lead says so in its answer's text, for a person
// reading the response, and writes nothing to its log: every follower gives
// that answer. Any other failed read is logged, because the answer hides it.
func TestClosedFactsEnrollmentSaysWhyAReadFailed(t *testing.T) {
	for name, tc := range map[string]struct {
		err          error
		body, logged string
	}{
		"not the leader":  {fmt.Errorf("read: %w", cluster.ErrQuorumReadNotLeader), "cluster quorum facts unavailable: this member is not the database leader\n", ""},
		"another failure": {errors.New("actual leader SQL reader failure"), "cluster quorum facts unavailable\n", "actual leader SQL reader failure"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newClosedFactsTLSFixture(t)
			f.session.err = tc.err
			var log bytes.Buffer
			requests := newMaterialRequestTracker()
			handler, err := closedQuorumFactsHandler(f.bundle, f.session, requests, slog.New(slog.NewTextHandler(&log, nil)))
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, enrollment.ClusterQuorumFactsPath, strings.NewReader(closedFactsRouteBody))
			request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{f.requesterPair.Leaf}}
			recorder := httptest.NewRecorder()
			handler(recorder, request)
			if recorder.Code != http.StatusServiceUnavailable || recorder.Body.String() != tc.body {
				t.Fatalf("HTTP %d %q, want 503 %q", recorder.Code, recorder.Body.String(), tc.body)
			}
			if tc.logged == "" && log.Len() != 0 {
				t.Fatalf("a follower's answer was logged: %s", log.String())
			}
			if tc.logged != "" && (!strings.Contains(log.String(), "cluster quorum facts read failed") || !strings.Contains(log.String(), tc.logged)) {
				t.Fatalf("the failed read is not in the log: %q", log.String())
			}
		})
	}
}
