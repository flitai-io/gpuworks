package app

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

// The installer judges the bootstrap password against the account the
// server will create on first start; the two must name the same account.
func TestInstallerAndServerNameTheSameAdmin(t *testing.T) {
	if got := DefaultConfig().AdminUser; got != selfinstall.DefaultAdminUser {
		t.Fatalf("server creates %q, installer checks the password against %q", got, selfinstall.DefaultAdminUser)
	}
}
