package controller

import (
	"sync"
	"time"
)

// poolMemory is what the controller remembers about each deployment's
// candidate pool between passes: the candidates whose trial plan refused a
// replica recently, and when its last pass found no node. Its zero value is
// ready to use; the caller passes the time.
type poolMemory struct {
	mu      sync.Mutex
	refused map[uint]map[uint]time.Time // by deployment, then node
	waiting map[uint]time.Time          // by deployment
}

// refuse remembers that node refused a replica of dep at now.
func (m *poolMemory) refuse(dep, node uint, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.refused == nil {
		m.refused = map[uint]map[uint]time.Time{}
	}
	if m.refused[dep] == nil {
		m.refused[dep] = map[uint]time.Time{}
	}
	m.refused[dep][node] = now
}

// refusedRecently reports whether node refused a replica of dep within
// poolRefusalMemory of now, dropping an older refusal.
func (m *poolMemory) refusedRecently(dep, node uint, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.refused[dep][node]
	if !ok {
		return false
	}
	if now.Sub(at) >= poolRefusalMemory {
		delete(m.refused[dep], node)
		return false
	}
	return true
}

// refusals is how many candidates of dep are remembered as refusing.
func (m *poolMemory) refusals(dep uint) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.refused[dep])
}

// wait remembers that dep's pass at now found no node.
func (m *poolMemory) wait(dep uint, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.waiting == nil {
		m.waiting = map[uint]time.Time{}
	}
	m.waiting[dep] = now
}

// settled forgets that dep was waiting: its pass placed what it needed.
func (m *poolMemory) settled(dep uint) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.waiting, dep)
}

// waitingRecently reports whether dep's last pass found no node within
// poolSettleBackoff of now, dropping an older wait.
func (m *poolMemory) waitingRecently(dep uint, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.waiting[dep]
	if !ok {
		return false
	}
	if now.Sub(at) >= poolSettleBackoff {
		delete(m.waiting, dep)
		return false
	}
	return true
}

// forget drops everything remembered about dep's pool.
func (m *poolMemory) forget(dep uint) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.refused, dep)
	delete(m.waiting, dep)
}
