package provision

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// dirScenario describes the host the probe runs on. The probe script runs for
// real under sh; what is faked are the host facts it reads - /proc/mounts, the
// id/stat/df/chown/sudo/findmnt commands - with output copied from the real
// tools, so each verdict is derived from text a real host prints.
type dirScenario struct {
	mounts    string // /proc/mounts content; %DIR% is the directory under test
	uid, user string
	statfs    string // `stat -f -c %T`
	totalKiB  int64
	availKiB  int64
	dfSource  string // first column of `df -Pk`
	chownErr  bool   // chown answers EPERM the way RunPod's NFS does
	colonErr  bool   // a ':' in a file name is refused the way RunPod's NFS does
	sudo      bool   // `sudo -n true` succeeds
	findmnt   string // when set, findmnt exists and prints this line
	container bool   // a /.dockerenv-style marker file exists
	docker    bool   // docker is installed and its daemon answers `docker info`
	// modes maps a directory to what `stat -c '%a %u %g'` prints for it
	// ("700 0 0"); every other directory reads as "755 0 0". runtimeID is the
	// uid and gid of the dedicated udocker account, "" while it does not exist.
	modes     map[string]string
	runtimeID string
	// nerdctl: containerd answers `nerdctl -n gpuworks ps`. useradd: the tool
	// that creates the udocker account is on PATH. arch is `uname -m` ("" =
	// this host's).
	nerdctl, useradd bool
	arch             string
	// groups is what `id -G udocker` prints ("1001 44 109": primary group
	// first); "" = just the primary group. aclDir is a directory `ls -ld`
	// shows with the trailing + of an ACL.
	groups, aclDir string
	// macOS: no /proc/mounts; BSD stat, and `mount` prints macMount. dfMount
	// is the "Mounted on" column df prints.
	mac      bool
	macMount string
	dfMount  string
}

const (
	runpodNFS4Line  = "ca-mtl-1-vast.runpod.net:/runpodfs/3b1f9c0e %DIR% nfs4 rw,relatime,vers=4.1,rsize=1048576,wsize=1048576,namlen=255,hard,proto=tcp,timeo=600,retrans=2,sec=sys,clientaddr=100.65.12.4,local_lock=none,addr=100.64.0.21 0 0"
	overlayRootLine = "overlay / overlay rw,relatime,lowerdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/812/fs,upperdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/1204/fs,workdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/1204/work 0 0"
	procLine        = "proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0"
	tmpfsDevLine    = "tmpfs /dev tmpfs rw,nosuid,size=65536k,mode=755 0 0"
)

func (sc dirScenario) withDefaults() dirScenario {
	if sc.uid == "" {
		sc.uid, sc.user = "0", "root"
	}
	if sc.user == "" {
		sc.user = "user" + sc.uid
	}
	if sc.statfs == "" {
		sc.statfs = "ext2/ext3"
	}
	if sc.totalKiB == 0 {
		sc.totalKiB = 1 << 30 // 1 TiB
		sc.availKiB = 900 << 20
	}
	if sc.dfSource == "" {
		sc.dfSource = "/dev/sda1"
	}
	return sc
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// scenarioHandler serves SSH exec requests by running the command with the
// scenario's fake tools first on PATH. dir is the directory the scenario's
// mount table refers to.
func scenarioHandler(t *testing.T, sc dirScenario, dir string) fakeSSHHandler {
	t.Helper()
	sc = sc.withDefaults()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// PATH holds only the fakes and the plain utilities the scripts use, so a
	// host with a real findmnt behaves as one without unless the scenario
	// supplies it.
	for _, tool := range []string{"sh", "awk", "dirname", "grep", "head", "tr", "rm", "uname", "cat", "test", "true", "mktemp"} {
		if real, err := exec.LookPath(tool); err == nil {
			if err := os.Symlink(real, filepath.Join(bin, tool)); err != nil {
				t.Fatal(err)
			}
		}
	}
	mountsFile := filepath.Join(root, "mounts")
	if !sc.mac {
		if err := os.WriteFile(mountsFile, []byte(strings.ReplaceAll(sc.mounts, "%DIR%", dir)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(root, "no-container-marker")
	if sc.container {
		marker = filepath.Join(root, "dockerenv")
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// id for the dedicated account prints its ids when it exists and fails the
	// way GNU id does when it does not.
	writeExecutable(t, filepath.Join(bin, "id"), `case "$*" in
  -u) echo "$FAKE_UID" ;;
  -un) echo "$FAKE_USER" ;;
  "-u udocker"|"-g udocker")
    if [ -z "$FAKE_RUNTIME_ID" ]; then echo "id: 'udocker': no such user" >&2; exit 1; fi
    echo "$FAKE_RUNTIME_ID" ;;
  "-G udocker")
    if [ -z "$FAKE_RUNTIME_ID" ]; then echo "id: 'udocker': no such user" >&2; exit 1; fi
    echo "${FAKE_RUNTIME_GROUPS:-$FAKE_RUNTIME_ID}" ;;
  *) exit 1 ;;
esac
`)
	if sc.mac {
		// macOS stat reads -f as a format: it prints "-c", then fails on the
		// remaining arguments as files (recorded on an M4 Max, macOS 26.5).
		writeExecutable(t, filepath.Join(bin, "stat"), `echo "-c"
echo "stat: %T: stat: No such file or directory" >&2
exit 1
`)
		writeExecutable(t, filepath.Join(bin, "mount"), `printf '%s\n' "$FAKE_MAC_MOUNT"
`)
	} else {
		// GNU stat: -f -c %T is the filesystem type; -c '%a %u %g' prints
		// "<octal mode> <uid> <gid>", e.g. "700 0 0" for a private home. A
		// path the scenario does not list is a stock 0755 root directory.
		writeExecutable(t, filepath.Join(bin, "stat"), `if [ "$1 $2 $3" = "-f -c %T" ]; then
  echo "$FAKE_STATFS"
  exit 0
fi
if [ "$1" = -c ] && [ "$2" = '%a %u %g' ]; then
  shift 2
  [ "$1" = -- ] && shift
  while read -r path mode uid gid; do
    if [ "$path" = "$1" ]; then
      if [ "$mode" = unreadable ]; then echo "stat: cannot statx '$1': Permission denied" >&2; exit 1; fi
      echo "$mode $uid $gid"
      exit 0
    fi
  done < "$FAKE_MODES"
  echo "755 0 0"
  exit 0
fi
exit 1
`)
		var modes strings.Builder
		for path, perm := range sc.modes {
			modes.WriteString(path + " " + perm + "\n")
		}
		if err := os.WriteFile(filepath.Join(root, "modes"), []byte(modes.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, filepath.Join(bin, "df"), `[ "$1" = "-Pk" ] || exit 1
echo "Filesystem     1024-blocks      Used Available Capacity Mounted on"
echo "$FAKE_DF_SOURCE $FAKE_TOTAL $((FAKE_TOTAL - FAKE_AVAIL)) $FAKE_AVAIL 10% ${FAKE_DF_MOUNT:-$2}"
`)
	writeExecutable(t, filepath.Join(bin, "chown"), `if [ -n "$FAKE_CHOWN_ERR" ]; then
  echo "chown: changing ownership of '$2': Operation not permitted" >&2
  exit 1
fi
exit 0
`)
	// RunPod's VAST-backed NFS refuses ':' in a file name; GNU touch says so
	// this way (seen on the pod: touch: cannot touch 'x:y': Invalid argument).
	writeExecutable(t, filepath.Join(bin, "touch"), `[ "$1" = -- ] && shift
case "$1" in
  *:*) if [ -n "$FAKE_COLON_ERR" ]; then
         echo "touch: cannot touch '$1': Invalid argument" >&2
         exit 1
       fi ;;
esac
: > "$1"
`)
	// sudo -n without a cached password or NOPASSWD rule fails like this.
	writeExecutable(t, filepath.Join(bin, "sudo"), `if [ "$FAKE_SUDO" = yes ]; then
  [ "$1" = -n ] && shift
  exec "$@"
fi
echo "sudo: a password is required" >&2
exit 1
`)
	if sc.findmnt != "" {
		writeExecutable(t, filepath.Join(bin, "findmnt"), `echo "$FAKE_FINDMNT"
`)
	}
	chownErr, colonErr, sudo := "", "", "no"
	if sc.chownErr {
		chownErr = "1"
	}
	if sc.colonErr {
		colonErr = "1"
	}
	if sc.sudo {
		sudo = "yes"
	}
	env := []string{
		"PATH=" + bin,
		"HOME=" + root,
		"FAKE_UID=" + sc.uid,
		"FAKE_USER=" + sc.user,
		"FAKE_STATFS=" + sc.statfs,
		"FAKE_MODES=" + filepath.Join(root, "modes"),
		"FAKE_RUNTIME_ID=" + sc.runtimeID,
		"FAKE_RUNTIME_GROUPS=" + sc.groups,
		"FAKE_ACL_DIR=" + sc.aclDir,
		"FAKE_TOTAL=" + strconv.FormatInt(sc.totalKiB, 10),
		"FAKE_AVAIL=" + strconv.FormatInt(sc.availKiB, 10),
		"FAKE_DF_SOURCE=" + sc.dfSource,
		"FAKE_CHOWN_ERR=" + chownErr,
		"FAKE_COLON_ERR=" + colonErr,
		"FAKE_SUDO=" + sudo,
		"FAKE_FINDMNT=" + strings.ReplaceAll(sc.findmnt, "%DIR%", dir),
		"FAKE_MAC_MOUNT=" + strings.ReplaceAll(sc.macMount, "%DIR%", dir),
		"FAKE_DF_MOUNT=" + strings.ReplaceAll(sc.dfMount, "%DIR%", dir),
	}
	if sc.nerdctl {
		writeExecutable(t, filepath.Join(bin, "nerdctl"), `[ "$1 $2 $3" = "-n gpuworks ps" ] || exit 1
echo "CONTAINER ID    IMAGE    COMMAND    CREATED    STATUS    PORTS    NAMES"
`)
	}
	if sc.useradd {
		writeExecutable(t, filepath.Join(bin, "useradd"), "exit 0\n")
	}
	// GNU ls -ld marks a directory with an ACL by a + after the mode string.
	writeExecutable(t, filepath.Join(bin, "ls"), `[ "$1" = -ld ] || exit 1
shift
[ "$1" = -- ] && shift
plus=""
[ "$1" = "$FAKE_ACL_DIR" ] && plus="+"
echo "drwx------$plus 5 root root 4096 Mar  3 10:11 $1"
`)
	if sc.arch != "" {
		// uname is a link to the host's tool; replace the link, not its target.
		_ = os.Remove(filepath.Join(bin, "uname"))
		writeExecutable(t, filepath.Join(bin, "uname"), `[ "$1" = -m ] || exit 1
echo "`+sc.arch+`"
`)
	}
	if sc.docker {
		writeExecutable(t, filepath.Join(bin, "docker"), `[ "$1" = info ] || exit 1
echo "Server Version: 27.3.1"
`)
	}
	return func(cmd string, _ io.Reader) (string, string, uint32) {
		// The two lines that name host files are the probe's only seams to
		// the real /proc/mounts and /.dockerenv.
		for from, to := range map[string]string{
			"mounts=/proc/mounts":                             "mounts=" + mountsFile,
			`containerfiles="/.dockerenv /run/.containerenv"`: `containerfiles="` + marker + `"`,
		} {
			if strings.Contains(cmd, "probed=%s") && !strings.Contains(cmd, from) {
				// The script changed and the seam silently stopped holding:
				// the probe would read this host's own files.
				t.Errorf("probe command lacks the seam %q", from)
			}
			cmd = strings.Replace(cmd, from, to, 1)
		}
		c := exec.Command("sh", "-c", cmd)
		c.Env = env
		var stdout, stderr strings.Builder
		c.Stdout, c.Stderr = &stdout, &stderr
		status := uint32(0)
		if err := c.Run(); err != nil {
			status = 1
		}
		return stdout.String(), stderr.String(), status
	}
}

// realTempDir is t.TempDir with symlinks resolved. The probe reports the
// directory `pwd -P` prints, and a mount table names real paths; on macOS the
// temp root /var is a link to /private/var.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// unwritableDir is an existing directory in which nobody on this host can
// create a file: /proc on Linux, even for root. Elsewhere a read-only
// directory serves as long as the test does not run as root.
func unwritableDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "linux" {
		return "/proc"
	}
	if os.Geteuid() == 0 {
		t.Skip("no directory refuses root on this host")
	}
	dir := realTempDir(t)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	return dir
}

func probeScenario(t *testing.T, sc dirScenario, dir, requested, kind string) InstallDirOutcome {
	t.Helper()
	return probeScenarioRuntime(t, sc, dir, requested, kind, "")
}

// probeScenarioRuntime is probeScenario for an install that names its runtime.
func probeScenarioRuntime(t *testing.T, sc dirScenario, dir, requested, kind, runtimeDriver string) InstallDirOutcome {
	t.Helper()
	handler := scenarioHandler(t, sc, dir)
	host, port := startFakeSSHServer(t, "root", "pw", handler)
	client := dialFakeSSH(t, host, port, "root", "pw")
	return ProbeInstallDirForRuntime(t.Context(), client, requested, kind, "", runtimeDriver)
}

func checkByName(t *testing.T, checks []CheckResult, name string) CheckResult {
	t.Helper()
	var found []CheckResult
	for _, c := range checks {
		if c.ID == name {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one %q check, got %d in %+v", name, len(found), checks)
	}
	return found[0]
}

func wantCheck(t *testing.T, checks []CheckResult, name, status string, details ...string) {
	t.Helper()
	c := checkByName(t, checks, name)
	if c.Status != status {
		t.Fatalf("%s status = %s, want %s: %s", name, c.Status, status, c.text())
	}
	// A check that is not a plain pass says why with a code (ADR 0067).
	if c.Status != "pass" && c.Code == "" {
		t.Fatalf("%s %s check has no code: %+v", name, c.Status, c)
	}
	for _, d := range details {
		if !strings.Contains(c.text(), d) {
			t.Fatalf("%s detail missing %q: %s", name, d, c.text())
		}
	}
}

// The RunPod pod from the field: / is the container overlay, /workspace is
// NFSv4 and refuses chown even for root.
func TestInstallDirProbeRunPodWorkspaceNFS(t *testing.T) {
	dir := realTempDir(t)
	out := probeScenario(t, dirScenario{
		mounts:   overlayRootLine + "\n" + procLine + "\n" + tmpfsDevLine + "\n" + runpodNFS4Line,
		statfs:   "nfs",
		dfSource: "ca-mtl-1-vast.runpod.net:/runpodfs/3b1f9c0e",
		chownErr: true,
		colonErr: true,
	}, dir, dir, "container")
	if out.Noexec {
		t.Fatal("rw nfs4 mount was reported noexec")
	}
	wantCheck(t, out.Checks, CheckIDInstallDir, "pass", dir, "filesystem nfs4", "mounted at "+dir, "900.0 GiB free", "writable by root")
	if d := checkByName(t, out.Checks, CheckIDInstallDir).text(); strings.Contains(d, "vers=4.1") || strings.Contains(d, "rsize") {
		t.Fatalf("mount options leaked into the install directory row: %s", d)
	}
	wantCheck(t, out.Checks, CheckIDInstallDirOwnership, "warn",
		"runveil workloads will run as the agent's own user (root) instead of the dedicated runtime user",
		"chown: changing ownership of '"+dir+"/.gpuworks-probe.", "/owner': Operation not permitted")
	wantCheck(t, out.Checks, CheckIDInstallDirFilesystem, "warn", "nfs4", "model loading")
	wantCheck(t, out.Checks, CheckIDInstallDirSpace, "pass", "900.0 GiB free of 1024.0 GiB")
	// A container on a mounted volume is the persistent case, not a warning.
	wantCheck(t, out.Checks, CheckIDInstallDirPersistence, "pass")
	wantCheck(t, out.Checks, CheckIDInstallDirNames, "fail",
		"does not allow ':' in file names", "dpkg/info", "cannot run deployments from here", "choose an install directory on a local filesystem",
		"touch: cannot touch '"+dir+"/.gpuworks-probe.", "/name:probe': Invalid argument")
	if len(out.Checks) != 7 {
		t.Fatalf("checks = %+v", out.Checks)
	}
	// Root and the dedicated user would otherwise be judged on a chown that
	// workloads never get: they run as root, so no ancestor matters.
	wantCheck(t, out.Checks, CheckIDInstallDirTraversal, "na", "not needed", "agent's own user")
}

func TestInstallDirProbeLeavesNothingBehind(t *testing.T) {
	dir := realTempDir(t)
	probeScenario(t, dirScenario{mounts: "/dev/sda1 %DIR% ext4 rw,relatime 0 0", statfs: "ext2/ext3"}, dir, dir, "vm")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("probe left %v (err %v) in the install directory", entries, err)
	}
}

func TestInstallDirProbeLocalDiskIsAllOK(t *testing.T) {
	dir := realTempDir(t)
	out := probeScenario(t, dirScenario{mounts: overlayRootLine + "\n/dev/nvme0n1p2 %DIR% ext4 rw,relatime 0 0"}, dir, dir, "vm")
	for _, c := range out.Checks {
		if c.Status != "pass" {
			t.Fatalf("%s = %s: %s", c.ID, c.Status, c.text())
		}
	}
	wantCheck(t, out.Checks, CheckIDInstallDirOwnership, "pass", "dedicated runtime user")
	wantCheck(t, out.Checks, CheckIDInstallDirFilesystem, "pass", "ext4 (local)")
	wantCheck(t, out.Checks, CheckIDInstallDirNames, "pass", "':' is allowed in file names")
}

func TestInstallDirProbeNoexecMountBlocks(t *testing.T) {
	dir := realTempDir(t)
	out := probeScenario(t, dirScenario{mounts: "/dev/sdb1 %DIR% ext4 rw,nosuid,nodev,noexec,relatime 0 0"}, dir, dir, "vm")
	if !out.Noexec {
		t.Fatal("noexec mount not detected")
	}
	wantCheck(t, out.Checks, CheckIDInstallDir, "fail", "mounted noexec", "agent binary", "PRoot", "mounted at "+dir)
	err := noexecInstallError(out)
	if err == nil || !strings.Contains(err.Error(), "noexec") || !strings.Contains(err.Error(), "install directory") {
		t.Fatalf("install error = %v", err)
	}
}

// A mount option that merely contains the word must not count.
func TestInstallDirProbeNoexecIsOneWholeOption(t *testing.T) {
	dir := realTempDir(t)
	out := probeScenario(t, dirScenario{mounts: "/dev/sdb1 %DIR% ext4 rw,relatime,x-noexec-note 0 0"}, dir, dir, "vm")
	if out.Noexec {
		t.Fatal("x-noexec-note treated as noexec")
	}
}

func TestInstallDirProbeOverlayInContainerWarnsAboutPersistence(t *testing.T) {
	dir := realTempDir(t)
	sc := dirScenario{mounts: overlayRootLine + "\n" + procLine, statfs: "overlayfs", dfSource: "overlay", totalKiB: 100 << 20, availKiB: 20 << 20}
	out := probeScenario(t, sc, dir, dir, "container")
	wantCheck(t, out.Checks, CheckIDInstallDirPersistence, "warn", "container's writable layer", "lost when the instance is recreated", "/workspace")
	wantCheck(t, out.Checks, CheckIDInstallDir, "pass", "filesystem overlay", "mounted at /")
	wantCheck(t, out.Checks, CheckIDInstallDirSpace, "warn", "20.0 GiB free of 100.0 GiB", "50.0 GiB")

	// The marker file the container runtime drops is enough without a hint.
	sc.container = true
	out = probeScenario(t, sc, dir, dir, "")
	wantCheck(t, out.Checks, CheckIDInstallDirPersistence, "warn", "container's writable layer")

	// The same overlay on a host that is not a container is nothing to warn about.
	sc.container = false
	out = probeScenario(t, sc, dir, dir, "vm")
	wantCheck(t, out.Checks, CheckIDInstallDirPersistence, "pass")
}

// A cloud provider's one-click flow fixes the install directory: the overlay
// still warns, since the agent is lost with the instance, but the detail says
// so instead of advising a persistent volume the flow cannot use.
func TestInstallDirProbeProviderContainerOverlayWarnsWithoutAdvice(t *testing.T) {
	dir := t.TempDir()
	sc := dirScenario{mounts: overlayRootLine + "\n" + procLine, statfs: "overlayfs", dfSource: "overlay", totalKiB: 100 << 20, availKiB: 80 << 20}
	host, port := startFakeSSHServer(t, "root", "pw", scenarioHandler(t, sc, dir))
	client := dialFakeSSH(t, host, port, "root", "pw")
	out := ProbeInstallDir(t.Context(), client, dir, "container", "runpod")
	wantCheck(t, out.Checks, CheckIDInstallDirPersistence, "warn", "container disk", "runpod", "deleted or recreated", "install the agent again")
	for _, c := range out.Checks {
		if c.ID == CheckIDInstallDirPersistence && strings.Contains(c.text(), "persistent volume") {
			t.Fatalf("one-click detail must not advise a persistent volume: %s", c.text())
		}
	}
}

// The overlay mount of a layered image lists every layer in its options; the
// row names the filesystem and the mount point only.
func TestInstallDirProbeDoesNotDumpMountOptions(t *testing.T) {
	dir := t.TempDir()
	out := probeScenario(t, dirScenario{mounts: overlayRootLine + "\n" + procLine, statfs: "overlayfs", dfSource: "overlay"}, dir, dir, "container")
	d := checkByName(t, out.Checks, CheckIDInstallDir).text()
	for _, leaked := range []string{"lowerdir", "upperdir", "workdir", "relatime"} {
		if strings.Contains(d, leaked) {
			t.Fatalf("mount option %q leaked into %q", leaked, d)
		}
	}
	wantCheck(t, out.Checks, CheckIDInstallDir, "pass", "filesystem overlay, mounted at /")
}

func TestInstallDirProbeMemoryBackedDirectoryWarns(t *testing.T) {
	dir := realTempDir(t)
	out := probeScenario(t, dirScenario{mounts: "tmpfs %DIR% tmpfs rw,nosuid,nodev 0 0", statfs: "tmpfs"}, dir, dir, "vm")
	wantCheck(t, out.Checks, CheckIDInstallDirPersistence, "warn", "tmpfs", "lost on reboot")
}

func TestInstallDirProbeNetworkFilesystems(t *testing.T) {
	for _, tc := range []struct {
		fstype, source string
		network        bool
	}{
		{"nfs", "10.0.0.2:/export", true},
		{"cifs", "//nas/share", true},
		{"fuse.sshfs", "u@host:/data", true},
		{"fuse.juicefs", "JuiceFS:vol", true},
		{"ceph", "10.0.0.3:6789:/", true},
		{"fuse.overlayfs", "fuse-overlayfs", false},
		{"xfs", "/dev/sdc1", false},
	} {
		dir := realTempDir(t)
		out := probeScenario(t, dirScenario{mounts: tc.source + " %DIR% " + tc.fstype + " rw,relatime 0 0"}, dir, dir, "vm")
		c := checkByName(t, out.Checks, CheckIDInstallDirFilesystem)
		if (c.Status == "warn") != tc.network {
			t.Fatalf("%s: status %s, network=%v: %s", tc.fstype, c.Status, tc.network, c.text())
		}
	}
}

// Not root and no sudo: the directory cannot be written. /proc refuses file
// creation for every uid, so it stands in for a directory the SSH user cannot
// write regardless of who runs the test.
func TestInstallDirProbeNotRootNoSudo(t *testing.T) {
	unwritable := unwritableDir(t)
	requested := filepath.Join(unwritable, "gpuworks")
	out := probeScenario(t, dirScenario{uid: "1000", user: "ubuntu", mounts: overlayRootLine + "\n" + procLine}, realTempDir(t), requested, "vm")
	wantCheck(t, out.Checks, CheckIDInstallDir, "fail", unwritable+" is not writable by ubuntu", "no passwordless sudo", "/tmp/gpuworks")
	wantCheck(t, out.Checks, CheckIDInstallDirOwnership, "na", "not checked", "ubuntu is not root")
}

func TestInstallDirProbeSudoThatCannotWriteEither(t *testing.T) {
	out := probeScenario(t, dirScenario{uid: "1000", user: "ubuntu", sudo: true, mounts: overlayRootLine + "\n" + procLine}, realTempDir(t), filepath.Join(unwritableDir(t), "gpuworks"), "vm")
	wantCheck(t, out.Checks, CheckIDInstallDir, "fail", "even through sudo")
	wantCheck(t, out.Checks, CheckIDInstallDirOwnership, "skip", "not checked", "no file could be created")
}

// Non-root with passwordless sudo: the chown test runs through sudo, because
// that is the identity the agent runs as.
func TestInstallDirProbeSudoRunsTheOwnershipTest(t *testing.T) {
	dir := realTempDir(t)
	out := probeScenario(t, dirScenario{uid: "1000", user: "ubuntu", sudo: true, chownErr: true, mounts: "10.0.0.2:/export %DIR% nfs4 rw,relatime 0 0"}, dir, dir, "vm")
	wantCheck(t, out.Checks, CheckIDInstallDir, "pass", "writable by ubuntu")
	wantCheck(t, out.Checks, CheckIDInstallDirOwnership, "warn", "Operation not permitted")
}

func TestInstallDirProbeMissingDirectoryUsesNearestExistingAncestor(t *testing.T) {
	parent := realTempDir(t)
	requested := filepath.Join(parent, "models", "gpuworks")
	out := probeScenario(t, dirScenario{mounts: overlayRootLine + "\n/dev/sda1 %DIR% ext4 rw,relatime 0 0"}, parent, requested, "vm")
	wantCheck(t, out.Checks, CheckIDInstallDir, "pass", requested, "does not exist yet", "nearest existing parent "+parent, "mounted at "+parent)
	if _, err := os.Stat(filepath.Join(parent, "models")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the precheck created a directory: %v", err)
	}
}

func TestInstallDirProbeRegularFileIsNotADirectory(t *testing.T) {
	dir := realTempDir(t)
	file := filepath.Join(dir, "gpuworks")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := probeScenario(t, dirScenario{mounts: "/dev/sda1 %DIR% ext4 rw,relatime 0 0"}, dir, file, "vm")
	wantCheck(t, out.Checks, CheckIDInstallDir, "fail", file+" exists but is not a directory")
}

// findmnt, when the host has it, is preferred over /proc/mounts.
func TestInstallDirProbePrefersFindmnt(t *testing.T) {
	dir := realTempDir(t)
	out := probeScenario(t, dirScenario{
		mounts:  "/dev/sda1 %DIR% ext4 rw,relatime 0 0",
		findmnt: "/mnt/vol one nfs4 rw,noexec,relatime,vers=4.2",
	}, dir, dir, "vm")
	if !out.Noexec {
		t.Fatal("findmnt options ignored")
	}
	wantCheck(t, out.Checks, CheckIDInstallDir, "fail", "mounted at /mnt/vol one", "filesystem nfs4")
}

func TestInstallDirProbeLongestMountPrefixWins(t *testing.T) {
	dir := realTempDir(t)
	mounts := "/dev/sda1 / ext4 rw,relatime 0 0\n/dev/sdb1 " + filepath.Dir(dir) + " xfs rw,relatime 0 0\n/dev/sdc1 " + dir + " ext4 rw,noexec 0 0\n/dev/sdd1 " + dir + "-sibling ext4 rw 0 0"
	out := probeScenario(t, dirScenario{mounts: mounts}, dir, dir, "vm")
	if !out.Noexec {
		t.Fatalf("most specific mount not chosen: %+v", out.Checks)
	}
}

func TestInstallDirProbeFailureIsASkipNotAVerdict(t *testing.T) {
	host, port := startFakeSSHServer(t, "root", "pw", func(string, io.Reader) (string, string, uint32) {
		return "", "sh: 1: cannot create /dev/null: Permission denied\n", 2
	})
	client := dialFakeSSH(t, host, port, "root", "pw")
	out := ProbeInstallDir(t.Context(), client, "/opt/gpuworks", "vm", "")
	if out.Noexec || len(out.Checks) != 1 {
		t.Fatalf("outcome = %+v", out)
	}
	wantCheck(t, out.Checks, CheckIDInstallDir, "skip", "could not inspect /opt/gpuworks")

	// Output from a login banner but no probe facts is not a verdict either.
	host, port = startFakeSSHServer(t, "root", "pw", func(string, io.Reader) (string, string, uint32) {
		return "Welcome to Ubuntu\n", "", 0
	})
	client = dialFakeSSH(t, host, port, "root", "pw")
	out = ProbeInstallDir(t.Context(), client, "/opt/gpuworks", "vm", "")
	wantCheck(t, out.Checks, CheckIDInstallDir, "skip", "incomplete probe output")
}

func TestInstallDirProbeTimesOutOnAHungMount(t *testing.T) {
	saved := installDirProbeTimeout
	installDirProbeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { installDirProbeTimeout = saved })
	host, port := startFakeSSHServer(t, "root", "pw", func(string, io.Reader) (string, string, uint32) {
		time.Sleep(3 * time.Second) // stat on a hung NFS mount
		return "", "", 0
	})
	client := dialFakeSSH(t, host, port, "root", "pw")
	start := time.Now()
	out := ProbeInstallDir(t.Context(), client, "/workspace", "container", "")
	if time.Since(start) > 2*time.Second {
		t.Fatalf("probe waited %v on a hung host", time.Since(start))
	}
	wantCheck(t, out.Checks, CheckIDInstallDir, "skip", "could not inspect /workspace", "deadline")
}

// With the host's real id/stat/df/chown and only the two host-file seams
// redirected, the script must still run to completion and report every row.
func TestInstallDirProbeRunsWithTheHostsRealTools(t *testing.T) {
	dir := realTempDir(t)
	cmd := installDirProbeCommand(dir)
	cmd = strings.Replace(cmd, `containerfiles="/.dockerenv /run/.containerenv"`, `containerfiles="/nonexistent"`, 1)
	host, port := startFakeSSHServer(t, "root", "pw", func(c string, _ io.Reader) (string, string, uint32) {
		out, err := exec.Command("sh", "-c", c).CombinedOutput()
		if err != nil {
			return string(out), "", 1
		}
		return string(out), "", 0
	})
	client := dialFakeSSH(t, host, port, "root", "pw")
	out, err := runContext(t.Context(), client, cmd)
	if err != nil {
		t.Fatalf("probe failed: %v: %s", err, out)
	}
	facts, err := parseInstallDirFacts(out)
	if err != nil {
		t.Fatal(err)
	}
	if !facts.haveSpace || facts.availKiB <= 0 || facts.filesystem() == "" || !facts.writable {
		t.Fatalf("facts = %+v\n%s", facts, out)
	}
	checks, _ := installDirChecks(facts, "vm", "")
	if len(checks) != 7 {
		t.Fatalf("checks = %+v", checks)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("probe left %v", entries)
	}
}

func TestInstallDirAccessCheckSudoWritable(t *testing.T) {
	f := installDirFacts{requested: "/opt/gpuworks", probed: "/opt", exists: "no", uid: "1000", user: "ubuntu", sudo: true, privWritable: true, mount: "/", fstype: "ext4", options: "rw", availKiB: 80 << 20, haveSpace: true, chown: "ok"}
	c := installDirAccessCheck(f, false)
	if c.Status != "pass" || !strings.Contains(c.text(), "writable through passwordless sudo") {
		t.Fatalf("check = %+v", c)
	}
}

func TestParseInstallDirFactsRejectsOutputWithoutFacts(t *testing.T) {
	if _, err := parseInstallDirFacts("probed=/opt\n"); err == nil {
		t.Fatal("a truncated probe was accepted")
	}
}

func TestMergeChecksReplacesByName(t *testing.T) {
	got := mergeChecks(
		[]CheckResult{{ID: "ssh", Status: "pass"}, {ID: CheckIDInstallDir, Status: "fail", Message: "old"}},
		[]CheckResult{{ID: CheckIDInstallDir, Status: "pass", Message: "new"}, {ID: CheckIDInstallDirSpace, Status: "pass"}},
	)
	if len(got) != 3 || got[1].text() != "new" || got[2].ID != CheckIDInstallDirSpace {
		t.Fatalf("merged = %+v", got)
	}
}

func TestInstallDirProbeCommandRunsUnderShAndQuotesTheDirectory(t *testing.T) {
	cmd := installDirProbeCommand("/data/it's here")
	if !strings.HasPrefix(cmd, "sh -c '") {
		t.Fatalf("probe must not depend on the login shell: %.40s", cmd)
	}
	if !strings.Contains(installDirProbeScript("/data/it's here"), `dir='/data/it'\''s here'`) {
		t.Fatal("directory not shell quoted")
	}
}

// The precheck reports the install directory's rows beside its other checks,
// and an empty install_dir means the install default.
func TestPrecheckSSHReportsInstallDirectory(t *testing.T) {
	dir := realTempDir(t)
	handler := scenarioHandler(t, dirScenario{mounts: runpodNFS4Line, statfs: "nfs", chownErr: true}, dir)
	host, port := startFakeSSHServer(t, "root", "pw", handler)
	p := NewSSHPrechecker()
	report, err := p.PrecheckSSH(t.Context(), SSHPrecheckRequest{
		Target: "root@" + host, Port: port, InstanceKind: "container", InstallDir: dir + "/",
		SSHAuth: SSHAuth{Mode: AuthPassword, Password: "pw"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantCheck(t, report.Checks, CheckIDInstallDir, "pass", dir)
	wantCheck(t, report.Checks, CheckIDInstallDirOwnership, "warn", "Operation not permitted")
	wantCheck(t, report.Checks, "os_arch", "pass")

	report, err = p.PrecheckSSH(t.Context(), SSHPrecheckRequest{
		Target: "root@" + host, Port: port, SSHAuth: SSHAuth{Mode: AuthPassword, Password: "pw"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c := checkByName(t, report.Checks, CheckIDInstallDir); !strings.Contains(c.text(), installDir("")) {
		t.Fatalf("default install directory not probed: %s", c.text())
	}
}

type installProbeRun struct {
	report SSHInstallReport
	err    error
	cmds   []string
}

// runInstallUntilBinaryRead drives InstallSSH through the probe and stops at
// the agent binary read, so nothing is uploaded or started on the test host.
func runInstallUntilBinaryRead(t *testing.T, sc dirScenario, dir string) installProbeRun {
	t.Helper()
	return runInstallNamingRuntime(t, sc, dir, "")
}

// runInstallNamingRuntime is runInstallUntilBinaryRead for an install that
// names its runtime driver.
func runInstallNamingRuntime(t *testing.T, sc dirScenario, dir, runtimeDriver string) installProbeRun {
	t.Helper()
	inner := scenarioHandler(t, sc, dir)
	var run installProbeRun
	handler := func(cmd string, stdin io.Reader) (string, string, uint32) {
		run.cmds = append(run.cmds, cmd)
		return inner(cmd, stdin)
	}
	host, port := startFakeSSHServer(t, "root", "pw", handler)
	installer := NewSSHInstaller(func(string) ([]byte, error) { return nil, errors.New("agent binary is not available in this test") })
	run.report, run.err = installer.InstallSSH(t.Context(), SSHInstallRequest{
		Target: "root@" + host, Port: port, ServerURL: "https://server.example", Token: "tok", InstallDir: dir,
		InstanceKind: "container", RuntimeDriver: runtimeDriver, SSHAuth: SSHAuth{Mode: AuthPassword, Password: "pw"},
	})
	return run
}

func TestInstallSSHAppendsInstallDirectoryChecksAndOnlyWarns(t *testing.T) {
	dir := realTempDir(t)
	run := runInstallUntilBinaryRead(t, dirScenario{mounts: runpodNFS4Line, statfs: "nfs", chownErr: true}, dir)
	// The ownership and network-filesystem warnings did not stop the install:
	// it got as far as reading the agent binary.
	if run.err == nil || !strings.Contains(run.err.Error(), "agent binary is not available") {
		t.Fatalf("install error = %v", run.err)
	}
	wantCheck(t, run.report.Checks, CheckIDInstallDir, "pass", dir)
	wantCheck(t, run.report.Checks, CheckIDInstallDirOwnership, "warn", "Operation not permitted")
	wantCheck(t, run.report.Checks, CheckIDInstallDirFilesystem, "warn", "nfs4")
}

func TestInstallSSHStopsBeforeUploadOnNoexecDirectory(t *testing.T) {
	dir := realTempDir(t)
	run := runInstallUntilBinaryRead(t, dirScenario{mounts: "/dev/sdb1 %DIR% ext4 rw,noexec,relatime 0 0"}, dir)
	if run.err == nil || !strings.Contains(run.err.Error(), "install directory") || !strings.Contains(run.err.Error(), "noexec") {
		t.Fatalf("install error = %v", run.err)
	}
	wantCheck(t, run.report.Checks, CheckIDInstallDir, "fail", "mounted noexec")
	assertStoppedAtTheProbe(t, run)
}

// assertStoppedAtTheProbe fails when the install ran anything after the
// install-directory probe (the probe itself reads `uname -m`, so it is skipped).
func assertStoppedAtTheProbe(t *testing.T, run installProbeRun) {
	t.Helper()
	for _, cmd := range run.cmds {
		if strings.Contains(cmd, "probed=%s") {
			continue
		}
		if strings.Contains(cmd, "uname -m") || strings.Contains(cmd, ".new.") {
			t.Fatalf("install went on after the blocking verdict: %q", cmd)
		}
	}
}

// The probe writes only inside directories mktemp creates, so a symlink a local
// user planted in a world-writable install directory is never followed by the
// privileged chown or touch: the file it points at keeps its content, and the
// probe leaves the directory as it found it.
func TestInstallDirProbeNeverFollowsPlantedSymlinks(t *testing.T) {
	dir := realTempDir(t)
	victim := filepath.Join(realTempDir(t), "victim")
	if err := os.WriteFile(victim, []byte("root's secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".gpuworks-dircheck.1", ".gpuworks-names:1", "owner", "name:probe", ".gpuworks-probe.AAAAAA"} {
		if err := os.Symlink(victim, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	probeScenario(t, dirScenario{mounts: "/dev/sda1 %DIR% ext4 rw,relatime 0 0"}, dir, dir, "vm")
	if body, err := os.ReadFile(victim); err != nil || string(body) != "root's secret" {
		t.Fatalf("victim = %q %v", body, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 5 {
		t.Fatalf("probe left %v", entries)
	}
}

// Every value is printed with printf, so a path holding a backslash is
// reported as it is (dash's echo would turn "\t" into a tab).
func TestInstallDirProbeReportsABackslashPathVerbatim(t *testing.T) {
	dir := filepath.Join(realTempDir(t), `back\tslash`)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out := probeScenario(t, dirScenario{mounts: "/dev/sda1 / ext4 rw,relatime 0 0"}, dir, dir, "vm")
	wantCheck(t, out.Checks, CheckIDInstallDir, "pass", dir+": filesystem ext4")
}

// A relative install directory is refused before anything runs on the host:
// it would land wherever the SSH session starts, and a leading '-' reads as an
// option to every tool.
func TestInstallDirProbeRefusesARelativeDirectory(t *testing.T) {
	ran := 0
	host, port := startFakeSSHServer(t, "root", "pw", func(string, io.Reader) (string, string, uint32) {
		ran++
		return "", "", 0
	})
	client := dialFakeSSH(t, host, port, "root", "pw")
	for _, dir := range []string{"gpuworks", "-dash"} {
		out := ProbeInstallDir(t.Context(), client, dir, "vm", "")
		if len(out.Checks) != 1 || out.Checks[0].ID != CheckIDInstallDir || out.Checks[0].Status != "fail" || !strings.Contains(out.Checks[0].text(), "not an absolute path") {
			t.Fatalf("%q: checks = %+v", dir, out.Checks)
		}
	}
	if ran != 0 {
		t.Fatalf("%d commands ran for relative directories", ran)
	}
}

// Root that cannot write is told why root cannot (a read-only mount or
// root-squashing NFS), not that it lacks sudo.
func TestInstallDirProbeRootThatCannotWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a real root writes through the directory mode this test relies on")
	}
	dir := realTempDir(t)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	out := probeScenario(t, dirScenario{mounts: "nfs:/export %DIR% nfs4 rw 0 0", statfs: "nfs"}, dir, dir, "vm")
	wantCheck(t, out.Checks, CheckIDInstallDir, "fail", "not writable even by root", "maps root to an unprivileged user")
}

// A filesystem that refuses ':' fails the check on a node without docker; with
// docker it is a warning, since dockerd keeps the images elsewhere.
func TestInstallDirProbeFileNamesWithAndWithoutDocker(t *testing.T) {
	for _, docker := range []bool{false, true} {
		dir := realTempDir(t)
		out := probeScenario(t, dirScenario{mounts: runpodNFS4Line, statfs: "nfs", colonErr: true, docker: docker}, dir, dir, "container")
		if docker {
			wantCheck(t, out.Checks, CheckIDInstallDirNames, "warn", "this node has docker", "Invalid argument")
		} else {
			wantCheck(t, out.Checks, CheckIDInstallDirNames, "fail", "cannot run deployments from here", "Invalid argument")
		}
	}
}

// `mount` on an M4 Max (macOS 26.5). / is the sealed read-only system volume;
// a home directory lives on the data volume behind the /Users firmlink, which
// a mount point prefix does not reveal.
const macMountOutput = `/dev/disk3s1s1 on / (apfs, sealed, local, read-only, journaled)
devfs on /dev (devfs, local, nobrowse)
/dev/disk3s6 on /System/Volumes/VM (apfs, local, noexec, journaled, noatime, nobrowse)
/dev/disk3s5 on /System/Volumes/Data (apfs, local, journaled, nobrowse, protect, root data)
map auto_home on /System/Volumes/Data/home (autofs, automounted, nobrowse)
/dev/disk5s1 on /Volumes/千问办公 Installer (hfs, local, nodev, nosuid, read-only, noowners, quarantine, mounted by chanpinguihua)
/dev/disk7s1 on %DIR% (apfs, local, nodev, nosuid, noexec, journaled, noowners)`

func TestInstallDirProbeMacHomeIsOnTheDataVolume(t *testing.T) {
	dir := t.TempDir()
	out := probeScenario(t, dirScenario{mac: true, macMount: macMountOutput, dfMount: "/System/Volumes/Data", dfSource: "/dev/disk3s5", uid: "501", user: "chanpinguihua"}, dir, dir, "")
	if out.Noexec {
		t.Fatal("the data volume was reported noexec")
	}
	wantCheck(t, out.Checks, CheckIDInstallDir, "pass", "filesystem apfs", "mounted at /System/Volumes/Data")
	for _, c := range out.Checks {
		if strings.Contains(c.text(), "-c") && c.ID == CheckIDInstallDirFilesystem {
			t.Fatalf("BSD stat's echo leaked into the filesystem type: %+v", c)
		}
	}
	wantCheck(t, out.Checks, CheckIDInstallDirFilesystem, "pass", "apfs (local)")
}

func TestInstallDirProbeMacNoexecVolumeBlocks(t *testing.T) {
	dir := t.TempDir()
	out := probeScenario(t, dirScenario{mac: true, macMount: macMountOutput, dfMount: "%DIR%", dfSource: "/dev/disk7s1", uid: "501"}, dir, dir, "")
	if !out.Noexec {
		t.Fatalf("a noexec volume was not detected: %+v", out.Checks)
	}
}

func TestInstallDirProbeMacMountPointWithSpaces(t *testing.T) {
	dir := t.TempDir()
	out := probeScenario(t, dirScenario{mac: true, macMount: macMountOutput, dfMount: "/Volumes/千问办公 Installer", dfSource: "/dev/disk5s1", uid: "501"}, dir, dir, "")
	wantCheck(t, out.Checks, CheckIDInstallDir, "pass", "filesystem hfs", "mounted at /Volumes/千问办公 Installer,")
}

func probeMacScenario(t *testing.T, sc dirScenario, requested string, admin bool) InstallDirOutcome {
	t.Helper()
	handler := scenarioHandler(t, sc, t.TempDir())
	host, port := startFakeSSHServer(t, "root", "pw", handler)
	client := dialFakeSSH(t, host, port, "root", "pw")
	return ProbeMacInstallDir(t.Context(), client, requested, admin)
}

// /opt on a Mac belongs to root. With sudo from the SSH password the installer
// creates /opt/gpuworks; the probe's own sudo -n cannot, and must not report
// the Linux fallback to /tmp that a Mac install never takes. (/proc stands in
// for a directory nobody here can write, root included.)
func TestInstallDirProbeMacDirectoryTheInstallerCreatesWithSudo(t *testing.T) {
	sc := dirScenario{mac: true, macMount: macMountOutput, dfMount: "/System/Volumes/Data", dfSource: "/dev/disk3s5", uid: "501", user: "huangsheng"}
	out := probeMacScenario(t, sc, "/proc/gpuworks", true)
	wantCheck(t, out.Checks, CheckIDInstallDir, "pass", "created by the installer with administrator rights", "mounted at /System/Volumes/Data")
	wantCheck(t, out.Checks, CheckIDInstallDirOwnership, "na", "not needed on a Mac")
	for _, c := range out.Checks {
		if strings.Contains(c.text(), "/tmp/gpuworks") {
			t.Fatalf("a Mac check names the Linux fallback: %+v", c)
		}
	}

	out = probeMacScenario(t, sc, "/proc/gpuworks", false)
	wantCheck(t, out.Checks, CheckIDInstallDir, "fail", "/proc is not writable by huangsheng", "leave the directory empty to install in ~/gpuworks", "account password")
}

func TestExpandRemoteHome(t *testing.T) {
	host, port := startFakeSSHServer(t, "me", "pw", func(cmd string, _ io.Reader) (string, string, uint32) {
		if cmd == `printf '%s' "$HOME"` {
			return "/Users/huangsheng", "", 0
		}
		return "", "unexpected " + cmd, 127
	})
	client := dialFakeSSH(t, host, port, "me", "pw")
	for in, want := range map[string]string{
		"~/.gpuworks":   "/Users/huangsheng/.gpuworks",
		" ~ ":           "/Users/huangsheng",
		"/opt/gpuworks": "/opt/gpuworks",
		"":              "",
		"~other/x":      "~other/x", // another user's home is not resolved; the absolute-path check rejects it
	} {
		if got := expandRemoteHome(t.Context(), client, in); got != want {
			t.Errorf("expandRemoteHome(%q) = %q, want %q", in, got, want)
		}
	}
}
