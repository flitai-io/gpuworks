package proto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// ServerDialChallenge is sent by a listening agent before yamux starts. The
// server must prove it knows the shared install token for this process.
type ServerDialChallenge struct {
	NodeUUID string `json:"node_uuid,omitempty"`
	Nonce    string `json:"nonce"`
	DataLane bool   `json:"data_lane,omitempty"`
}

// ServerDialProof is the server's response to ServerDialChallenge. On the
// data lane the server also sends AckNonce, a fresh challenge the agent must
// answer in its TDataAck, so the data lane authenticates the agent as the
// control lane's node token does.
type ServerDialProof struct {
	HMAC      string `json:"hmac"`
	Lane      string `json:"lane,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	AckNonce  string `json:"ack_nonce,omitempty"`
}

// ServerDialDataAck is the body of the agent's TDataAck on a server_dial data
// lane: its answer to ServerDialProof.AckNonce.
type ServerDialDataAck struct {
	HMAC string `json:"hmac"`
}

// ServerDialHMAC signs a server_dial challenge nonce with the shared secret.
func ServerDialHMAC(secret, nonce string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(nonce))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyServerDialHMAC checks a server_dial proof in constant time.
func VerifyServerDialHMAC(secret, nonce, proof string) bool {
	want := ServerDialHMAC(secret, nonce)
	return hmac.Equal([]byte(want), []byte(proof))
}

// ServerDialNodeSecret derives the post-registration reverse-auth secret from
// the node token. The server already stores this SHA-256 value for normal node
// authentication, so reconnects do not require persisting the plaintext token.
func ServerDialNodeSecret(nodeToken string) string {
	sum := sha256.Sum256([]byte(nodeToken))
	return hex.EncodeToString(sum[:])
}

// ServerDialLaneHMAC binds a fresh challenge to the connection purpose and
// parent control session. A control proof cannot be replayed as a data proof.
func ServerDialLaneHMAC(secret, nonce, lane, sessionID string) string {
	return ServerDialHMAC(secret, "gpuworks/server-dial/lane/v1\x00"+nonce+"\x00"+lane+"\x00"+sessionID)
}

func VerifyServerDialLaneHMAC(secret, nonce string, proof ServerDialProof) bool {
	return hmac.Equal([]byte(ServerDialLaneHMAC(secret, nonce, proof.Lane, proof.SessionID)), []byte(proof.HMAC))
}

// ServerDialDataAckHMAC is the agent's proof on a server_dial data lane. Its
// purpose tag differs from the server's lane proof, so neither side's proof
// can be replayed as the other's.
func ServerDialDataAckHMAC(secret, ackNonce, sessionID string) string {
	return ServerDialHMAC(secret, "gpuworks/server-dial/data-ack/v1\x00"+ackNonce+"\x00"+sessionID)
}

// VerifyServerDialDataAck checks the agent's data-lane proof in constant time.
func VerifyServerDialDataAck(secret, ackNonce, sessionID, proof string) bool {
	return hmac.Equal([]byte(ServerDialDataAckHMAC(secret, ackNonce, sessionID)), []byte(proof))
}
