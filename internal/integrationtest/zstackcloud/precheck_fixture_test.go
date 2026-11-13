package zstackcloud_test

import (
	"context"
	"fmt"
	"sync"

	"gpuworks.ai/gpuworks/internal/server/provision"
)

// sshPrecheckFixture stands in for the SSH precheck an install runs first.
// The Cloud fixtures list hosts at documentation addresses with no SSH server
// behind them: the real precheck would wait out its connection timeout on
// every install (about twenty seconds), and how long depends on the network
// the test runs on. The fixture answers as the real one does: a report for
// the target it parsed, opening with the SSH authentication row, or, for an
// unreachable host, the dial error the service turns into the SSH row.
type sshPrecheckFixture struct {
	mu          sync.Mutex
	requests    []provision.SSHPrecheckRequest
	unreachable bool
}

func (f *sshPrecheckFixture) PrecheckSSH(_ context.Context, req provision.SSHPrecheckRequest) (provision.SSHPrecheckReport, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	unreachable := f.unreachable
	f.mu.Unlock()
	user, host, port, err := provision.ParseTarget(req.Target, req.User, req.Port)
	if err != nil {
		return provision.SSHPrecheckReport{}, err
	}
	if unreachable {
		return provision.SSHPrecheckReport{}, fmt.Errorf("dial tcp %s: connect: connection refused", req.Target)
	}
	return provision.SSHPrecheckReport{Target: req.Target, User: user, Host: host, Port: port, Checks: []provision.CheckResult{
		{Name: "SSH authentication", Status: "ok", Detail: "password accepted"},
	}}, nil
}

func (f *sshPrecheckFixture) Requests() []provision.SSHPrecheckRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]provision.SSHPrecheckRequest(nil), f.requests...)
}
