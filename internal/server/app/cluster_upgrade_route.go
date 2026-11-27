//go:build linux && dqlite

package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// clusterUpgradeHandler serves the steps of a cluster upgrade on the member's
// own enrollment listener, to a caller that proves an admitted member key.
// It works on the session that owns the database, so it answers while the
// member's business is closed and while its executable is ahead of the schema.
//
// agents reports the agent connections this member holds (nil when the
// member has no business to hold any): the upgrade command replaces the
// members that hold none first.
func clusterUpgradeHandler(bundle *enrollmentBundle, session joinedRaftSession, requests *materialRequestTracker, agents func() (int, bool)) (http.HandlerFunc, error) {
	if requests == nil || bundle == nil || bundle.trust == nil {
		return nil, errors.New("cluster upgrade requires an authenticated enrollment identity")
	}
	stepper, ok := session.(clusterUpgradeStepper)
	if !ok {
		return nil, errors.New("native session does not support the cluster upgrade")
	}
	expected := store.ClusterIdentity{ClusterID: bundle.info.ClusterID, CAFingerprint: bundle.info.CAFingerprint, Protocol: bundle.info.Protocol}
	return requests.wrap(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "member authentication rejected", http.StatusForbidden)
			return
		}
		requester, err := bundle.trust.VerifyMember(r.TLS.PeerCertificates[0], time.Now().UTC())
		if err != nil {
			http.Error(w, "member authentication rejected", http.StatusForbidden)
			return
		}
		var request enrollment.ClusterUpgradeRequest
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
		if err != nil || json.Unmarshal(data, &request) != nil || !enrollment.ValidClusterUpgradeAction(request.Action) {
			http.Error(w, "invalid cluster upgrade request", http.StatusBadRequest)
			return
		}
		status, leader, migrating, err := stepper.ClusterUpgrade(r.Context(), expected, requester, request.Action)
		if errors.Is(err, store.ErrClusterAdmissionRejected) || errors.Is(err, store.ErrClusterIdentityConflict) {
			http.Error(w, "member authentication rejected", http.StatusForbidden)
			return
		}
		response := enrollment.ClusterUpgradeResponse{
			Action: request.Action, Leader: leader, Migrating: migrating,
			Phase: status.Phase.Phase, PhaseRevision: status.Phase.Revision,
			Applied: status.Applied, Known: status.Known, Base: status.Base, Ready: status.Ready(),
			AppliedExtensions: status.AppliedExtensions, KnownExtensions: status.KnownExtensions,
			AppliedByExtension: status.AppliedExtensionMarkers(),
			StorageTier:        string(cluster.ProcessStorageTier()),
		}
		if recorded, err := RecordedStorageTier(bundle.dataDir); err == nil {
			response.RecordedStorageTier = string(recorded)
		}
		if agents != nil {
			if held, known := agents(); known {
				response.AgentConnections = &held
			}
		}
		for _, member := range status.Members {
			response.Members = append(response.Members, enrollment.ClusterUpgradeMember{
				Address: member.Address, Reported: member.Reported, Markers: member.Markers, Last: member.Last,
				Extensions: member.Extensions, Fresh: member.Fresh})
		}
		switch {
		case errors.Is(err, store.ErrClusterUpgradeRefused):
			response.Refused = err.Error()
		case err != nil:
			response.Error = err.Error()
		}
		body, err := json.Marshal(response)
		if err != nil {
			http.Error(w, "cluster upgrade unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(append(body, '\n'))
	}), nil
}

// ClusterUpgradeStep asks this installation's own member for one step of a
// cluster upgrade and returns the cluster's upgrade state after it. It proves
// the member's key to the member's enrollment listener, so it needs the
// installation's data directory and no session. A step that was refused or
// failed is in the response, not the error.
func ClusterUpgradeStep(ctx context.Context, cfg Config, action string, timeout time.Duration) (enrollment.ClusterUpgradeResponse, error) {
	none := enrollment.ClusterUpgradeResponse{}
	if timeout <= 0 {
		return none, errors.New("a cluster upgrade step requires a positive timeout")
	}
	report, err := readClusterStatus(cfg)
	if err != nil {
		return none, err
	}
	if report.clusterID == "not recorded" || report.pin == "" || len(report.cert) == 0 || len(report.key) == 0 {
		return none, errors.New("this installation holds no cluster member identity")
	}
	origin := report.publicURL
	if origin == "" && report.enrollmentListen != "" {
		origin = "https://" + report.enrollmentListen
	}
	if !enrollment.CanonicalEnrollmentOrigin(origin) {
		return none, errors.New("this member records no enrollment listener")
	}
	client, err := enrollment.NewClient(origin+enrollmentCertificatePath, report.clusterID, report.pin, report.protocol)
	if err != nil {
		return none, err
	}
	defer client.Close()
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return client.ClusterUpgrade(callCtx, report.cert, report.key, action)
}
