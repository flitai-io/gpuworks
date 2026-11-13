package selfinstall

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// otherAccount is an account that owns nothing in the fixture tree and shares
// no group with it: what the kernel judges by the "other" bits.
func otherAccount(string) (Account, error) {
	return Account{UID: strconv.Itoa(os.Getuid() + 4242), GroupIDs: []string{strconv.Itoa(os.Getgid() + 4242)}}, nil
}

func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the service account reach check runs only under systemd")
	}
}

func TestCanSearchFollowsTheKernelClassOrder(t *testing.T) {
	owner := searchIdentity{uid: 10, hasUID: true, gids: map[uint32]bool{20: true}}
	member := searchIdentity{uid: 11, hasUID: true, gids: map[uint32]bool{20: true}}
	stranger := searchIdentity{uid: 12, hasUID: true, gids: map[uint32]bool{}}
	toBeCreated := searchIdentity{gids: map[uint32]bool{20: true}}
	for _, tc := range []struct {
		name string
		id   searchIdentity
		mode os.FileMode
		want bool
	}{
		{"owner with u+x", owner, 0o700, true},
		// The owner class alone decides: the group and other bits do not help.
		{"owner without u+x", owner, 0o077, false},
		{"member with g+x", member, 0o710, true},
		{"member without g+x", member, 0o701, false},
		{"stranger with o+x", stranger, 0o701, true},
		{"stranger on /root", stranger, 0o700, false},
		{"account to be created in the group", toBeCreated, 0o710, true},
		// An account without a uid yet is never the owner.
		{"account to be created outside the group", toBeCreated, 0o700, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := dirAccess{uid: 10, gid: 20, mode: os.ModeDir | tc.mode}
			if got := tc.id.canSearch(d); got != tc.want {
				t.Fatalf("canSearch(%v) = %v, want %v", tc.mode, got, tc.want)
			}
		})
	}
}

// reachFixture lays out parent/locked/server, with the install directory
// inside a directory whose mode the test sets.
func reachFixture(t *testing.T, mode os.FileMode) (*harness, string) {
	t.Helper()
	requireLinux(t)
	base := mustEvalSymlinks(t, t.TempDir())
	// The test's temporary root is 0700 itself; open it so only the mode under
	// test can deny.
	if err := os.Chmod(filepath.Dir(base), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(base, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	h := newHarness(t, filepath.Join(locked, "server"))
	h.installer.deps.LookupAccount = otherAccount
	return h, locked
}

func TestServiceUserReachRefusesAnUnenterableAncestor(t *testing.T) {
	h, locked := reachFixture(t, 0o700)
	roots, err := h.installer.permissionRoots()
	if err != nil {
		t.Fatal(err)
	}
	err = h.installer.checkServiceUserReach(Preflight{Mode: ModeSystemd, ServiceUser: DefaultServiceUser}, roots)
	if err == nil {
		t.Fatal("an install below a 0700 directory of another account was accepted")
	}
	for _, want := range []string{"cannot enter " + locked, "rwx------", "--service-user root", h.layout.Binary()} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

func TestServiceUserReachAcceptsSearchOnlyAncestors(t *testing.T) {
	// Search without read is all a path walk needs.
	h, _ := reachFixture(t, 0o711)
	roots, err := h.installer.permissionRoots()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.installer.checkServiceUserReach(Preflight{Mode: ModeSystemd, ServiceUser: DefaultServiceUser}, roots); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestServiceUserReachSkipsWhatTheInstallCreates(t *testing.T) {
	// Missing ancestors are created traversable by mkdir -p, and the install
	// directory itself is handed to the account.
	h, locked := reachFixture(t, 0o755)
	h.layout = NewLayout(filepath.Join(locked, "a", "b", "server"), "", "")
	h.installer = New(h.layout, h.installer.deps, h.io)
	roots, err := h.installer.permissionRoots()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.installer.checkServiceUserReach(Preflight{Mode: ModeSystemd, ServiceUser: DefaultServiceUser}, roots); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestServiceUserReachChecksASeparateDataDirectory(t *testing.T) {
	h, locked := reachFixture(t, 0o755)
	privateParent := filepath.Join(locked, "private")
	if err := os.Mkdir(privateParent, 0o700); err != nil {
		t.Fatal(err)
	}
	h.layout = NewLayout(h.layout.Dir, filepath.Join(privateParent, "data"), "")
	h.installer = New(h.layout, h.installer.deps, h.io)
	roots, err := h.installer.permissionRoots()
	if err != nil {
		t.Fatal(err)
	}
	err = h.installer.checkServiceUserReach(Preflight{Mode: ModeSystemd, ServiceUser: DefaultServiceUser}, roots)
	if err == nil || !strings.Contains(err.Error(), "cannot enter "+privateParent) {
		t.Fatalf("err = %v", err)
	}
}

func TestServiceUserReachOnlyAppliesToADedicatedSystemdAccount(t *testing.T) {
	h, _ := reachFixture(t, 0o700)
	roots, err := h.installer.permissionRoots()
	if err != nil {
		t.Fatal(err)
	}
	for _, pf := range []Preflight{
		{Mode: ModeSystemd, ServiceUser: "root"},
		{Mode: ModeSystemd},
		{Mode: ModeNohup, ServiceUser: DefaultServiceUser},
		{Mode: ModeLaunchd, ServiceUser: DefaultServiceUser},
	} {
		if err := h.installer.checkServiceUserReach(pf, roots); err != nil {
			t.Fatalf("%+v: err = %v", pf, err)
		}
	}
}

func TestServiceUserReachUsesTheGroupOfAnAccountToBeCreated(t *testing.T) {
	// The account does not exist yet; useradd will put it in the credential
	// group, which the fixture maps to the suite's own group.
	h, locked := reachFixture(t, 0o750)
	if err := os.Chown(locked, -1, os.Getegid()); err != nil {
		t.Fatal(err)
	}
	h.installer.deps.LookupAccount = func(name string) (Account, error) { return Account{}, user.UnknownUserError(name) }
	roots, err := h.installer.permissionRoots()
	if err != nil {
		t.Fatal(err)
	}
	pf := Preflight{Mode: ModeSystemd, ServiceUser: DefaultServiceUser}
	if err := h.installer.checkServiceUserReach(pf, roots); err != nil {
		t.Fatalf("group search was not credited: %v", err)
	}
	if err := os.Chmod(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := h.installer.checkServiceUserReach(pf, roots); err == nil {
		t.Fatal("a directory only its owner can enter was accepted for an account to be created")
	}
}

func TestServiceUserReachReportsAFailedAccountLookup(t *testing.T) {
	h, _ := reachFixture(t, 0o755)
	h.installer.deps.LookupAccount = func(string) (Account, error) { return Account{}, errors.New("passwd unreadable") }
	err := h.installer.checkServiceUserReach(Preflight{Mode: ModeSystemd, ServiceUser: DefaultServiceUser}, []string{h.layout.Dir})
	if err == nil || !strings.Contains(err.Error(), "passwd unreadable") {
		t.Fatalf("err = %v", err)
	}
	h.installer.deps.LookupAccount = func(string) (Account, error) { return Account{UID: "nobody"}, nil }
	err = h.installer.checkServiceUserReach(Preflight{Mode: ModeSystemd, ServiceUser: DefaultServiceUser}, []string{h.layout.Dir})
	if err == nil || !strings.Contains(err.Error(), `uid "nobody"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallRefusesAnInstallDirectoryTheServiceCannotReach(t *testing.T) {
	h, locked := reachFixture(t, 0o700)
	h.installSelf("binary")

	_, err := h.installer.Install(context.Background(), h.quiet())
	if err == nil || !strings.Contains(err.Error(), "cannot enter "+locked) || !strings.Contains(err.Error(), "--skip-check") {
		t.Fatalf("err = %v", err)
	}
	if fileExists(h.layout.Binary()) || len(h.run.executed()) != 0 {
		t.Fatalf("install changed the host before refusing: %v", h.run.executed())
	}
}

func TestInstallSkipCheckInstallsBelowAnUnreachableDirectory(t *testing.T) {
	h, locked := reachFixture(t, 0o700)
	h.installSelf("binary")
	req := h.quiet()
	req.SkipCheck = true

	report := h.mustInstall(req)

	if report.Mode != ModeSystemd || !fileExists(h.layout.Binary()) {
		t.Fatalf("report = %+v", report)
	}
	if !strings.Contains(h.errOut.String(), "warning: ignored because of --skip-check: the service account gpuworks cannot enter "+locked) {
		t.Fatalf("the skipped check was not reported: %s", h.errOut.String())
	}
}

func TestInstallSkipCheckDowngradesTheHostChecks(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	h.freeBytes = 1 << 30
	req := h.quiet()
	req.SkipCheck = true

	h.mustInstall(req)

	if !strings.Contains(h.errOut.String(), "warning: ignored because of --skip-check:") ||
		!strings.Contains(h.errOut.String(), "at least 2 GiB") {
		t.Fatalf("the skipped disk check was not reported: %s", h.errOut.String())
	}
}

func TestInstallSkipCheckKeepsTheHostSafetyGuards(t *testing.T) {
	// Handing a broad directory to the service account is not a prediction
	// about the server; it damages the host, and no flag waives it.
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	h.layout = NewLayout(h.layout.Dir, "/var", "")
	h.installer = New(h.layout, h.deps, h.io)
	req := h.quiet()
	req.SkipCheck = true
	req.DataDirSet = true

	_, err := h.installer.Install(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "refusing recursive") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "--skip-check") {
		t.Fatalf("a safety guard offered --skip-check: %v", err)
	}
}
