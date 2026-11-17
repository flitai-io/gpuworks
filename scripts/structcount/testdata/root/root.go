// Package root is a structcount fixture: every way of hiding a field, a lock,
// a map or an RPC case the ratchet must still see.
package root

import (
	"sync"

	"gpuworks.ai/gpuworks/internal/proto"
)

type uuidSet map[string]bool

type images struct {
	mu   sync.Mutex
	refs map[string]int
}

type Root struct {
	aMu, bMu sync.Mutex     // two locks on one line
	cMu      *sync.RWMutex  // a lock by pointer
	m1, m2   map[string]int // two maps on one line
	cache    sync.Map
	seen     uuidSet
	nested   struct { // wrapping does not hide them
		dMu  sync.Mutex
		byID map[string]int
	}
	images *images // a subsystem of its own counts once
	sync.Mutex
}

func (r *Root) OnRPC(method string) {
	switch method {
	case proto.RPCPing, proto.RPCDoctor:
	default:
		r.more(method)
	}
}

func (r Root) more(method string) {
	switch method {
	case proto.RPCPing: // counted once
	case proto.RPCSmoke:
	}
}

func (i *images) pull() {}
