package conn

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

// processInstanceID names this agent process in its hellos: random per
// process start (an in-place exec starts a new one), the same for every
// connection the process makes. The server tells two processes sharing one
// node token from one process reconnecting by it.
var processInstanceID = sync.OnceValue(func() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
})
