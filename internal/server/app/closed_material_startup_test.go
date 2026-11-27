//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv/testport"
)

func TestClosedMaterialStartupSignedTLSUnavailable(t *testing.T) {
	f, s := newClosedDiscoveryRouteFixture(t)
	s.proof.LeaderID = 11 // A restarting follower must also deny without a fatal missing route.
	server := startClosedDiscoveryFixture(t, f, s)
	response, body, err := closedFactsRouteRequest(t, f.client(t, &f.requesterPair), server, http.MethodPost, materialBatchPath, `{"files":[{"digest":"`+strings.Repeat("a", 64)+`","bytes":1}]}`)
	if err != nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("startup batch response=%v body=%q err=%v; want unavailable denial", response, body, err)
	}
	if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Gpuworks-Material-Member") != "" || response.Header.Get("Content-Type") == materialBatchType {
		t.Fatalf("startup denial advertised material metadata: %v", response.Header)
	}
	if server.materialReceiver != nil || s.calls != 0 || s.discoveryCalls != 1 {
		t.Fatalf("startup assembled receiver or requested leader proof: receiver=%v facts=%d discovery=%d", server.materialReceiver, s.calls, s.discoveryCalls)
	}
}

func TestClosedMaterialStartupRoutesAndIdentity(t *testing.T) {
	for _, mode := range []string{"active", "unadmitted", "revoked", "requester-address", "provider-address", "identity", "busy", "missing-certificate", "wrong-ca"} {
		t.Run(mode, func(t *testing.T) {
			f, s := newClosedDiscoveryRouteFixture(t)
			pair := &f.requesterPair
			want := http.StatusServiceUnavailable
			switch mode {
			case "unadmitted":
				pair, want = &f.unknownPair, http.StatusForbidden
			case "revoked":
				now := time.Now()
				s.admissions[1].RevokedAt, want = &now, http.StatusForbidden
			case "requester-address":
				s.admissions[1].Address, want = "127.0.0.2:9911", http.StatusForbidden
			case "provider-address":
				s.members[0].Address, want = "127.0.0.2:9910", http.StatusForbidden
			case "identity":
				s.expected.ClusterID, want = strings.Repeat("b", 32), http.StatusForbidden
			case "busy":
				s.err = errors.New("native session is classifying or closing")
			case "missing-certificate":
				pair = nil
			case "wrong-ca":
				pair = &newClosedFactsTLSFixture(t).requesterPair
			}
			before := append([]store.ClusterAdmission(nil), s.admissions...)
			server := startClosedDiscoveryFixture(t, f, s)
			response, body, err := closedFactsRouteRequest(t, f.client(t, pair), server, http.MethodPost, materialBatchPath, `{}`)
			if mode == "missing-certificate" || mode == "wrong-ca" {
				if err == nil {
					t.Fatalf("invalid TLS caller got HTTP response %v %s", response, body)
				}
				return
			}
			if err != nil || response.StatusCode != want {
				t.Fatalf("response=%v body=%s err=%v want=%d", response, body, err, want)
			}
			if !reflect.DeepEqual(before, s.admissions) || server.materialReceiver != nil || s.calls != 0 {
				t.Fatal("denial changed admissions, assembled material receiver or requested leader proof")
			}
		})
	}
	f, s := newClosedDiscoveryRouteFixture(t)
	server := startClosedDiscoveryFixture(t, f, s)
	client := f.client(t, &f.requesterPair)
	digest := strings.Repeat("a", 64)
	for _, method := range []string{http.MethodHead, http.MethodGet, http.MethodPost} {
		r, body, err := closedFactsRouteRequest(t, client, server, method, materialReplicaPath+"/"+digest, "")
		if err != nil || r.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("replica %s: response=%v body=%s err=%v", method, r, body, err)
		}
	}
	for _, action := range []string{"verify", "materialize", "installed"} {
		r, body, err := closedFactsRouteRequest(t, client, server, http.MethodPost, materialManifestPath+"/"+digest+"/"+action, "")
		if err != nil || r.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("manifest %s: response=%v body=%s err=%v", action, r, body, err)
		}
	}
	for _, probe := range []struct {
		method, path string
		status       int
	}{
		// GET batch matches the replica digest shape, then rejects invalid digest.
		{http.MethodGet, materialBatchPath, http.StatusNotFound},
		{http.MethodDelete, materialReplicaPath + "/" + digest, http.StatusMethodNotAllowed},
		{http.MethodGet, materialReplicaPath + "/invalid", http.StatusNotFound},
		{http.MethodPost, materialManifestPath + "/" + digest + "/unknown", http.StatusNotFound},
		{http.MethodPost, "/_cluster/unknown", http.StatusNotFound},
		{http.MethodPost, "/_cluster/bootstrap-secrets", http.StatusNotFound},
		{http.MethodGet, "/api/v1/settings", http.StatusNotFound},
	} {
		r, body, err := closedFactsRouteRequest(t, client, server, probe.method, probe.path, "")
		if err != nil || r.StatusCode != probe.status {
			t.Fatalf("%s %s: response=%v body=%s err=%v", probe.method, probe.path, r, body, err)
		}
	}
}

func TestClosedMaterialStartupDrainsCanceledLookup(t *testing.T) {
	f, s := newClosedDiscoveryRouteFixture(t)
	entered, canceled := make(chan struct{}), make(chan struct{})
	s.hold = func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}
	server := startClosedDiscoveryFixture(t, f, s)
	client := f.client(t, &f.requesterPair)
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequest(http.MethodPost, "https://"+server.Addr()+materialBatchPath, strings.NewReader(`{}`))
		r, err := client.Do(req)
		if err == nil {
			_ = r.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("startup request never entered lookup")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := server.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	default:
		t.Fatal("Close did not cancel lookup before drain")
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("HTTP request did not finish")
	}
	if err := server.materialRequests.wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestClosedMaterialStartupTransitionsToRealReceiver(t *testing.T) {
	f, s := newClosedDiscoveryRouteFixture(t)
	f.bundle.info.ListenAddress = testport.Address(t)
	server := startClosedDiscoveryFixture(t, f, s)
	client := f.client(t, &f.requesterPair)
	content := []byte("canonical bytes after actual receiver construction")
	digest := materialTestDigest(content)
	body := `{"files":[{"digest":"` + digest + `","bytes":` + strconv.Itoa(len(content)) + `}]}`
	// The exact requested bytes are checked by the real batch framing verifier below.
	request := func(target *enrollmentServer, body string) (*http.Response, []byte, error) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+target.Addr()+materialBatchPath, strings.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		return response, data, err
	}
	r, _, err := request(server, body)
	if err != nil || r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("startup response=%v err=%v", r, err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "receiver.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.InitializeClusterIdentity(t.Context(), s.expected); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InitializeClusterSeedAdmission(t.Context(), s.expected.ClusterID, f.serverKey, "127.0.0.1:9910"); err != nil {
		t.Fatal(err)
	}
	admin := store.User{Username: "startup-admin", Role: "admin"}
	if err := st.DB().Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	_, token, err := st.IssueClusterInvitation(t.Context(), s.expected.ClusterID, admin.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimClusterInvitation(t.Context(), s.expected.ClusterID, token, f.requesterKey, "127.0.0.1:9911"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	receiver, err := newMaterialReceiver(root, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, digest[:2]), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, digest[:2], digest), content, 0600); err != nil {
		t.Fatal(err)
	}
	before := server.Addr()
	// Use the production replacement path, retaining the published origin and identity.
	bundle := *f.bundle
	bundle.info.ListenAddress = before
	requests := newMaterialRequestTracker()
	prepared, err := newEnrollmentServer(before, enrollmentMaterialHandlerWithTrackedReceiver(&bundle, st, http.NotFoundHandler(), nil, receiver, requests), bundle.tls, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	prepared.materialRequests = requests
	prepared.materialReceiver = receiver
	a := &App{memberState: memberState{enrollment: server}}
	if err := a.replaceClosedEnrollment(t.Context(), prepared); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := prepared.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	r, data, err := request(prepared, body)
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("ready response=%v body=%q err=%v", r, data, err)
	}
	if prepared.Addr() != before || r.Header.Get("Content-Type") != materialBatchType || r.Header.Get("Cache-Control") != "no-store" || r.Header.Get("X-Gpuworks-Material-Member") != "7" {
		t.Fatalf("ready origin/headers changed: %s %v", prepared.Addr(), r.Header)
	}
	if err := verifyMaterialBatchBody(t.Context(), strings.NewReader(string(data)), []materialBatchItem{{Digest: digest, Bytes: int64(len(content))}}); err != nil {
		t.Fatal(err)
	}
	missing := strings.Repeat("a", 64)
	r, _, err = request(prepared, `{"files":[{"digest":"`+missing+`","bytes":1}]}`)
	if err != nil || r.StatusCode != http.StatusNotFound {
		t.Fatalf("real missing object lost404: response=%v err=%v", r, err)
	}
}
