package app

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// A plain host: ext4 root, nothing else of interest.
const mountInfoPlainHost = `26 1 253:0 / / rw,relatime shared:1 - ext4 /dev/mapper/vg-root rw,errors=remount-ro
27 26 0:6 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw
`

// A RunPod pod as the container sees it: the overlay layer is lost with the
// pod, /workspace is the persistent NFSv4 volume.
const mountInfoRunPod = `2113 1810 0:213 / / rw,relatime master:1 - overlay overlay rw,lowerdir=/var/lib/docker/overlay2/l/ABCDEF:/var/lib/docker/overlay2/l/GHIJKL,upperdir=/var/lib/docker/overlay2/9f2c/diff,workdir=/var/lib/docker/overlay2/9f2c/work
2151 2113 0:216 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
2150 2113 0:45 / /workspace rw,relatime - nfs4 10.1.2.3:/volumes/abcd1234 rw,vers=4.1,rsize=1048576,wsize=1048576,namlen=255,hard,proto=tcp,timeo=600,retrans=2,sec=sys,clientaddr=10.1.2.4,local_lock=none,addr=10.1.2.3
2160 2113 0:52 / /dev/shm rw,nosuid,nodev,noexec,relatime - tmpfs shm rw,size=65536k
`

// A host with a noexec data volume, a read-only archive and a mount point with
// a space in its name.
const mountInfoDataVolumes = `26 1 253:0 / / rw,relatime - ext4 /dev/mapper/vg-root rw
40 26 259:1 / /data rw,nosuid,nodev,noexec,relatime - xfs /dev/nvme0n1p1 rw,attr2
41 26 259:2 / /archive ro,relatime - ext4 /dev/nvme1n1p1 ro
42 26 259:3 / /mnt/my\040disk rw,relatime - ext4 /dev/sdc1 rw
`

func usage(kind, path string) proto.PathUsage {
	return proto.PathUsage{Kind: kind, Path: path, TotalBytes: 200 << 30, AvailableBytes: 150 << 30}
}

func findingsByCode(r proto.DoctorReport, code string) []proto.DoctorFinding {
	var out []proto.DoctorFinding
	for _, f := range r.Findings {
		if f.Code == code {
			out = append(out, f)
		}
	}
	return out
}

func factsByCode(r proto.DoctorReport, code string) []string {
	var out []string
	for _, f := range r.Facts {
		if f.Code == code {
			out = append(out, f.Value)
		}
	}
	return out
}

func diskAt(t *testing.T, r proto.DoctorReport, path string) proto.DoctorDisk {
	t.Helper()
	for _, d := range r.Host.Disks {
		if d.Path == path {
			return d
		}
	}
	t.Fatalf("no disk %s in %+v", path, r.Host.Disks)
	return proto.DoctorDisk{}
}

func TestParseMountInfoReadsTheFieldsAfterTheSeparator(t *testing.T) {
	entries := parseMountInfo([]byte(mountInfoRunPod + "garbage line\n\n" + mountInfoDataVolumes))
	byPoint := map[string]mountEntry{}
	for _, e := range entries {
		byPoint[e.point] = e
	}
	nfs := byPoint["/workspace"]
	if nfs.fsType != "nfs4" || nfs.source != "10.1.2.3:/volumes/abcd1234" || !hasOption(nfs.superOpts, "vers=4.1") || !hasOption(nfs.mountOpts, "relatime") {
		t.Fatalf("nfs entry = %+v", nfs)
	}
	if o := byPoint["/"]; o.fsType != "overlay" && o.fsType != "ext4" {
		t.Fatalf("root entry = %+v", o)
	}
	// Tagged optional fields before the separator ("master:1") do not shift the rest.
	if e := byPoint["/proc"]; e.fsType != "proc" {
		t.Fatalf("proc entry = %+v", e)
	}
	if _, ok := byPoint["/mnt/my disk"]; !ok {
		t.Fatalf("octal escape not undone: %v", entries)
	}
}

func TestMountHoldingPicksTheDeepestContainingMount(t *testing.T) {
	entries := parseMountInfo([]byte(mountInfoRunPod))
	for path, want := range map[string]string{
		"/workspace":                 "/workspace",
		"/workspace/gpuworks/models": "/workspace",
		"/workspace2/models":         "/", // a sibling that shares a prefix is not inside
		"/opt/gpuworks":              "/",
		"/dev/shm/x":                 "/dev/shm",
	} {
		got, ok := mountHolding(entries, path)
		if !ok || got.point != want {
			t.Errorf("mountHolding(%s) = %q ok=%v, want %q", path, got.point, ok, want)
		}
	}
	if _, ok := mountHolding(nil, "/x"); ok {
		t.Fatal("an empty table holds nothing")
	}
}

// The RunPod pod of the field report, installed under /workspace: nothing is
// wrong, and the network filesystem is stated, not judged.
func TestDoctorStatesANetworkFilesystemWithoutJudgingIt(t *testing.T) {
	withNoTools(t)
	withDoctorMounts(t, mountInfoRunPod, "/.dockerenv")
	r := buildDoctorReport(context.Background(), doctorFixedNonNVIDIAProber(), []proto.PathUsage{
		usage("models", "/workspace/gpuworks/models"),
		usage("udocker", "/workspace/gpuworks/udocker"),
		usage(doctorKindAgent, "/workspace/gpuworks"),
	})
	for _, f := range r.Findings {
		if strings.HasPrefix(f.Code, "path_") {
			t.Errorf("unexpected finding %+v", f)
		}
	}
	facts := factsByCode(r, proto.DoctorFactNetworkFilesystem)
	if len(facts) != 1 || facts[0] != "/workspace is nfs4 (10.1.2.3:/volumes/abcd1234); holds models /workspace/gpuworks/models, runveil /workspace/gpuworks/udocker, agent /workspace/gpuworks" {
		t.Fatalf("network facts = %q", facts)
	}
	// The same statement typed, for the console to word in its language.
	for _, f := range r.Facts {
		if f.Code != proto.DoctorFactNetworkFilesystem {
			continue
		}
		var held []string
		for _, d := range f.Disks {
			held = append(held, d.Kind+" "+d.Path+" "+d.Mount.Point)
		}
		if strings.Join(held, ", ") != "models /workspace/gpuworks/models /workspace, udocker /workspace/gpuworks/udocker /workspace, agent /workspace/gpuworks /workspace" {
			t.Fatalf("typed disks = %q", held)
		}
	}
	m := diskAt(t, r, "/workspace/gpuworks/udocker").Mount
	if m == nil || m.FSType != "nfs4" || !m.Network || m.Ephemeral || m.NoExec || m.ReadOnly || !strings.Contains(m.Options, "vers=4.1") {
		t.Fatalf("udocker mount = %+v", m)
	}
	if root := diskAt(t, r, "/").Mount; root == nil || root.FSType != "overlay" || !root.Ephemeral {
		t.Fatalf("root mount = %+v", root)
	}
}

// The same pod installed in the default directory keeps its models and the
// udocker tree on the pod's own layer.
func TestDoctorWarnsWhenPersistentPathsAreOnTheContainerLayer(t *testing.T) {
	withNoTools(t)
	withDoctorMounts(t, mountInfoRunPod, "/.dockerenv")
	r := buildDoctorReport(context.Background(), doctorFixedNonNVIDIAProber(), []proto.PathUsage{
		usage("models", "/opt/gpuworks/models"),
		usage("udocker", "/opt/gpuworks/udocker"),
		usage("docker", "/var/lib/docker"), // dockerd's own data is not the agent's to persist
		usage("storage", "/workspace/shared"),
		usage(doctorKindAgent, "/opt/gpuworks"),
	}, &provisioningDriver{})
	found := findingsByCode(r, proto.DoctorPathEphemeral)
	if len(found) != 1 {
		t.Fatalf("ephemeral findings = %+v", found)
	}
	f := found[0]
	if f.Status != proto.CheckWarn || f.Probe != "/proc/self/mountinfo" || f.Recheck != "findmnt -T /opt/gpuworks/models" || f.Disk == nil || f.Disk.Kind != "models" {
		t.Fatalf("finding = %+v", f)
	}
	want := []proto.DoctorField{
		{Name: "TARGET", Value: "/"}, {Name: "FSTYPE", Value: "overlay"},
		{Name: "OPTIONS", Value: f.Evidence[2].Value}, {Name: "container", Value: "/.dockerenv"},
		{Name: "paths", Value: "/opt/gpuworks/models, /opt/gpuworks/udocker, /opt/gpuworks"},
	}
	if !reflect.DeepEqual(f.Evidence, want) || !strings.Contains(f.Evidence[2].Value, "lowerdir=") {
		t.Fatalf("evidence = %+v", f.Evidence)
	}
}

// An overlay root on a machine that is not a container (no marker file) is an
// ordinary host filesystem, and a container whose data sits on a volume is fine.
func TestDoctorOnlyCallsAnOverlayEphemeralInsideAContainer(t *testing.T) {
	withNoTools(t)
	withDoctorMounts(t, mountInfoRunPod, "")
	r := buildDoctorReport(context.Background(), doctorFixedNonNVIDIAProber(), []proto.PathUsage{usage("models", "/opt/gpuworks/models")})
	if got := findingsByCode(r, proto.DoctorPathEphemeral); len(got) != 0 {
		t.Fatalf("host overlay flagged: %+v", got)
	}
}

// The runveil store is reported for capacity on every node, but on a docker
// node it holds nothing: its mount is not judged there. The agent's own
// directory still has to be writable.
func TestDoctorJudgesTheRunveilStoreOnlyWhenRunveilRuns(t *testing.T) {
	withNoTools(t)
	withDoctorMounts(t, mountInfoDataVolumes, "")
	r := buildDoctorReport(context.Background(), doctorFixedNonNVIDIAProber(), []proto.PathUsage{
		usage("udocker", "/data/gpuworks/udocker"),
		usage(doctorKindAgent, "/archive/gpuworks"),
	}, &doctorDockerDriver{})
	if got := findingsByCode(r, proto.DoctorPathNoExec); len(got) != 0 {
		t.Fatalf("docker node judged the runveil store: %+v", got)
	}
	readOnly := findingsByCode(r, proto.DoctorPathReadOnly)
	if len(readOnly) != 1 || readOnly[0].Disk.Kind != doctorKindAgent {
		t.Fatalf("read-only = %+v", readOnly)
	}
}

func TestDoctorFlagsExecutionAndWritesTheMountForbids(t *testing.T) {
	withNoTools(t)
	withDoctorMounts(t, mountInfoDataVolumes, "")
	r := buildDoctorReport(context.Background(), doctorFixedNonNVIDIAProber(), []proto.PathUsage{
		usage("udocker", "/data/gpuworks/udocker"),
		usage("models", "/data/models"),   // noexec is harmless for model files
		usage("storage", "/archive/pool"), // a read-only archive is a supported storage
		usage(doctorKindAgent, "/archive/gpuworks"),
		usage("models", "/mnt/my disk/models"),
	}, &provisioningDriver{})
	noexec := findingsByCode(r, proto.DoctorPathNoExec)
	if len(noexec) != 1 || noexec[0].Status != proto.CheckFail || noexec[0].Disk.Path != "/data/gpuworks/udocker" ||
		noexec[0].Recheck != "findmnt -T /data/gpuworks/udocker" || noexec[0].Evidence[0].Value != "/data" || noexec[0].Evidence[1].Value != "xfs" {
		t.Fatalf("noexec = %+v", noexec)
	}
	if !strings.Contains(noexec[0].Evidence[2].Value, "noexec") {
		t.Fatalf("options evidence = %+v", noexec[0].Evidence)
	}
	readOnly := findingsByCode(r, proto.DoctorPathReadOnly)
	if len(readOnly) != 1 || readOnly[0].Status != proto.CheckFail || readOnly[0].Disk.Kind != doctorKindAgent || readOnly[0].Disk.Path != "/archive/gpuworks" {
		t.Fatalf("read-only = %+v", readOnly)
	}
	if got := findingsByCode(r, proto.DoctorPathEphemeral); len(got) != 0 {
		t.Fatalf("ephemeral = %+v", got)
	}
	if m := diskAt(t, r, "/mnt/my disk/models").Mount; m == nil || m.Point != "/mnt/my disk" {
		t.Fatalf("mount of a path with a space = %+v", m)
	}
	// Errors sort before the warnings of the same report.
	if len(r.Findings) < 2 || r.Findings[0].Status != proto.CheckFail {
		t.Fatalf("findings = %+v", r.Findings)
	}
}

func TestDoctorWithoutAReadableMountTableReportsNoMounts(t *testing.T) {
	withNoTools(t)
	withDoctorMounts(t, "", "")
	doctorMountTable = func() ([]byte, error) { return nil, context.DeadlineExceeded }
	r := buildDoctorReport(context.Background(), doctorFixedNonNVIDIAProber(), []proto.PathUsage{usage("udocker", "/data/udocker")})
	for _, d := range r.Host.Disks {
		if d.Mount != nil {
			t.Fatalf("disk %s has a mount without a table: %+v", d.Path, d.Mount)
		}
	}
	for _, code := range []string{proto.DoctorPathNoExec, proto.DoctorPathReadOnly, proto.DoctorPathEphemeral} {
		if got := findingsByCode(r, code); len(got) != 0 {
			t.Fatalf("%s without a table: %+v", code, got)
		}
	}
	// The mount checks did not run, and the report says so with why.
	skipped := findingsByCode(r, proto.DoctorSkipMountTableUnreadable)
	if len(skipped) != 1 || skipped[0].ID != proto.DoctorCheckMounts || skipped[0].Status != proto.CheckSkip ||
		skipped[0].Evidence[0].Value != context.DeadlineExceeded.Error() {
		t.Fatalf("skip = %+v", skipped)
	}
	if !strings.Contains(r.Report, "[not checked] MOUNT_TABLE_UNREADABLE") {
		t.Fatalf("report text lacks the skip:\n%s", r.Report)
	}
}

// A disk reported by an agent without mount knowledge decodes, and one with it
// keeps exactly these keys on the wire.
func TestDoctorMountWireForm(t *testing.T) {
	withNoTools(t)
	withDoctorMounts(t, mountInfoRunPod, "/.dockerenv")
	r := buildDoctorReport(context.Background(), doctorFixedNonNVIDIAProber(), []proto.PathUsage{usage("udocker", "/workspace/udocker")})
	raw, err := json.Marshal(diskAt(t, r, "/workspace/udocker"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	mount, _ := got["mount"].(map[string]any)
	if got["kind"] != "udocker" || mount["point"] != "/workspace" || mount["fs_type"] != "nfs4" || mount["network"] != true {
		t.Fatalf("disk json = %s", raw)
	}
	for _, absent := range []string{"noexec", "read_only", "ephemeral"} {
		if _, ok := mount[absent]; ok {
			t.Errorf("false flag %s is on the wire: %s", absent, raw)
		}
	}
}

// The CLI text names the filesystem on each disk line and each path finding.
func TestDoctorTextShowsFilesystemsAndPathFindings(t *testing.T) {
	withNoTools(t)
	withDoctorMounts(t, mountInfoRunPod, "/.dockerenv")
	r := buildDoctorReport(context.Background(), doctorFixedNonNVIDIAProber(), []proto.PathUsage{
		usage("udocker", "/workspace/udocker"),
		usage("models", "/opt/gpuworks/models"),
	})
	for _, want := range []string{
		"/workspace/udocker  200.0 GiB total, 150.0 GiB available (75%)  [nfs4, network]",
		"[warning] PATH_EPHEMERAL: /opt/gpuworks/models\n",
		"container layer",
		"recheck   findmnt -T /opt/gpuworks/models",
		"network filesystem /workspace is nfs4 (10.1.2.3:/volumes/abcd1234); holds runveil /workspace/udocker",
	} {
		if !strings.Contains(r.Report, want) {
			t.Errorf("report text lacks %q:\n%s", want, r.Report)
		}
	}
}
