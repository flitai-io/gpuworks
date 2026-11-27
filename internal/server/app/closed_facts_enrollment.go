//go:build linux && dqlite

package app

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// This listener exposes a leader's fixed quorum observation. It neither grants
// material authorization nor proves that every voter process is closed.
// local is this member's own configuration: where its database and log are.
func newClosedFactsEnrollmentServer(bundle *enrollmentBundle, session joinedRaftSession, local Config, log *slog.Logger) (*enrollmentServer, error) {
	requests := newMaterialRequestTracker()
	router := chi.NewRouter()
	if err := registerMemberObservation(router, bundle, session, requests, nil, local, log); err != nil {
		return nil, err
	}
	registerClosedMaterialUnavailable(router, bundle, session, requests)
	config := bundle.tls.Clone()
	config.ClientAuth = tls.RequireAndVerifyClientCert
	server, err := newEnrollmentServer(bundle.info.ListenAddress, router, config, log)
	if err != nil {
		return nil, err
	}
	server.materialRequests = requests
	return server, nil
}

// Material listeners can register the same fixed route with their existing
// request owner. Every authentication/read/write remains inside that owner.
func closedQuorumFactsHandler(bundle *enrollmentBundle, session joinedRaftSession, requests *materialRequestTracker, log *slog.Logger) (http.HandlerFunc, error) {
	if requests == nil || bundle == nil || bundle.trust == nil || bundle.tls == nil || len(bundle.tls.Certificates) != 1 {
		return nil, errors.New("quorum facts require an authenticated enrollment identity")
	}
	reader, ok := session.(quorumFactsReader)
	if !ok {
		return nil, errors.New("native session does not support cluster quorum facts")
	}
	pair := bundle.tls.Certificates[0]
	if len(pair.Certificate) == 0 {
		return nil, errors.New("quorum facts require the actual listener certificate")
	}
	providerCert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	expected := store.ClusterIdentity{ClusterID: bundle.info.ClusterID, CAFingerprint: bundle.info.CAFingerprint, Protocol: bundle.info.Protocol}
	return requests.wrap(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "member authentication rejected", http.StatusForbidden)
			return
		}
		now := time.Now().UTC()
		requester, err := bundle.trust.VerifyMember(r.TLS.PeerCertificates[0], now)
		if err != nil {
			http.Error(w, "member authentication rejected", http.StatusForbidden)
			return
		}
		provider, err := bundle.trust.VerifyMember(providerCert, now)
		if err != nil {
			http.Error(w, "quorum facts provider unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.Body == nil {
			http.Error(w, "invalid quorum facts request", http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
		if err != nil {
			http.Error(w, "invalid quorum facts request", http.StatusBadRequest)
			return
		}
		request, err := enrollment.DecodeClusterQuorumFactsRequest(data)
		if err != nil {
			http.Error(w, "invalid quorum facts request", http.StatusBadRequest)
			return
		}
		legacy, phase, admissions, proof, err := reader.ReadLocalQuorumFactsForCluster(r.Context(), expected, requester)
		if r.Context().Err() != nil {
			panic(http.ErrAbortHandler)
		}
		if err != nil {
			quorumFactsUnavailable(w, err, log)
			return
		}
		facts, requesterAdmission, providerAddress, err := projectClosedQuorumFacts(expected, request.Nonce, legacy, phase, admissions, proof, requester, provider)
		if err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, store.ErrClusterAdmissionRejected) {
				status = http.StatusForbidden
			}
			http.Error(w, "cluster quorum facts unavailable", status)
			return
		}
		if !factsCertificateCovers(r.TLS.PeerCertificates[0], requesterAdmission.Address) {
			http.Error(w, "member authentication rejected", http.StatusForbidden)
			return
		}
		if !factsCertificateCovers(providerCert, providerAddress) {
			http.Error(w, "quorum facts provider unavailable", http.StatusServiceUnavailable)
			return
		}
		data, err = json.Marshal(facts)
		if err == nil {
			_, err = enrollment.DecodeClusterQuorumFacts(data)
		}
		if err != nil {
			http.Error(w, "cluster quorum facts unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.Context().Err() != nil {
			panic(http.ErrAbortHandler)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(append(data, '\n'))
	}), nil
}

func factsCertificateCovers(certificate *x509.Certificate, address string) bool {
	canonical, err := identity.NormalizeMemberAddress(address)
	if err != nil || canonical != address {
		return false
	}
	host, _, err := net.SplitHostPort(address)
	return err == nil && certificate.VerifyHostname(host) == nil
}

func projectClosedQuorumFacts(expected store.ClusterIdentity, nonce string, legacy bool, phase store.MaintenancePhaseState, admissions []store.ClusterAdmission, proof cluster.QuorumProof, requester, provider string) (enrollment.ClusterQuorumFacts, store.ClusterAdmission, string, error) {
	fail := func() (enrollment.ClusterQuorumFacts, store.ClusterAdmission, string, error) {
		return enrollment.ClusterQuorumFacts{}, store.ClusterAdmission{}, "", store.ErrClusterIdentityConflict
	}
	if phase.Revision < 0 || len(admissions) == 0 || len(admissions) > 512 || len(proof.Members) == 0 || len(proof.Members) > 512 {
		return fail()
	}
	active := make(map[string]store.ClusterAdmission, len(admissions))
	for _, admission := range admissions {
		if admission.RevokedAt != nil {
			continue
		}
		if _, exists := active[admission.KeyFingerprint]; exists {
			return fail()
		}
		active[admission.KeyFingerprint] = admission
	}
	requesterAdmission, admitted := active[requester]
	if !admitted {
		return enrollment.ClusterQuorumFacts{}, store.ClusterAdmission{}, "", store.ErrClusterAdmissionRejected
	}
	facts := enrollment.ClusterQuorumFacts{
		ClusterID: expected.ClusterID, CAFingerprint: expected.CAFingerprint, Protocol: expected.Protocol,
		Nonce: nonce, Legacy: legacy, Phase: phase.Phase, PhaseRevision: uint64(phase.Revision),
		Term: proof.Term, Index: proof.Index, ConfigurationIndex: proof.ConfigurationIndex, LeaderID: proof.LeaderID,
		Members: make([]enrollment.ClusterMember, 0, len(proof.Members)),
	}
	// Which members of the database are members of the cluster is the
	// store's definition, the one every membership grant uses.
	bound, err := store.BindClusterMembers(proof.Members, admissions)
	if err != nil {
		return fail()
	}
	providerAddress := ""
	for _, member := range bound {
		facts.Members = append(facts.Members, enrollment.ClusterMember{ID: member.ID, Address: member.Address, Role: member.Role, KeyFingerprint: member.KeyFingerprint, EnrollmentOrigin: member.EnrollmentOrigin})
		if member.ID == proof.LeaderID && member.Role == "voter" && member.KeyFingerprint == provider {
			providerAddress = member.Address
		}
	}
	if providerAddress == "" {
		return fail()
	}
	sort.Slice(facts.Members, func(i, j int) bool { return facts.Members[i].ID < facts.Members[j].ID })
	return facts, requesterAdmission, providerAddress, nil
}

// quorumFactsUnavailable answers a facts read that failed. A member that
// does not lead says so in the answer's text, for a person reading the
// response: the member client reports a refused request by its status only
// and never repeats a member's answer in an error. That answer is not
// logged, since every follower gives it to every asker. Any other failure
// is logged here, because the answer does not carry it.
func quorumFactsUnavailable(w http.ResponseWriter, err error, log *slog.Logger) {
	switch {
	case errors.Is(err, store.ErrClusterAdmissionRejected):
		http.Error(w, "cluster quorum facts unavailable", http.StatusForbidden)
	case errors.Is(err, cluster.ErrQuorumReadNotLeader):
		http.Error(w, "cluster quorum facts unavailable: this member is not the database leader", http.StatusServiceUnavailable)
	default:
		if log != nil {
			log.Warn("cluster quorum facts read failed", "error", err)
		}
		http.Error(w, "cluster quorum facts unavailable", http.StatusServiceUnavailable)
	}
}
