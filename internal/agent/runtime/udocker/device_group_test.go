package udocker

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

func node(mode os.FileMode, uid, gid uint32) deviceInfo {
	return deviceInfo{name: "n", mode: mode, stat: &syscall.Stat_t{Uid: uid, Gid: gid}}
}

func TestRootOnlyDeviceMatchesOnlyRootRootWithoutGroupOrOtherBits(t *testing.T) {
	for _, tc := range []struct {
		name string
		info deviceInfo
		want bool
	}{
		{"root:root 0600", node(os.ModeDevice|0o600, 0, 0), true},
		{"root:root 0400", node(os.ModeDevice|0o400, 0, 0), true},
		{"root:root 0660 keeps root group", node(os.ModeDevice|0o660, 0, 0), false},
		{"root:root 0640 has group read", node(os.ModeDevice|0o640, 0, 0), false},
		{"root:root 0606 has other access", node(os.ModeDevice|0o606, 0, 0), false},
		{"owned by a device group", node(os.ModeDevice|0o600, 0, 1234), false},
		{"owned by another user", node(os.ModeDevice|0o600, 1000, 0), false},
		{"regular file", node(0o600, 0, 0), false},
		{"no stat metadata", deviceInfo{name: "n", mode: os.ModeDevice | 0o600}, false},
	} {
		if got := rootOnlyDevice(tc.info); got != tc.want {
			t.Errorf("%s: rootOnlyDevice = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// fakeDeviceHost is an in-memory host: device nodes with ownership and modes,
// a group table, and injectable failures for each operation.
type fakeDeviceHost struct {
	nodes      map[string]deviceInfo
	groups     map[string]string
	addCalls   int
	lookups    int
	addErr     error
	addCreates bool // whether a failing groupadd still created the group (concurrent creator)
	lookupErr  error
	chownErr   error
	chmodErr   error
	chowns     []string
	chmods     []string
}

func (h *fakeDeviceHost) ops() deviceGroupOps {
	return deviceGroupOps{
		stat: func(path string) (os.FileInfo, error) {
			info, ok := h.nodes[path]
			if !ok {
				return nil, os.ErrNotExist
			}
			return info, nil
		},
		lookupGroup: func(name string) (*user.Group, error) {
			h.lookups++
			if h.lookupErr != nil {
				return nil, h.lookupErr
			}
			gid, ok := h.groups[name]
			if !ok {
				return nil, user.UnknownGroupError(name)
			}
			return &user.Group{Name: name, Gid: gid}, nil
		},
		addGroup: func(_ context.Context, name string) error {
			h.addCalls++
			if h.addErr != nil {
				if h.addCreates {
					h.groups[name] = "990"
				}
				return h.addErr
			}
			h.groups[name] = "990"
			return nil
		},
		chown: func(path string, uid, gid int) error {
			if h.chownErr != nil {
				return h.chownErr
			}
			if uid != -1 {
				return errors.New("owner must stay unchanged")
			}
			info := h.nodes[path]
			info.stat = &syscall.Stat_t{Uid: info.stat.Uid, Gid: uint32(gid)}
			h.nodes[path] = info
			h.chowns = append(h.chowns, path)
			return nil
		},
		chmod: func(path string, mode os.FileMode) error {
			if h.chmodErr != nil {
				return h.chmodErr
			}
			info := h.nodes[path]
			info.mode = os.ModeDevice | mode
			h.nodes[path] = info
			h.chmods = append(h.chmods, path)
			return nil
		},
	}
}

func ascendHost() *fakeDeviceHost {
	return &fakeDeviceHost{
		nodes: map[string]deviceInfo{
			"/dev/davinci4":         node(os.ModeDevice|0o660, 0, 1234),
			"/dev/davinci_manager":  node(os.ModeDevice|0o600, 0, 0),
			"/dev/devmm_svm":        node(os.ModeDevice|0o600, 0, 0),
			"/usr/local/dcmi":       node(os.ModeDir|0o755, 0, 0),
			"/dev/already-adopted0": node(os.ModeDevice|0o660, 0, 990),
		},
		groups: map[string]string{"HwHiAiUser": "1234"},
	}
}

func mountsOf(paths ...string) []runtime.Mount {
	out := make([]runtime.Mount, len(paths))
	for i, p := range paths {
		out[i] = runtime.Mount{Source: p}
	}
	return out
}

func TestAdoptRootOnlyDevicesGivesOnlyRootOnlyNodesToTheRuntimeGroup(t *testing.T) {
	h := ascendHost()
	mounts := mountsOf("/dev/davinci4", "/dev/davinci_manager", "/dev/devmm_svm", "/usr/local/dcmi", "/dev/missing")
	adopted, err := adoptRootOnlyDevices(context.Background(), mounts, h.ops())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/dev/davinci_manager", "/dev/devmm_svm"}
	if !slices.Equal(adopted, want) || !slices.Equal(h.chowns, want) || !slices.Equal(h.chmods, want) {
		t.Fatalf("adopted=%v chowns=%v chmods=%v, want %v", adopted, h.chowns, h.chmods, want)
	}
	if h.addCalls != 1 || h.groups[RuntimeDeviceGroup] != "990" {
		t.Fatalf("groupadd calls=%d groups=%v, want the runtime group created once", h.addCalls, h.groups)
	}
	for _, path := range want {
		info := h.nodes[path]
		if info.Mode().Perm() != 0o660 || info.stat.Gid != 990 || info.stat.Uid != 0 {
			t.Fatalf("%s mode=%v uid=%d gid=%d, want root:gpuworks-runtime 0660", path, info.Mode().Perm(), info.stat.Uid, info.stat.Gid)
		}
	}
	if info := h.nodes["/dev/davinci4"]; info.Mode().Perm() != 0o660 || info.stat.Gid != 1234 {
		t.Fatalf("group-owned device changed: mode=%v gid=%d", info.Mode().Perm(), info.stat.Gid)
	}

	// The next workload start finds nothing left to adopt and creates nothing.
	h.chowns, h.chmods = nil, nil
	adopted, err = adoptRootOnlyDevices(context.Background(), mounts, h.ops())
	if err != nil || len(adopted) != 0 || len(h.chowns) != 0 || h.addCalls != 1 {
		t.Fatalf("second pass adopted=%v chowns=%v groupadd=%d err=%v, want idempotent", adopted, h.chowns, h.addCalls, err)
	}
}

func TestAdoptRootOnlyDevicesReusesAnExistingGroupAndSkipsLookupWithoutRootOnlyNodes(t *testing.T) {
	h := ascendHost()
	h.groups[RuntimeDeviceGroup] = "990"
	if _, err := adoptRootOnlyDevices(context.Background(), mountsOf("/dev/davinci_manager"), h.ops()); err != nil {
		t.Fatal(err)
	}
	if h.addCalls != 0 {
		t.Fatalf("groupadd called %d times for an existing group", h.addCalls)
	}

	h = ascendHost()
	if _, err := adoptRootOnlyDevices(context.Background(), mountsOf("/dev/davinci4", "/usr/local/dcmi"), h.ops()); err != nil {
		t.Fatal(err)
	}
	if h.lookups != 0 || h.addCalls != 0 {
		t.Fatalf("lookups=%d groupadd=%d without root-only nodes, want none", h.lookups, h.addCalls)
	}
}

func TestAdoptRootOnlyDevicesToleratesAConcurrentGroupCreation(t *testing.T) {
	h := ascendHost()
	h.addErr, h.addCreates = errors.New("groupadd: group 'gpuworks-runtime' already exists"), true
	adopted, err := adoptRootOnlyDevices(context.Background(), mountsOf("/dev/davinci_manager"), h.ops())
	if err != nil || len(adopted) != 1 {
		t.Fatalf("adopted=%v err=%v, want the concurrently created group used", adopted, err)
	}
}

func TestAdoptRootOnlyDevicesReportsEachFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*fakeDeviceHost)
		want   string
	}{
		{"groupadd fails", func(h *fakeDeviceHost) { h.addErr = errors.New("groupadd: permission denied") }, "create group gpuworks-runtime: groupadd: permission denied"},
		{"group lookup fails", func(h *fakeDeviceHost) { h.lookupErr = errors.New("nss down") }, "look up group gpuworks-runtime: nss down"},
		{"chown fails", func(h *fakeDeviceHost) { h.chownErr = syscall.EPERM }, "give root-only accelerator device /dev/davinci_manager to group gpuworks-runtime"},
		{"chmod fails", func(h *fakeDeviceHost) { h.chmodErr = syscall.EROFS }, "grant group gpuworks-runtime read/write on accelerator device /dev/davinci_manager"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := ascendHost()
			tc.mutate(h)
			_, err := adoptRootOnlyDevices(context.Background(), mountsOf("/dev/davinci_manager", "/dev/devmm_svm"), h.ops())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if len(h.chmods) != 0 && tc.name == "chown fails" {
				t.Fatalf("chmod ran after chown failed: %v", h.chmods)
			}
		})
	}
}

func TestRequiredDeviceGroupsPredictsAdoptionOrNamesTheFix(t *testing.T) {
	h := ascendHost()
	stat := h.ops().stat
	lookup := func(gid string) (*user.Group, error) {
		if gid == "1234" {
			return &user.Group{Gid: gid, Name: "HwHiAiUser"}, nil
		}
		t.Fatalf("unexpected group lookup %q", gid)
		return nil, nil
	}
	mounts := mountsOf("/dev/davinci4", "/dev/davinci_manager", "/dev/devmm_svm")

	needs, err := requiredDeviceGroups(mounts, 1000, 1000, map[int]bool{1000: true}, stat, lookup, true)
	if err != nil {
		t.Fatal(err)
	}
	if needs.devices != 3 || !slices.Equal(needs.labels(), []string{"HwHiAiUser", RuntimeDeviceGroup}) {
		t.Fatalf("devices=%d groups=%v, want 3 and HwHiAiUser+%s", needs.devices, needs.labels(), RuntimeDeviceGroup)
	}
	if needs.groups[0].gid != 1234 || needs.groups[1].gid != adoptedDeviceGroupID {
		t.Fatalf("groups = %+v, want the real gid and the predicted adoption", needs.groups)
	}

	_, err = requiredDeviceGroups(mounts, 1000, 1000, map[int]bool{1000: true}, stat, lookup, false)
	if err == nil || !strings.Contains(err.Error(), "root-only accelerator device /dev/davinci_manager") || !strings.Contains(err.Error(), "grant-device-access") {
		t.Fatalf("err = %v, want the root-only node and the grant-device-access fix named", err)
	}
}

func TestPrepareDeviceAccessLeavesHostsAloneWithoutARootRunUser(t *testing.T) {
	dir := t.TempDir()
	dev := filepath.Join(dir, "not-a-device")
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d := New(t.TempDir())
	spec := runtime.WorkloadSpec{Mounts: []runtime.Mount{{Source: dev, Target: dev}}}
	if err := d.prepareDeviceAccess(context.Background(), spec); err != nil {
		t.Fatalf("prepareDeviceAccess without a run user: %v", err)
	}
	info, err := os.Stat(dev)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file changed: mode=%v err=%v", info.Mode().Perm(), err)
	}
}

// A launcher that cannot inject supplementary groups needs the membership to
// exist before the workload starts, so the advice has to be runnable as
// printed: usermod takes names, and a group the host cannot name has to be
// given one first.
func TestMembershipAdviceNamesEveryGroupItTellsTheOperatorToJoin(t *testing.T) {
	for _, tc := range []struct {
		name  string
		needs deviceGroupNeeds
		want  string
	}{
		{
			name:  "named group",
			needs: deviceGroupNeeds{groups: []deviceGroup{{gid: 1001, name: "davinci"}}},
			want:  "sudo usermod -aG davinci gpuworks",
		},
		{
			name:  "group the host cannot name",
			needs: deviceGroupNeeds{groups: []deviceGroup{{gid: 1001}}},
			want:  "sudo groupadd --system --gid 1001 gpuworks-device-1001 && sudo usermod -aG gpuworks-device-1001 gpuworks",
		},
		{
			name:  "named and nameless together",
			needs: deviceGroupNeeds{groups: []deviceGroup{{gid: 990, name: "hisi"}, {gid: 1001}}},
			want:  "sudo groupadd --system --gid 1001 gpuworks-device-1001 && sudo usermod -aG hisi,gpuworks-device-1001 gpuworks",
		},
	} {
		got := tc.needs.membershipAdvice("gpuworks")
		if got != tc.want {
			t.Errorf("%s: membershipAdvice = %q, want %q", tc.name, got, tc.want)
		}
		if strings.Contains(got, "usermod -aG 1001") {
			t.Errorf("%s: advice passes a numeric id to usermod, which only takes names: %q", tc.name, got)
		}
	}
}

// os/user without cgo reports a missing or unreadable /etc/group as a path
// error rather than an unknown id, and such a host can still answer through
// NSS, so the getent fallback must not be reserved for the unknown-id case.
func TestLookupGroupIDAsksNSSWhateverTheLocalFailureWas(t *testing.T) {
	absent := func() []string {
		var out []string
		for gid := 64000; gid < 64100 && len(out) < 2; gid++ {
			id := strconv.Itoa(gid)
			if _, err := user.LookupGroupId(id); err != nil {
				out = append(out, id)
			}
		}
		return out
	}()
	if len(absent) < 2 {
		t.Fatalf("need two group ids this host does not know, found %v", absent)
	}
	known, unknown := absent[0], absent[1]

	dir := t.TempDir()
	stub := filepath.Join(dir, "getent")
	script := "#!/bin/sh\nif [ \"$1\" = group ] && [ \"$2\" = " + known + " ]; then echo 'ldap-davinci:x:" + known + ":someone'; exit 0; fi\nexit 2\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	group, err := lookupGroupID(known)
	if err != nil {
		t.Fatalf("lookupGroupID(%s): %v", known, err)
	}
	if group.Name != "ldap-davinci" || group.Gid != known {
		t.Fatalf("lookupGroupID(%s) = %+v, want name ldap-davinci", known, group)
	}
	if _, err := lookupGroupID(unknown); err == nil {
		t.Fatalf("lookupGroupID(%s) of a gid NSS does not know either: want an error", unknown)
	}
}
