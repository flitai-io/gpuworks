package kube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi/kubefake"
)

func TestEnrollSavesTheIdentityAndFallsBackToIt(t *testing.T) {
	k := kubefake.New(t, testNS, testNode)
	var got map[string]string
	answer := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/kube/enroll" || r.Method != http.MethodPost {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(answer)
		if answer == http.StatusOK {
			_, _ = w.Write([]byte(`{"node_uuid":"n-1","node_token":"tok-1","idle_return_seconds":-1}`))
		} else {
			_, _ = w.Write([]byte(`{"code":"KUBE_ENROLL_REFUSED"}`))
		}
	}))
	defer srv.Close()
	state := t.TempDir()
	id, err := Enroll(context.Background(), k.Client(), srv.Client(), srv.URL+"/", "enroll-tok", testNode, state)
	if err != nil || id.NodeToken != "tok-1" || id.IdleReturn() != -1 {
		t.Fatalf("Enroll = %+v, %v", id, err)
	}
	if got["token"] != "enroll-tok" || got["cluster_uid"] != "kube-system-uid" || got["node_name"] != testNode {
		t.Fatalf("enroll request = %v", got)
	}
	info, err := os.Stat(filepath.Join(state, IdentityFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("identity file: %v %v", info, err)
	}
	// The server is down: the saved identity serves.
	srv.Close()
	if id, err := Enroll(context.Background(), k.Client(), http.DefaultClient, srv.URL, "enroll-tok", testNode, state); err != nil || id.NodeToken != "tok-1" {
		t.Fatalf("Enroll with the server down = %+v, %v", id, err)
	}
	// Never enrolled and the server is down: an error.
	if _, err := Enroll(context.Background(), k.Client(), http.DefaultClient, srv.URL, "enroll-tok", testNode, t.TempDir()); err == nil {
		t.Fatal("enrolled without a server")
	}
}

func TestARevokedTokenKeepsEnrolledNodes(t *testing.T) {
	k := kubefake.New(t, testNS, testNode)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"KUBE_ENROLL_REFUSED","params":{"reason":"unknown_token"}}`))
	}))
	defer srv.Close()
	state := t.TempDir()
	if err := writeIdentity(filepath.Join(state, IdentityFile), Identity{NodeUUID: "n", NodeToken: "old"}); err != nil {
		t.Fatal(err)
	}
	if id, err := Enroll(context.Background(), k.Client(), srv.Client(), srv.URL, "revoked", testNode, state); err != nil || id.NodeToken != "old" {
		t.Fatalf("Enroll of an enrolled node after revocation = %+v, %v", id, err)
	}
	if _, err := Enroll(context.Background(), k.Client(), srv.Client(), srv.URL, "revoked", testNode, t.TempDir()); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("Enroll of a new node after revocation = %v", err)
	}
	if (Identity{IdleReturnSeconds: 600}).IdleReturn() != 10*time.Minute {
		t.Fatal("IdleReturn")
	}
}

// TestEnrollRefusalsNamingAnotherEntryAreNeverPaperedOverBySavedIdentity:
// other_cluster and node_taken are facts about this node's own identity,
// not about the server being unreachable or the token having gone stale,
// so a saved identity must never mask them, with or without one on disk.
func TestEnrollRefusalsNamingAnotherEntryAreNeverPaperedOverBySavedIdentity(t *testing.T) {
	for _, reason := range []string{"other_cluster", "node_taken"} {
		t.Run(reason, func(t *testing.T) {
			k := kubefake.New(t, testNS, testNode)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"refused","code":"KUBE_ENROLL_REFUSED","params":{"reason":"` + reason + `"}}`))
			}))
			defer srv.Close()

			// With a saved identity from an earlier, different node: the
			// refusal still wins.
			state := t.TempDir()
			if err := writeIdentity(filepath.Join(state, IdentityFile), Identity{NodeUUID: "n", NodeToken: "old"}); err != nil {
				t.Fatal(err)
			}
			id, err := Enroll(context.Background(), k.Client(), srv.Client(), srv.URL, "tok", testNode, state)
			var refusal *EnrollRefusal
			if err == nil || id.NodeToken != "" || !errors.As(err, &refusal) || refusal.Reason != reason {
				t.Fatalf("Enroll refused as %s with a saved identity = %+v, %v, want an EnrollRefusal naming %s", reason, id, err, reason)
			}

			// Without any saved identity: the same refusal, the same way.
			if _, err := Enroll(context.Background(), k.Client(), srv.Client(), srv.URL, "tok", testNode, t.TempDir()); !errors.As(err, &refusal) || refusal.Reason != reason {
				t.Fatalf("Enroll refused as %s without a saved identity = %v", reason, err)
			}
		})
	}
}

// TestEnrollTreatsAnUnrecognizedRefusalBodyAsAFallbackCase: a 403 from
// something other than the server's own ErrorKubeEnrollRefused handler (a
// proxy, an old server) is not a refusal this build understands, so it must
// not be read as a hard refusal of this node's own identity.
func TestEnrollTreatsAnUnrecognizedRefusalBodyAsAFallbackCase(t *testing.T) {
	k := kubefake.New(t, testNS, testNode)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("403 Forbidden"))
	}))
	defer srv.Close()
	state := t.TempDir()
	if err := writeIdentity(filepath.Join(state, IdentityFile), Identity{NodeUUID: "n", NodeToken: "old"}); err != nil {
		t.Fatal(err)
	}
	id, err := Enroll(context.Background(), k.Client(), srv.Client(), srv.URL, "tok", testNode, state)
	if err != nil || id.NodeToken != "old" {
		t.Fatalf("Enroll against an unrecognized 403 with a saved identity = %+v, %v", id, err)
	}
}
