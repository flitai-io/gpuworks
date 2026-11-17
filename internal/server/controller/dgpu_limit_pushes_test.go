package controller

import "testing"

// isPending reports whether dep has a push pending, without taking it.
func (p *dgpuLimitPushes) isPending(dep uint) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pending[dep]
}

// A pending push is taken once; forgetting drops it.
func TestDGPULimitPushesTakeOnce(t *testing.T) {
	var p dgpuLimitPushes
	if p.take(1) {
		t.Fatal("nothing marked, yet a push was taken")
	}
	p.mark(1)
	p.mark(2)
	if !p.take(1) || p.take(1) {
		t.Fatal("a marked push is not taken exactly once")
	}
	p.forget(2)
	if p.take(2) {
		t.Fatal("a forgotten push was taken")
	}
}
