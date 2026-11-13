package kube

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

// IdentityFile is where an enrolled agent keeps its node token, under the
// state directory (a hostPath, so it outlives the agent pod).
const IdentityFile = "kube-node-token.json"

// Identity is what enrollment gives a Kubernetes node.
type Identity struct {
	NodeUUID          string `json:"node_uuid"`
	NodeToken         string `json:"node_token"`
	IdleReturnSeconds int    `json:"idle_return_seconds"`
	// Entry names the cluster entry that issued the identity, by its
	// enrollment token (EntryOf): entries of one cluster share the default
	// state directory, and an identity is only this agent's when its own
	// entry issued it.
	Entry string `json:"entry,omitempty"`
}

// EntryOf names an entry by its enrollment token without keeping the token.
func EntryOf(enrollToken string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(enrollToken)))
	return hex.EncodeToString(sum[:16])
}

// IdleReturn is the cluster's grace period for borrowed cards; negative
// keeps them.
func (id Identity) IdleReturn() time.Duration { return IdleReturnOf(id.IdleReturnSeconds) }

// IdleReturnOf is the grace period an entry's idle_return_seconds names;
// negative keeps borrowed cards.
func IdleReturnOf(seconds int) time.Duration {
	if seconds < 0 {
		return -1
	}
	return time.Duration(seconds) * time.Second
}

// ReadIdentity is the node identity this state directory saved, or a zero
// Identity when there is none yet (never enrolled, or an unreadable file).
func ReadIdentity(stateDir string) Identity {
	id, _ := readIdentity(filepath.Join(stateDir, IdentityFile))
	return id
}

// ReadOwnIdentity is the saved identity when the entry with enrollToken
// issued it, else a zero Identity: another entry's identity is not this
// agent's to run on.
func ReadOwnIdentity(stateDir, enrollToken string) Identity {
	id := ReadIdentity(stateDir)
	if id.Entry != EntryOf(enrollToken) {
		return Identity{}
	}
	return id
}

// kubeEnrollRefusedCode is the server's product-error code for a refused
// enrollment (internal/server/api.ErrorKubeEnrollRefused); the agent must
// not import server packages, so this wire contract is duplicated here
// rather than shared.
const kubeEnrollRefusedCode = "KUBE_ENROLL_REFUSED"

// EnrollRefusal is why the server refused this node's enrollment, named by
// {"code":"KUBE_ENROLL_REFUSED","params":{"reason":...}}'s reason: an
// enrollment token it does not know or that was revoked ("unknown_token"),
// a node name that belongs to another cluster ("other_cluster"), or a node
// name already held by another entry of this cluster ("node_taken"). Only
// "unknown_token" still lets a node that enrolled earlier keep running on
// the identity it saved — the other two describe this node's own identity
// clashing with another entry's, which an old token does not excuse away.
type EnrollRefusal struct {
	// Reason is the server's reason code, or "" when the 403 body carries
	// none (an older server, from before this field existed, whose only
	// refusal was always an unknown or revoked token; treated the same
	// way).
	Reason string
	// Message is the server's own English text, when it sent one.
	Message string
}

func (e *EnrollRefusal) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "the server refused this node's enrollment: " + e.Reason
}

// fallsBack is whether a refusal with this reason still lets a node that
// enrolled earlier run on the identity it saved.
func (e *EnrollRefusal) fallsBack() bool {
	return e.Reason == "" || e.Reason == "unknown_token"
}

// parseEnrollRefusal reads a 403 response body for the server's reason.
// Anything that does not parse as the expected product error (a proxy's
// own 403, or a server that predates ErrorKubeEnrollRefused entirely) is
// folded into a plain error, which Enroll treats exactly like a network
// failure: this response is not a refusal this build understands, so it
// must not be read as one naming this node's identity as another entry's.
func parseEnrollRefusal(raw []byte) error {
	var body struct {
		Error  string `json:"error"`
		Code   string `json:"code"`
		Params struct {
			Reason string `json:"reason"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Code != kubeEnrollRefusedCode {
		return errors.New("the server refused this cluster's enrollment token: " + strings.TrimSpace(string(raw)))
	}
	return &EnrollRefusal{Reason: body.Params.Reason, Message: body.Error}
}

// Enroll trades the cluster's enrollment token for this node's identity.
// The server answers the same node every time, so it asks on every start
// (picking up a changed cluster setting) and falls back to the identity it
// saved when it gets no answer, or a refusal that is not about this node's
// own identity (EnrollRefusal.fallsBack): a revoked enrollment token stops
// new nodes joining, not the ones that already did, whose own node token is
// what the server checks. A refusal naming this node's name as another
// entry's is never papered over by an old token, with or without a saved
// identity. Only a node that never enrolled needs the server to say yes.
//
// Only an identity this entry issued is fallen back to (Identity.Entry), and
// not at all when fallback is false: the caller passes false once the
// server has refused this node's identity, so a later network failure does
// not hide that refusal behind the saved identity.
func Enroll(ctx context.Context, api *kubeapi.Client, httpc *http.Client, serverURL, enrollToken, nodeName, stateDir string, fallback bool) (Identity, error) {
	path := filepath.Join(stateDir, IdentityFile)
	saved, savedErr := readIdentity(path)
	entry := EntryOf(enrollToken)
	id, err := enroll(ctx, api, httpc, serverURL, enrollToken, nodeName)
	if err != nil {
		var refusal *EnrollRefusal
		if errors.As(err, &refusal) && !refusal.fallsBack() {
			return Identity{}, err
		}
		if fallback && savedErr == nil && saved.Entry == entry && saved.NodeToken != "" {
			return saved, nil
		}
		return Identity{}, err
	}
	id.Entry = entry
	if id != saved {
		if err := writeIdentity(path, id); err != nil {
			return Identity{}, err
		}
	}
	return id, nil
}

func enroll(ctx context.Context, api *kubeapi.Client, httpc *http.Client, serverURL, enrollToken, nodeName string) (Identity, error) {
	var ns kubeapi.Namespace
	if err := api.Get(ctx, kubeapi.NamespacePath("kube-system"), &ns); err != nil {
		return Identity{}, fmt.Errorf("read the cluster's identity (kube-system namespace): %w", err)
	}
	body, _ := json.Marshal(map[string]string{"token": enrollToken, "cluster_uid": ns.Metadata.UID, "node_name": nodeName})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(serverURL, "/")+"/api/v1/kube/enroll", bytes.NewReader(body))
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if httpc == nil {
		httpc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("enroll with %s: %w", serverURL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusForbidden {
		return Identity{}, parseEnrollRefusal(raw)
	}
	if resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("enroll with %s: HTTP %d: %s", serverURL, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var id Identity
	if err := json.Unmarshal(raw, &id); err != nil || id.NodeToken == "" {
		return Identity{}, fmt.Errorf("enroll with %s: unexpected answer %q", serverURL, raw)
	}
	return id, nil
}

func readIdentity(path string) (Identity, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Identity{}, err
	}
	var id Identity
	if err := json.Unmarshal(raw, &id); err != nil || id.NodeToken == "" {
		return Identity{}, fmt.Errorf("%s is not a node identity", path)
	}
	return id, nil
}

func writeIdentity(path string, id Identity) error {
	raw, _ := json.Marshal(id)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
