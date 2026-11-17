package app

import (
	"os"
	"path/filepath"
	"strings"

	udockerdrv "gpuworks.ai/gpuworks/internal/agent/runtime/udocker"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Doctor path kinds the checks below care about, besides the ones the node
// reports for capacity (root, models, docker, udocker, storage).
const doctorKindAgent = "agent"

// doctorKindRunveil is the runveil runtime's store, reported for capacity on
// every node under its wire name.
const doctorKindRunveil = "udocker"

// Which kinds of path a mount flag matters for. The runveil runtime executes
// from its store and the agent's own directory and writes to them; every
// runtime writes to the models root. Storage mounts may legitimately be
// read-only (a shared model archive) and the docker data root belongs to
// dockerd, so neither is judged. The runveil kinds are judged only when
// runveil is the selected runtime: on a docker node they hold nothing.
var (
	doctorExecKinds      = map[string]bool{doctorKindRunveil: true, doctorKindAgent: true}
	doctorWriteKinds     = map[string]bool{"models": true, doctorKindRunveil: true, doctorKindAgent: true}
	doctorPersistedKinds = map[string]bool{"models": true, doctorKindRunveil: true, doctorKindAgent: true}
	doctorRunveilKinds   = map[string]bool{doctorKindRunveil: true, doctorKindAgent: true}
)

// doctorJudged reports whether a mount check applies to a path of kind on a
// node whose selected runtime is runtimeName.
func doctorJudged(kinds map[string]bool, kind, runtimeName string, write bool) bool {
	if !kinds[kind] {
		return false
	}
	if runtimeName == doctorKindRunveil {
		return true
	}
	// Without runveil only the models root is the runtime's concern, and the
	// agent's directory still has to hold the agent's state.
	return !doctorRunveilKinds[kind] || (write && kind == doctorKindAgent)
}

// doctorPathUsage is the paths the node reports for capacity plus the agent's
// own directory, which the runtime executes from and the doctor checks too.
func (n *NodeRuntime) doctorPathUsage() []proto.PathUsage {
	paths := n.pathUsage()
	if n.stateDir == "" {
		return paths
	}
	return append(paths, statAgentOwnedPath(doctorKindAgent, n.stateDir))
}

// doctorMountTable reads the mount table; doctorResolvePath follows symlinks
// of a path (or of its nearest existing ancestor); doctorContainerMarker
// names the file that shows the node itself runs in a container, "" when it
// does not. They are package vars so a test can present the mount tables of
// machines it does not run on.
var (
	doctorMountTable  = func() ([]byte, error) { return os.ReadFile("/proc/self/mountinfo") }
	doctorResolvePath = func(path string) string {
		rest := ""
		for probe := path; ; probe = filepath.Dir(probe) {
			if resolved, err := filepath.EvalSymlinks(probe); err == nil {
				return filepath.Join(resolved, rest)
			}
			if parent := filepath.Dir(probe); parent == probe {
				return path
			}
			rest = filepath.Join(filepath.Base(probe), rest)
		}
	}
	doctorContainerMarker = func() string {
		for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
			if _, err := os.Stat(marker); err == nil {
				return marker
			}
		}
		return ""
	}
)

// mountEntry is one line of /proc/self/mountinfo.
type mountEntry struct {
	point, fsType, source string
	mountOpts, superOpts  []string
}

// parseMountInfo reads mountinfo lines:
//
//	36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
//
// Fields before the lone "-" are id, parent, device, root, mount point, mount
// options and optional tagged fields; after it come the filesystem type, the
// source and the superblock options. Lines that do not fit are skipped.
func parseMountInfo(data []byte) []mountEntry {
	var entries []mountEntry
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		sep := -1
		for i, f := range fields {
			if f == "-" {
				sep = i
				break
			}
		}
		if sep < 6 || len(fields) < sep+3 {
			continue
		}
		entry := mountEntry{
			point:     unescapeMountField(fields[4]),
			mountOpts: strings.Split(fields[5], ","),
			fsType:    fields[sep+1],
			source:    unescapeMountField(fields[sep+2]),
		}
		if len(fields) > sep+3 {
			entry.superOpts = strings.Split(fields[sep+3], ",")
		}
		entries = append(entries, entry)
	}
	return entries
}

// unescapeMountField undoes the kernel's octal escaping of spaces, tabs,
// newlines and backslashes in mountinfo fields (\040 and so on).
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

// mountHolding is the mount a path lives on: the entry with the longest mount
// point that contains it, the later one when two are stacked on one point.
func mountHolding(entries []mountEntry, path string) (mountEntry, bool) {
	var best mountEntry
	found := false
	for _, e := range entries {
		contains := e.point == path || e.point == "/" || strings.HasPrefix(path, strings.TrimSuffix(e.point, "/")+"/")
		if contains && (!found || len(e.point) >= len(best.point)) {
			best, found = e, true
		}
	}
	return best, found
}

// networkFSTypes are filesystems whose data lives on another machine.
var networkFSTypes = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "smb3": true, "smbfs": true, "ceph": true,
	"glusterfs": true, "lustre": true, "gpfs": true, "beegfs": true, "9p": true, "afs": true,
	"fuse.sshfs": true, "fuse.s3fs": true, "fuse.juicefs": true, "fuse.glusterfs": true,
	"fuse.ceph": true, "fuse.cephfs": true, "fuse.rclone": true, "fuse.gcsfuse": true,
	"fuse.goofys": true, "fuse.mountpoint-s3": true,
}

func hasOption(options []string, want string) bool {
	for _, o := range options {
		if o == want {
			return true
		}
	}
	return false
}

// describeMount turns a mountinfo entry into the report's form. container is
// whether the node runs in a container, which is what makes an overlay mount
// the container's own, disposable, layer.
func describeMount(e mountEntry, container bool) *proto.DoctorMount {
	var options []string
	seen := map[string]bool{}
	for _, o := range append(append([]string{}, e.mountOpts...), e.superOpts...) {
		if o != "" && !seen[o] {
			seen[o] = true
			options = append(options, o)
		}
	}
	return &proto.DoctorMount{
		Point: e.point, FSType: e.fsType, Source: e.source, Options: strings.Join(options, ","),
		Network:   networkFSTypes[e.fsType],
		NoExec:    hasOption(e.mountOpts, "noexec"),
		ReadOnly:  hasOption(e.mountOpts, "ro") || hasOption(e.superOpts, "ro"),
		Ephemeral: container && e.fsType == "overlay",
	}
}

// doctorFilesystems annotates every listed path with its mount and reports
// what the mount makes impossible or fragile. A network filesystem is stated
// as a fact only: shared NFS storage is a supported setup.
func doctorFilesystems(r *proto.DoctorReport, runtimeName string) {
	if doctorOnMac {
		// macOS has no mount table of this form, and the mount checks
		// judge Linux mount options: they do not apply.
		return
	}
	data, err := doctorMountTable()
	if err != nil {
		// No path's mount can be judged: say so, or the report reads as if
		// every path were fine.
		r.Findings = append(r.Findings, proto.DoctorFinding{
			ID: proto.DoctorCheckMounts, Status: proto.CheckSkip, Code: proto.DoctorSkipMountTableUnreadable,
			Probe: "/proc/self/mountinfo", Evidence: []proto.DoctorField{{Name: "error", Value: err.Error()}},
			Recheck: "findmnt",
		})
		return
	}
	entries := parseMountInfo(data)
	marker := doctorContainerMarker()
	for i := range r.Host.Disks {
		disk := &r.Host.Disks[i]
		if entry, ok := mountHolding(entries, doctorResolvePath(disk.Path)); ok {
			disk.Mount = describeMount(entry, marker != "")
		}
	}

	type check struct {
		id, code string
		kinds    map[string]bool
		write    bool
		flagged  func(*proto.DoctorMount) bool
	}
	for _, c := range []check{
		{proto.DoctorCheckPathExec, proto.DoctorPathNoExec, doctorExecKinds, false, func(m *proto.DoctorMount) bool { return m.NoExec }},
		{proto.DoctorCheckPathWrite, proto.DoctorPathReadOnly, doctorWriteKinds, true, func(m *proto.DoctorMount) bool { return m.ReadOnly }},
		{proto.DoctorCheckPathPersist, proto.DoctorPathEphemeral, doctorPersistedKinds, true, func(m *proto.DoctorMount) bool { return m.Ephemeral }},
	} {
		status := proto.CheckFail
		if c.code == proto.DoctorPathEphemeral {
			status = proto.CheckWarn
		}
		var order []string
		groups := map[string][]proto.DoctorDisk{}
		for _, disk := range r.Host.Disks {
			if disk.Mount == nil || !doctorJudged(c.kinds, disk.Kind, runtimeName, c.write) || !c.flagged(disk.Mount) {
				continue
			}
			if groups[disk.Mount.Point] == nil {
				order = append(order, disk.Mount.Point)
			}
			groups[disk.Mount.Point] = append(groups[disk.Mount.Point], disk)
		}
		for _, point := range order {
			disks := groups[point]
			first := disks[0]
			m := first.Mount
			finding := proto.DoctorFinding{
				ID: c.id, Status: status, Code: c.code, Disk: &first, Probe: "/proc/self/mountinfo",
				Evidence: []proto.DoctorField{{Name: "TARGET", Value: m.Point}, {Name: "FSTYPE", Value: m.FSType}, {Name: "OPTIONS", Value: m.Options}},
				Recheck:  "findmnt -T " + first.Path,
			}
			if c.code == proto.DoctorPathEphemeral {
				finding.Evidence = append(finding.Evidence, proto.DoctorField{Name: "container", Value: marker})
			}
			if len(disks) > 1 {
				paths := make([]string, len(disks))
				for i, d := range disks {
					paths[i] = d.Path
				}
				finding.Evidence = append(finding.Evidence, proto.DoctorField{Name: "paths", Value: strings.Join(paths, ", ")})
			}
			r.Findings = append(r.Findings, finding)
		}
	}

	var points []string
	byPoint := map[string][]proto.DoctorDisk{}
	for _, disk := range r.Host.Disks {
		if disk.Mount != nil && disk.Mount.Network {
			if byPoint[disk.Mount.Point] == nil {
				points = append(points, disk.Mount.Point)
			}
			byPoint[disk.Mount.Point] = append(byPoint[disk.Mount.Point], disk)
		}
	}
	for _, point := range points {
		disks := byPoint[point]
		m := disks[0].Mount
		holds := make([]string, len(disks))
		for i, d := range disks {
			holds[i] = doctorKindLabel(d.Kind) + " " + d.Path
		}
		value := point + " is " + m.FSType
		if m.Source != "" {
			value += " (" + m.Source + ")"
		}
		r.Facts = append(r.Facts, proto.DoctorFact{Code: proto.DoctorFactNetworkFilesystem, Value: value + "; holds " + strings.Join(holds, ", "), Disks: disks})
	}
}

// doctorKindLabel names a path kind in report text: the runveil store by the
// name operators know the runtime by.
func doctorKindLabel(kind string) string {
	if kind == doctorKindRunveil {
		return udockerdrv.LauncherName()
	}
	return kind
}
