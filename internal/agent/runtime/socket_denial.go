package runtime

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// accountCommand runs id(1) or getent(1) and returns what it printed. The
// account database may be a directory service (NIS, LDAP) that only the
// host's NSS reaches, which these do and a pure-Go /etc/group read does
// not. A variable so a test can answer for the host.
var accountCommand = func(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// id(1) translates "uid=" and "groups=" under a non-English locale.
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	return string(out), err
}

// DescribeSocketDenial is what an operator needs to let the agent's account
// open socket: the group that owns it, the account, whether the account is
// in another group of that name (a directory service's), and whether the
// account database already lists it in the group so that only a restart of
// the agent is missing — or that the process holds the group already, and
// membership is not what refuses it. What cannot be found out stays empty.
func DescribeSocketDenial(ctx context.Context, socket string) proto.RuntimeSocketDenial {
	d := proto.RuntimeSocketDenial{Socket: socket, GID: proto.GIDUnknown}
	gid := proto.GIDUnknown
	if info, err := os.Stat(socket); err == nil {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			gid = int(stat.Gid)
			d.GID = gid
		}
	}
	// `id` with no account reports this process: the groups it started with.
	self, selfOK := identity(ctx)
	if selfOK {
		d.Account = self.account
	}
	var listed accountIdentity
	listedOK := false
	if d.Account != "" {
		// `id <account>` asks the account database now, as a new login would.
		listed, listedOK = identity(ctx, d.Account)
	}
	if gid < 0 {
		return d
	}
	d.Group = self.groups[gid]
	if d.Group == "" {
		d.Group = listed.groups[gid]
	}
	if d.Group == "" {
		d.Group = groupName(ctx, gid)
	}
	if d.Group != "" {
		d.SameNameGID = sameNameGroup(self, d.Group, gid)
		if d.SameNameGID == 0 {
			d.SameNameGID = sameNameGroup(listed, d.Group, gid)
		}
	}
	// `id` reports this process's own groups (getgroups and the effective
	// gid), so it answers whether the process holds the socket's group.
	_, held := self.groups[gid]
	d.GroupHeld = selfOK && held
	if selfOK && listedOK && !d.GroupHeld {
		_, inDB := listed.groups[gid]
		d.RestartPending = inDB
	}
	return d
}

// accountIdentity is what id(1) printed: the account and its groups, gid to
// name ("" when the gid has no name).
type accountIdentity struct {
	account string
	groups  map[int]string
}

var (
	idUser  = regexp.MustCompile(`uid=\d+(?:\(([^)]*)\))?`)
	idGIDs  = regexp.MustCompile(`(?:^| )e?gid=(\d+)(?:\(([^)]*)\))?`)
	idGroup = regexp.MustCompile(`(\d+)(?:\(([^)]*)\))?`)
)

// identity runs `id [account]` and parses its C-locale answer,
// "uid=1001(gpusvc) gid=1001(gpusvc) groups=1001(gpusvc),1002(docker)".
// A name may hold spaces ("domain users"), so the answer is not split on
// them.
func identity(ctx context.Context, account ...string) (accountIdentity, bool) {
	out, err := accountCommand(ctx, "id", account...)
	if err != nil {
		return accountIdentity{}, false
	}
	id := accountIdentity{groups: map[int]string{}}
	if m := idUser.FindStringSubmatch(out); m != nil {
		id.account = m[1]
	}
	// The primary (and an effective) gid count as held whether or not
	// groups= repeats them.
	for _, m := range idGIDs.FindAllStringSubmatch(out, -1) {
		if g, err := strconv.Atoi(m[1]); err == nil {
			id.groups[g] = m[2]
		}
	}
	if len(id.groups) == 0 {
		return accountIdentity{}, false
	}
	if _, groups, ok := strings.Cut(out, "groups="); ok {
		groups, _, _ = strings.Cut(groups, " context=")
		for _, m := range idGroup.FindAllStringSubmatch(groups, -1) {
			if g, err := strconv.Atoi(m[1]); err == nil {
				id.groups[g] = m[2]
			}
		}
	}
	return id, true
}

// sameNameGroup is the id of a group in id named name that is not gid, or 0.
func sameNameGroup(id accountIdentity, name string, gid int) int {
	for g, n := range id.groups {
		if n == name && g != gid {
			return g
		}
	}
	return 0
}

// groupName is the name the account database gives gid, or "".
func groupName(ctx context.Context, gid int) string {
	out, err := accountCommand(ctx, "getent", "group", strconv.Itoa(gid))
	if err != nil {
		return ""
	}
	name, _, _ := strings.Cut(strings.TrimSpace(out), ":")
	return name
}
