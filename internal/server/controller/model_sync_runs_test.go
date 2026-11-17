package controller

// register puts run under key, as a peer sync's admission does.
func (r *modelSyncRuns) register(key string, run *modelSyncRun) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.put(key, run)
}

// current is the run registered under key, nil when there is none.
func (r *modelSyncRuns) current(key string) *modelSyncRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byKey[key]
}
