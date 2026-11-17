package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// fakeAccountDB answers id(1) and getent(1) as a host's NSS would, in the C
// locale: self is `id` (this process), listed `id <account>` (the account
// database now), group the getent line for the socket's gid. An empty
// answer is the command failing, as id does for an unknown account.
type fakeAccountDB struct {
	self, listed, group string
	calls               []string
}

func (f *fakeAccountDB) install(t *testing.T) {
	t.Helper()
	previous := accountCommand
	t.Cleanup(func() { accountCommand = previous })
	accountCommand = func(_ context.Context, name string, args ...string) (string, error) {
		call := strings.Join(append([]string{name}, args...), " ")
		f.calls = append(f.calls, call)
		var out string
		switch {
		case name == "id" && len(args) == 0:
			out = f.self
		case name == "id" && len(args) == 1:
			out = f.listed
		case name == "getent" && len(args) == 2 && args[0] == "group":
			out = f.group
		default:
			return "", fmt.Errorf("unexpected command %q", call)
		}
		if out == "" {
			return "", errors.New("exit status 1")
		}
		return out + "\n", nil
	}
}

// socketOwnedByThisGroup stands in for the daemon socket: what Describe
// reads from it is its group, which for a file the test creates is the
// test's own. The fixtures keep that gid out of the account's groups.
func socketOwnedByThisGroup(t *testing.T) (string, int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docker.sock")
	if err := os.WriteFile(path, nil, 0o660); err != nil {
		t.Fatal(err)
	}
	return path, os.Getegid()
}

func TestDescribeSocketDenialFindsWhatTheOperatorNeeds(t *testing.T) {
	socket, gid := socketOwnedByThisGroup(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		db   fakeAccountDB
		want proto.RuntimeSocketDenial
	}{
		{
			// The E-HPC case: the directory service's own "docker" group
			// (gid 51002) is not the host's, which owns the socket.
			name: "in a directory service's group of the same name",
			db: fakeAccountDB{
				self:   "uid=51001(gpusvc) gid=51001(gpusvc) groups=51001(gpusvc),51002(docker)",
				listed: "uid=51001(gpusvc) gid=51001(gpusvc) groups=51001(gpusvc),51002(docker)",
				group:  fmt.Sprintf("docker:x:%d:", gid),
			},
			want: proto.RuntimeSocketDenial{Socket: socket, Group: "docker", GID: gid, Account: "gpusvc", SameNameGID: 51002},
		},
		{
			// gpasswd -a ran after the agent started: the database lists
			// the group, the process does not hold it.
			name: "added to the group after the agent started",
			db: fakeAccountDB{
				self:   "uid=51001(gpusvc) gid=51001(gpusvc) groups=51001(gpusvc)",
				listed: fmt.Sprintf("uid=51001(gpusvc) gid=51001(gpusvc) groups=51001(gpusvc),%d(docker)", gid),
			},
			want: proto.RuntimeSocketDenial{Socket: socket, Group: "docker", GID: gid, Account: "gpusvc", RestartPending: true},
		},
		{
			name: "names with spaces, an SELinux context, no clash",
			db: fakeAccountDB{
				self:   "uid=51001(svc ops) gid=51003(domain users) groups=51003(domain users),51004 context=unconfined_u:unconfined_r:unconfined_t:s0",
				listed: "uid=51001(svc ops) gid=51003(domain users) groups=51003(domain users),51004",
				group:  fmt.Sprintf("docker:x:%d:alice,bob", gid),
			},
			want: proto.RuntimeSocketDenial{Socket: socket, Group: "docker", GID: gid, Account: "svc ops"},
		},
		{
			// The process has the socket's group and is still refused: the
			// socket's mode (0600) or SELinux/AppArmor, not membership.
			name: "the process already holds the socket's group",
			db: fakeAccountDB{
				self:   fmt.Sprintf("uid=51001(gpusvc) gid=51001(gpusvc) groups=51001(gpusvc),%d(docker)", gid),
				listed: fmt.Sprintf("uid=51001(gpusvc) gid=51001(gpusvc) groups=51001(gpusvc),%d(docker)", gid),
			},
			want: proto.RuntimeSocketDenial{Socket: socket, Group: "docker", GID: gid, Account: "gpusvc", GroupHeld: true},
		},
		{
			// The socket's group is the account's primary one; id need
			// not repeat it under groups=.
			name: "the socket's group is the account's primary group",
			db: fakeAccountDB{
				self:   fmt.Sprintf("uid=51001(gpusvc) gid=%d(docker)", gid),
				listed: fmt.Sprintf("uid=51001(gpusvc) gid=%d(docker)", gid),
			},
			want: proto.RuntimeSocketDenial{Socket: socket, Group: "docker", GID: gid, Account: "gpusvc", GroupHeld: true},
		},
		{
			name: "id and getent both fail",
			db:   fakeAccountDB{},
			want: proto.RuntimeSocketDenial{Socket: socket, GID: gid},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := tc.db
			db.install(t)
			if got := DescribeSocketDenial(ctx, socket); got != tc.want {
				t.Fatalf("denial = %+v\nwant     %+v\ncalls %q", got, tc.want, db.calls)
			}
			if db.self != "" && !slices.Contains(db.calls, "id "+tc.want.Account) {
				t.Fatalf("the account database was not asked: %q", db.calls)
			}
		})
	}
}

// A socket that cannot be read leaves its group unknown (GIDUnknown) rather
// than gid 0, which is root's.
func TestDescribeSocketDenialOfAMissingSocket(t *testing.T) {
	db := fakeAccountDB{self: "uid=51001(gpusvc) gid=51001(gpusvc) groups=51001(gpusvc)", listed: "uid=51001(gpusvc) gid=51001(gpusvc) groups=51001(gpusvc)"}
	db.install(t)
	missing := filepath.Join(t.TempDir(), "docker.sock")
	if got := DescribeSocketDenial(context.Background(), missing); got != (proto.RuntimeSocketDenial{Socket: missing, GID: proto.GIDUnknown, Account: "gpusvc"}) {
		t.Fatalf("denial = %+v", got)
	}
}

func TestSocketDeniedErrorIsAPermissionRefusal(t *testing.T) {
	err := fmt.Errorf("detect: %w", &SocketDeniedError{Socket: "/var/run/docker.sock", Detail: "permission denied while trying to connect"})
	if !IsPermissionDenied(err) || errors.Is(err, ErrDaemonNotReady) {
		t.Fatalf("%v: permission=%t not-ready=%t", err, IsPermissionDenied(err), errors.Is(err, ErrDaemonNotReady))
	}
	if !strings.Contains(err.Error(), "/var/run/docker.sock") {
		t.Fatalf("error %q does not name the socket", err)
	}
}
