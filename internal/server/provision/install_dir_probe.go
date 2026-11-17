package provision

import (
	"context"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// The agent keeps its binary, the runtime's PRoot and unpacked images, and the
// model cache under the install directory, so the filesystem behind it decides
// whether the node works at all (noexec), which user workloads can run as
// (ownership changes), how fast models load (network filesystems) and whether
// the install survives the instance being recreated (container overlay). The
// precheck and the SSH install share this one probe so both say the same thing.

// minInstallDirFreeKiB is the free space below which the install directory is
// flagged: one inference engine image unpacks to 10-20 GiB under the udocker
// runtime and a mid-size model is another 15-70 GiB, so 50 GiB is the least
// that fits one engine plus one model with some headroom.
const minInstallDirFreeKiB = 50 << 20

// installDirProbeTimeout bounds one probe: stat and df block indefinitely on a
// hung network mount.
var installDirProbeTimeout = 20 * time.Second

// ownershipProbeID is the uid:gid the probe tries to hand a scratch file to.
// It is the conventional "nobody" id: any filesystem that lets root assign it
// lets root assign the dedicated udocker user's id as well.
const ownershipProbeID = "65534:65534"

// installDirProbeScript prints facts about dir as key=value lines. It is POSIX
// sh and needs neither findmnt nor GNU-only flags, and creates no directory on
// the install path. Its writes go only into scratch directories mktemp creates
// in the nearest existing ancestor (mode 700, a fresh name), never to a
// predictable path, so a symlink planted there cannot redirect a privileged
// write; a trap removes them however the script ends. It always exits 0; a fact
// it could not learn is printed empty or left out.
func installDirProbeScript(dir string) string {
	return "dir=" + shellQuote(dir) + "\n" + installDirProbeBody
}

// installDirProbeCommand wraps the script so it runs under sh whatever the SSH
// account's login shell is.
func installDirProbeCommand(dir string) string {
	return "sh -c " + shellQuote(installDirProbeScript(dir))
}

const installDirProbeBody = `mounts=/proc/mounts
probe=$dir
while [ ! -d "$probe" ]; do
  parent=$(dirname -- "$probe")
  [ "$parent" = "$probe" ] && break
  probe=$parent
done
if [ -d "$dir" ]; then exists=yes; elif [ -e "$dir" ]; then exists=file; else exists=no; fi
resolved=$(CDPATH= cd -- "$probe" 2>/dev/null && pwd -P)
[ -n "$resolved" ] || resolved=$probe
uid=$(id -u 2>/dev/null)
printf 'requested=%s\n' "$dir"
printf 'exists=%s\n' "$exists"
printf 'probed=%s\n' "$resolved"
printf 'uid=%s\n' "$uid"
printf 'user=%s\n' "$(id -un 2>/dev/null)"
info=""
if command -v findmnt >/dev/null 2>&1; then
  info=$(findmnt -n -o TARGET,FSTYPE,OPTIONS -T "$resolved" 2>/dev/null | head -n 1 | awk 'NF >= 3 { t = $1; for (i = 2; i < NF - 1; i++) t = t " " $i; print "mount=" t; print "fstype=" $(NF - 1); print "options=" $NF }')
fi
if [ -z "$info" ] && [ -r "$mounts" ]; then
  info=$(P="$resolved" awk 'BEGIN { best = -1; p = ENVIRON["P"] }
    { m = $2; gsub(/\\040/, " ", m); gsub(/\\011/, "\t", m)
      if ((m == "/" || p == m || substr(p, 1, length(m) + 1) == m "/") && length(m) >= best) { best = length(m); t = m; f = $3; o = $4 } }
    END { if (best >= 0) { print "mount=" t; print "fstype=" f; print "options=" o } }' "$mounts" 2>/dev/null)
fi
if [ -z "$info" ] && [ ! -r "$mounts" ] && command -v mount >/dev/null 2>&1; then
  # BSD and macOS. df names the mount point: on macOS a path's prefix can
  # mislead, since /Users is a firmlink into /System/Volumes/Data while / is
  # the sealed read-only system volume. mount then gives that point's type and
  # options: "/dev/disk3s5 on /System/Volumes/Data (apfs, local, journaled)".
  point=$(df -Pk -- "$resolved" 2>/dev/null | awk 'NR == 2 { m = $6; for (i = 7; i <= NF; i++) m = m " " $i; print m }')
  [ -z "$point" ] || info=$(mount 2>/dev/null | M="$point" awk '
    { on = index($0, " on "); open = index($0, " ("); if (!on || open <= on) next
      m = substr($0, on + 4, open - on - 4); if (m != ENVIRON["M"]) next
      opts = substr($0, open + 2); sub(/\)$/, "", opts)
      n = split(opts, parts, ", "); o = ""
      for (i = 2; i <= n; i++) { v = parts[i]; if (v == "read-only") v = "ro"; o = o (o == "" ? "" : ",") v }
      print "mount=" m; print "fstype=" parts[1]; print "options=" o; exit }')
fi
[ -z "$info" ] || printf '%s\n' "$info"
# GNU stat only: BSD stat reads -f as a format and echoes it back.
fs=$(stat -f -c %T -- "$resolved" 2>/dev/null) || fs=""
printf 'statfs=%s\n' "$fs"
df -Pk -- "$resolved" 2>/dev/null | awk 'NR == 2 { print "total_kib=" $2; print "avail_kib=" $4 }'
container=no
containerfiles="/.dockerenv /run/.containerenv"
for f in $containerfiles; do [ -e "$f" ] && container=yes; done
printf 'container=%s\n' "$container"
sudo=no
if [ "$uid" != 0 ] && command -v sudo >/dev/null 2>&1 && sudo -n true >/dev/null 2>&1; then sudo=yes; fi
printf 'sudo=%s\n' "$sudo"
priv=""
[ "$sudo" = yes ] && priv="sudo -n"
docker=no
if command -v docker >/dev/null 2>&1; then
  limit=""
  command -v timeout >/dev/null 2>&1 && limit="timeout 5"
  if $limit docker info >/dev/null 2>&1 || { [ -n "$priv" ] && $priv $limit docker info >/dev/null 2>&1; }; then docker=yes; fi
fi
printf 'docker=%s\n' "$docker"
# The agent takes the first runtime that answers (docker, then nerdctl, then
# runveil), so a containerd node without docker runs nerdctl, not runveil.
nerdctl=no
if command -v nerdctl >/dev/null 2>&1; then
  limit=""
  command -v timeout >/dev/null 2>&1 && limit="timeout 5"
  if $limit nerdctl -n gpuworks ps >/dev/null 2>&1 || { [ -n "$priv" ] && $priv $limit nerdctl -n gpuworks ps >/dev/null 2>&1; }; then nerdctl=yes; fi
fi
printf 'nerdctl=%s\n' "$nerdctl"
printf 'arch=%s\n' "$(uname -m 2>/dev/null)"
ud=""
pd=""
cleanup() {
  [ -z "$ud" ] || rm -rf -- "$ud"
  [ -z "$pd" ] || $priv rm -rf -- "$pd"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
writable=no
if ud=$(mktemp -d "$resolved/.gpuworks-probe.XXXXXX" 2>/dev/null); then writable=yes; else ud=""; fi
printf 'writable=%s\n' "$writable"
chown_state=skipped
if [ "$uid" = 0 ] || [ "$sudo" = yes ]; then
  if pd=$($priv mktemp -d "$resolved/.gpuworks-probe.XXXXXX" 2>/dev/null); then
    printf 'priv_writable=yes\n'
    if err=$($priv sh -c ': > "$1/owner" && chown ` + ownershipProbeID + ` "$1/owner"' sh "$pd" 2>&1); then
      chown_state=ok
    else
      chown_state=refused
      printf 'chown_err=%s\n' "$(printf '%s' "$err" | tr '\n' ' ')"
    fi
  else
    pd=""
    printf 'priv_writable=no\n'
  fi
fi
printf 'chown=%s\n' "$chown_state"
# A root agent launches workloads as the dedicated runtime user, which has to
# search every directory above the state directory (the install directory
# unless --state says otherwise; the installer opens that one itself). Judge
# the user, its primary group or others the way the kernel picks one class,
# exactly as the agent does: the account may not exist yet, and then only the
# others bits can let it through. stat runs with the agent's privileges, since
# the SSH user may be unable to look inside a private home.
anc=skipped
if [ "$chown_state" != refused ] && { [ "$uid" = 0 ] || [ "$sudo" = yes ]; }; then
  ruid=$(id -u udocker 2>/dev/null) || ruid=-1
  rgid=$(id -g udocker 2>/dev/null) || rgid=-1
  # Supplementary groups (the device groups grant-device-access adds): the
  # kernel counts them as the group class too.
  rgids=" $(id -G udocker 2>/dev/null) "
  printf 'runtime_uid=%s\n' "$ruid"
  # The agent creates a missing account with useradd; without one it runs
  # workloads as itself.
  useradd=no
  if $priv sh -c 'command -v useradd' >/dev/null 2>&1; then useradd=yes; fi
  printf 'useradd=%s\n' "$useradd"
  if [ "$exists" = yes ]; then p=$(dirname -- "$resolved"); else p=$resolved; fi
  anc=ok
  while :; do
    if ! st=$($priv stat -c '%a %u %g' -- "$p" 2>/dev/null); then anc=unknown; break; fi
    set -- $st
    m=$((0$1))
    if [ "$2" = "$ruid" ]; then b=$((m / 64 % 8)); elif [ "$3" = "$rgid" ]; then b=$((m / 8 % 8)); else
      case "$rgids" in *" $3 "*) b=$((m / 8 % 8)) ;; *) b=$((m % 8)) ;; esac
    fi
    if [ $((b % 2)) = 0 ]; then
      anc=blocked
      printf 'ancestor_dir=%s\nancestor_mode=%s\nancestor_uid=%s\nancestor_gid=%s\n' "$p" "$1" "$2" "$3"
      # The mode bits are the whole story only without an ACL (ls marks one
      # with a trailing +): an entry naming the user could still let it in.
      lsl=$($priv ls -ld -- "$p" 2>/dev/null)
      case "${lsl%% *}" in "") acl=unknown ;; *+) acl=yes ;; *) acl=no ;; esac
      printf 'ancestor_acl=%s\n' "$acl"
      break
    fi
    next=$(dirname -- "$p")
    [ "$next" = "$p" ] && break
    p=$next
  done
fi
printf 'ancestors=%s\n' "$anc"
names=skipped
nd=""
nw=""
if [ -n "$ud" ]; then nd=$ud; elif [ -n "$pd" ]; then nd=$pd; nw=$priv; fi
if [ -n "$nd" ]; then
  if err=$($nw touch -- "$nd/name:probe" 2>&1); then
    names=ok
  else
    names=refused
    printf 'names_err=%s\n' "$(printf '%s' "$err" | tr '\n' ' ')"
  fi
fi
printf 'names=%s\n' "$names"
exit 0
`

func gib(kib int64) string { return fmt.Sprintf("%.1f GiB", float64(kib)/(1<<20)) }

// location says which directory was inspected and where it lives: filesystem
// type and mount point, never the mount options. A container root mount lists
// every image layer in its options (dozens of paths on a layered image), which
// says nothing the type does not; noexec, the one option that matters, has its
// own message.
func (f installDirFacts) location() string {
	var b strings.Builder
	b.WriteString(f.requested)
	if f.exists == "no" {
		fmt.Fprintf(&b, " (does not exist yet; inspected its nearest existing parent %s)", f.probed)
	} else if f.probed != f.requested {
		fmt.Fprintf(&b, " (resolves to %s)", f.probed)
	}
	if fs := f.filesystem(); fs != "" {
		fmt.Fprintf(&b, ": filesystem %s", fs)
	} else {
		b.WriteString(": filesystem type unknown")
	}
	if f.mount != "" {
		fmt.Fprintf(&b, ", mounted at %s", f.mount)
	}
	if f.haveSpace {
		fmt.Fprintf(&b, ", %s free", gib(f.availKiB))
	}
	return b.String()
}

// installDirChecks turns the facts into one CheckResult per verdict.
// instanceKind is the caller's hint ("container" or "vm"); the probe's own
// /.dockerenv and /run/.containerenv sniffing covers callers without one.
// providerKind is set when a cloud provider's one-click flow chose the
// directory, which the operator then cannot change.
func installDirChecks(f installDirFacts, instanceKind, providerKind string) (checks []CheckResult, noexec bool) {
	noexec = f.noexec()
	checks = append(checks,
		installDirAccessCheck(f, noexec),
		installDirOwnershipCheck(f),
		installDirTraversalCheck(f),
		installDirFilesystemCheck(f),
		installDirSpaceCheck(f),
		installDirPersistenceCheck(f, instanceKind, providerKind),
		installDirNamesCheck(f),
	)
	return checks, noexec
}

// installDirNamesCheck reports a filesystem that refuses ':' in file names
// (RunPod's /workspace answers EINVAL). The runveil runtime keeps its images
// and unpacks their root filesystems under the install directory, and both
// hold such names, so it cannot run anything from there: a failure on a node
// without docker. A node with docker keeps its images with dockerd, so there
// it is only a warning about the runtime the node does not use. The install is
// not stopped either way.
func installDirNamesCheck(f installDirFacts) CheckResult {
	params := map[string]any{"dir": f.probed}
	switch f.names {
	case "ok":
		return PassCheck(CheckIDInstallDirNames, messages.CheckInstallDirColonAllowed, "':' is allowed in file names", nil)
	case "refused":
		refusal := fmt.Sprintf("%s does not allow ':' in file names, which container images need (image layers are named sha256:<hex>, and Debian-based images hold /var/lib/dpkg/info/<package>:amd64.list)", f.probed)
		if f.docker {
			return WarnCheck(CheckIDInstallDirNames, messages.CheckInstallDirColonRefusedDocker, refusal+"; this node has docker, which keeps images elsewhere, but the runveil runtime could not run from here", params).WithEvidence(f.namesErr)
		}
		return FailCheck(CheckIDInstallDirNames, messages.CheckInstallDirColonRefused, refusal+", so the runveil runtime used on nodes without docker cannot run deployments from here; choose an install directory on a local filesystem (on a container instance such as RunPod, a path on the container disk sized for the images)", params).WithEvidence(f.namesErr)
	default:
		return SkipCheck(CheckIDInstallDirNames, messages.CheckInstallDirNamesUntested, fmt.Sprintf("not checked: no file could be created in %s", f.probed), params)
	}
}

// installDirProblem is one reason the install directory cannot hold the
// agent; the first one found is the check's code.
type installDirProblem struct {
	code, message string
}

func installDirAccessCheck(f installDirFacts, noexec bool) CheckResult {
	params := map[string]any{"dir": f.requested, "user": f.userLabel()}
	if f.probed != f.requested {
		params["probed"] = f.probed
	}
	if fs := f.filesystem(); fs != "" {
		params["filesystem"] = fs
	}
	if f.mount != "" {
		params["mount"] = f.mount
	}
	var problems []installDirProblem
	if f.exists == "file" {
		problems = append(problems, installDirProblem{messages.CheckInstallDirNotDirectory, f.requested + " exists but is not a directory"})
	}
	if noexec {
		problems = append(problems, installDirProblem{messages.CheckInstallDirNoexec, fmt.Sprintf("%s is mounted noexec, so the agent binary and the runtime's PRoot cannot run from it; choose a directory on a filesystem mounted with exec", f.mount)})
	}
	var access, accessCode string
	switch {
	case f.mac && !f.writable && f.macAdmin:
		access, accessCode = "created by the installer with administrator rights", messages.CheckInstallDirMacAdmin
	case f.mac && !f.writable:
		problems = append(problems, installDirProblem{messages.CheckInstallDirMacNotWritable, fmt.Sprintf("%s is not writable by %s and the installer has no administrator rights; leave the directory empty to install in ~/gpuworks, or sign in with the account password so the installer can use sudo", f.probed, f.userLabel())})
	case f.writable:
		access, accessCode = "writable by "+f.userLabel(), messages.CheckInstallDirWritable
	case f.sudo && f.privWritable:
		access, accessCode = "writable through passwordless sudo", messages.CheckInstallDirWritableSudo
	case f.root():
		problems = append(problems, installDirProblem{messages.CheckInstallDirRootNotWritable, fmt.Sprintf("%s is not writable even by root (a read-only mount, or a network filesystem that maps root to an unprivileged user); choose a writable directory (left empty, the install falls back to /tmp/gpuworks, which is lost on reboot)", f.probed)})
	case f.sudo:
		problems = append(problems, installDirProblem{messages.CheckInstallDirSudoNotWritable, fmt.Sprintf("%s is not writable by %s, even through sudo; choose a writable directory (left empty, the install falls back to /tmp/gpuworks, which is lost on reboot)", f.probed, f.userLabel())})
	default:
		problems = append(problems, installDirProblem{messages.CheckInstallDirNotWritable, fmt.Sprintf("%s is not writable by %s and there is no passwordless sudo; choose a writable directory (left empty, the install falls back to /tmp/gpuworks, which is lost on reboot)", f.probed, f.userLabel())})
	}
	if len(problems) > 0 {
		texts := make([]string, len(problems))
		for i, p := range problems {
			texts[i] = p.message
		}
		return FailCheck(CheckIDInstallDir, problems[0].code, strings.Join(texts, "; ")+". "+f.location(), params)
	}
	return PassCheck(CheckIDInstallDir, accessCode, f.location()+"; "+access, params)
}

func (f installDirFacts) userLabel() string {
	if f.user != "" {
		return f.user
	}
	return "uid " + f.uid
}

func installDirOwnershipCheck(f installDirFacts) CheckResult {
	if f.mac {
		return NACheck(CheckIDInstallDirOwnership, messages.CheckNotNeededOnMac, "not needed on a Mac: engines run as the agent's own user", nil)
	}
	switch f.chown {
	case "ok":
		return PassCheck(CheckIDInstallDirOwnership, messages.CheckInstallDirChownAllowed, "files can be handed to another user; runveil workloads run as the dedicated runtime user", nil)
	case "refused":
		return WarnCheck(CheckIDInstallDirOwnership, messages.CheckInstallDirChownRefused,
			fmt.Sprintf("%s does not allow changing file ownership, so runveil workloads will run as the agent's own user (root) instead of the dedicated runtime user", f.probed),
			map[string]any{"dir": f.probed}).WithEvidence(f.chownErr)
	default:
		if !f.root() && !f.sudo {
			return NACheck(CheckIDInstallDirOwnership, messages.CheckInstallDirChownUnprivileged,
				fmt.Sprintf("not checked: %s is not root and has no passwordless sudo, so the agent will run as that user anyway", f.userLabel()), map[string]any{"user": f.userLabel()})
		}
		return SkipCheck(CheckIDInstallDirOwnership, messages.CheckInstallDirChownUntested,
			fmt.Sprintf("not checked: no file could be created in %s with privileges", f.probed), map[string]any{"dir": f.probed})
	}
}

func installDirFilesystemCheck(f installDirFacts) CheckResult {
	if f.onNetworkFilesystem() {
		return WarnCheck(CheckIDInstallDirFilesystem, messages.CheckInstallDirNetworkFilesystem,
			fmt.Sprintf("%s is a network or userspace filesystem (%s); model loading and engine start-up speed depend on its throughput and latency", f.probed, f.filesystem()),
			map[string]any{"dir": f.probed, "filesystem": f.filesystem()})
	}
	if fs := f.filesystem(); fs != "" {
		return PassCheck(CheckIDInstallDirFilesystem, messages.CheckInstallDirLocalFilesystem, fs+" (local)", map[string]any{"filesystem": fs})
	}
	return SkipCheck(CheckIDInstallDirFilesystem, messages.CheckInstallDirFilesystemUnknown, "filesystem type unknown", nil)
}

func installDirSpaceCheck(f installDirFacts) CheckResult {
	if !f.haveSpace {
		return SkipCheck(CheckIDInstallDirSpace, messages.CheckInstallDirSpaceUnknown, "free space unknown (df reported nothing)", nil)
	}
	detail := fmt.Sprintf("%s free of %s", gib(f.availKiB), gib(f.totalKiB))
	params := map[string]any{"available": gib(f.availKiB), "total": gib(f.totalKiB), "available_kib": f.availKiB, "total_kib": f.totalKiB}
	if f.availKiB < minInstallDirFreeKiB {
		params["minimum"] = gib(minInstallDirFreeKiB)
		return WarnCheck(CheckIDInstallDirSpace, messages.CheckInstallDirSpaceLow,
			fmt.Sprintf("%s; inference engine images and model weights need roughly %s or more", detail, gib(minInstallDirFreeKiB)), params)
	}
	return PassCheck(CheckIDInstallDirSpace, messages.CheckInstallDirSpaceOK, detail, params)
}

// installDirPersistenceCheck warns about an install directory whose contents
// do not outlive the instance. In a provider's one-click flow the directory is
// fixed and the container disk is the only place the agent can live (the
// provider's network volume refuses the file names the runtime needs), so the
// warning says what will be lost instead of advising a move the operator
// cannot make there.
func installDirPersistenceCheck(f installDirFacts, instanceKind, providerKind string) CheckResult {
	container := f.container || strings.EqualFold(strings.TrimSpace(instanceKind), "container")
	params := map[string]any{"dir": f.probed, "filesystem": f.filesystem()}
	switch {
	case f.onOverlay() && container && strings.TrimSpace(providerKind) != "":
		params["provider"] = strings.TrimSpace(providerKind)
		return WarnCheck(CheckIDInstallDirPersistence, messages.CheckInstallDirProviderDisk, fmt.Sprintf("%s is on the instance's container disk (%s), the only place the agent can live on a %s container instance. It is lost when the instance is deleted or recreated, taking the agent, its identity and the model cache with it; install the agent again on the replacement", f.probed, f.filesystem(), strings.TrimSpace(providerKind)), params)
	case f.onOverlay() && container:
		return WarnCheck(CheckIDInstallDirPersistence, messages.CheckInstallDirContainerLayer, fmt.Sprintf("%s is on the container's writable layer (%s), which is lost when the instance is recreated, taking the agent, its identity and the model cache with it; install under a persistent volume on a local filesystem if the instance has one; a network volume that refuses ':' in file names (RunPod's /workspace) cannot hold the runveil runtime's images", f.probed, f.filesystem()), params)
	case f.onMemory():
		return WarnCheck(CheckIDInstallDirPersistence, messages.CheckInstallDirInMemory, fmt.Sprintf("%s is on %s, which lives in memory and is lost on reboot", f.probed, f.filesystem()), params)
	default:
		return PassCheck(CheckIDInstallDirPersistence, messages.CheckInstallDirPersistent, "not on a container's writable layer", nil)
	}
}

// InstallDirOutcome is what probing an install directory found.
type InstallDirOutcome struct {
	Checks []CheckResult
	// Noexec: the directory is on a noexec mount, so nothing installed there
	// can run. The install stops on it; every other verdict only reports.
	Noexec bool
	// Untraversable: a directory above the install directory is closed to the
	// dedicated runtime user, and the probe established that the agent will
	// run as root with the runveil runtime and that user, so every launch
	// would fail. The install stops on it, like Noexec.
	Untraversable bool
	// untraversable is the error an install stops with when Untraversable.
	untraversable *InstallDirUntraversableError
}

// blockingError is why an install must not go ahead on this directory, nil
// when nothing found stops it.
func (o InstallDirOutcome) blockingError() error {
	switch {
	case o.Noexec:
		return noexecInstallError(o)
	case o.Untraversable && o.untraversable != nil:
		return o.untraversable
	}
	return nil
}

// keepUntraversable is the outcome for a reinstall of an agent that already
// runs from this directory (out-of-band recovery): refusing would leave the
// node offline, which is worse than workloads that cannot launch, so the
// closed ancestor stays a warning.
func (o InstallDirOutcome) keepUntraversable() InstallDirOutcome {
	if !o.Untraversable {
		return o
	}
	checks := append([]CheckResult(nil), o.Checks...)
	for i := range checks {
		if checks[i].ID == CheckIDInstallDirTraversal {
			checks[i].Status = proto.CheckWarn
			checks[i].Code = messages.CheckInstallDirPathClosedReinstall
			checks[i].Message += "; not stopping the reinstall of an agent that already runs from here"
		}
	}
	o.Checks, o.Untraversable, o.untraversable = checks, false, nil
	return o
}

// ProbeInstallDir inspects the install directory over SSH. dir is used as
// given (callers resolve the default with installDir). A probe that cannot run
// is reported as a warning rather than an error: it says nothing about the
// directory, and must not stop an install that would otherwise work.
func ProbeInstallDir(ctx context.Context, client *ssh.Client, dir, instanceKind, providerKind string) InstallDirOutcome {
	return ProbeInstallDirForRuntime(ctx, client, dir, instanceKind, providerKind, "")
}

// ProbeInstallDirForRuntime is ProbeInstallDir for an install that names its
// runtime driver (docker, nerdctl, udocker; "" lets the agent pick), which
// decides whether the runveil runtime will be used at all.
func ProbeInstallDirForRuntime(ctx context.Context, client *ssh.Client, dir, instanceKind, providerKind, runtimeDriver string) InstallDirOutcome {
	return probeInstallDir(ctx, client, dir, instanceKind, providerKind, func(f *installDirFacts) { f.runtimeDriver = strings.ToLower(strings.TrimSpace(runtimeDriver)) })
}

func probeInstallDir(ctx context.Context, client *ssh.Client, dir, instanceKind, providerKind string, adjust func(*installDirFacts)) InstallDirOutcome {
	if !strings.HasPrefix(dir, "/") {
		// A relative directory lands wherever the SSH session starts, and a
		// leading '-' would read as an option to every tool the probe runs.
		return InstallDirOutcome{Checks: []CheckResult{FailCheck(CheckIDInstallDir, messages.CheckInstallDirNotAbsolute,
			fmt.Sprintf("%q is not an absolute path; give the install directory as one, for example /opt/gpuworks", dir), map[string]any{"dir": dir})}}
	}
	ctx, cancel := context.WithTimeout(ctx, installDirProbeTimeout)
	defer cancel()
	out, err := runContext(ctx, client, installDirProbeCommand(dir))
	if err != nil {
		return InstallDirOutcome{Checks: []CheckResult{installDirProbeFailed(dir, outputEvidence(out, err))}}
	}
	facts, err := parseInstallDirFacts(out)
	if err != nil {
		return InstallDirOutcome{Checks: []CheckResult{installDirProbeFailed(dir, err.Error())}}
	}
	if adjust != nil {
		adjust(&facts)
	}
	checks, noexec := installDirChecks(facts, instanceKind, providerKind)
	outcome := InstallDirOutcome{Checks: checks, Noexec: noexec}
	if v := installDirTraversalVerdict(facts); v.blocks {
		outcome.Untraversable = true
		outcome.untraversable = &InstallDirUntraversableError{InstallDir: facts.requested, Ancestor: facts.ancestorDir, Mode: facts.ancestorMode, Detail: v.detail}
	}
	return outcome
}

// ProbeMacInstallDir probes an install directory on a Mac. admin says whether
// the installer will have administrator rights (macPrivilege), which the probe
// cannot learn itself when they come from sudo with the SSH password.
func ProbeMacInstallDir(ctx context.Context, client *ssh.Client, dir string, admin bool) InstallDirOutcome {
	return probeInstallDir(ctx, client, dir, "", "", func(f *installDirFacts) { f.mac, f.macAdmin = true, admin })
}

// installDirProbeFailed is the row for a probe that could not run: it says
// nothing about the directory, so the checks are skipped, not failed.
func installDirProbeFailed(dir, evidence string) CheckResult {
	return SkipCheck(CheckIDInstallDir, messages.CheckInstallDirProbeFailed, "could not inspect "+dir, map[string]any{"dir": dir}).WithEvidence(evidence)
}

// mergeChecks adds checks to a report, replacing earlier rows of the same id
// so a re-probe after the install directory changed does not leave the old
// directory's verdicts beside the new ones.
func mergeChecks(existing, added []CheckResult) []CheckResult {
	for _, c := range added {
		replaced := false
		for i := range existing {
			if existing[i].ID == c.ID {
				existing[i], replaced = c, true
				break
			}
		}
		if !replaced {
			existing = append(existing, c)
		}
	}
	return existing
}

// noexecInstallError is the reason an install stops on a noexec directory.
func noexecInstallError(outcome InstallDirOutcome) error {
	for _, c := range outcome.Checks {
		if c.ID == CheckIDInstallDir {
			return fmt.Errorf("install directory: %s", c.Message)
		}
	}
	return fmt.Errorf("install directory is mounted noexec")
}
