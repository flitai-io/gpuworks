package kubemanifest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeKubectl answers the calls check.sh makes as kubectl does, from the
// files of its world: "ns" (the namespace exists; its content is the phase),
// "secret", "desired" (the DaemonSet exists; its content is
// desiredNumberScheduled, empty before the controller saw it), "pods" (the
// go-template output), "events-<kind>-<name>-<reason>", "unreachable" and
// "apply-fails" (its content is kubectl's error). Every call is logged to
// "calls"; a call it does not know fails loudly, as a kubectl that was
// handed a wrong command would.
const fakeKubectl = `#!/usr/bin/env bash
W="$GPUWORKS_FAKE"
printf '%s\n' "$*" >>"$W/calls"
args=" $* "
selector=
prev=
for a in "$@"; do
	[ "$prev" = --field-selector ] && selector=$a
	prev=$a
done
case "$args" in
*" get --raw /version "*)
	if [ -f "$W/unreachable" ]; then
		echo 'The connection to the server localhost:8080 was refused - did you specify the right host or port?' >&2
		exit 1
	fi
	echo '{"major":"1","minor":"34"}' ;;
*" get namespace "*)
	if [ -f "$W/ns" ]; then
		case "$args" in *" -o go-template="*) cat "$W/ns" ;; *) echo "namespace/x" ;; esac
		exit 0
	fi
	echo 'Error from server (NotFound): namespaces "x" not found' >&2
	exit 1 ;;
*" get secret "*)
	[ -f "$W/secret" ] && exit 0
	echo 'Error from server (NotFound): secrets "x" not found' >&2
	exit 1 ;;
*" apply -f - "*)
	n=$(ls "$W" | grep -c '^applied\.')
	cat >"$W/applied.$n"
	if [ -f "$W/apply-fails" ]; then
		cat "$W/apply-fails" >&2
		exit 1
	fi
	grep -q '^kind: Namespace' "$W/applied.$n" && printf Active >"$W/ns"
	echo applied ;;
*" get daemonset "*)
	if [ -f "$W/desired" ]; then
		case "$args" in *" -o go-template="*) cat "$W/desired" ;; esac
		exit 0
	fi
	echo 'Error from server (NotFound): daemonsets.apps "x" not found' >&2
	exit 1 ;;
*" get pods "*)
	case "$args" in *" -l app.kubernetes.io/name="*" -o go-template="*) ;; *) echo "unexpected pods call" >&2; exit 99 ;; esac
	[ -f "$W/pods" ] && cat "$W/pods"
	exit 0 ;;
*" get events "*)
	key=$(printf '%s' "$selector" | sed 's/involvedObject.kind=//; s/,involvedObject.name=/-/; s/,reason=/-/')
	[ -f "$W/events-$key" ] && cat "$W/events-$key"
	exit 0 ;;
*" delete "*) exit 0 ;;
*)
	echo "fake kubectl: unexpected call: $*" >&2
	exit 99 ;;
esac
`

// checkWorld is a PATH holding the fake kubectl and the few tools the
// script and the fake use, and the files the fake answers from.
type checkWorld struct {
	t   *testing.T
	dir string
	bin string
	// noKubectl leaves kubectl off PATH.
	noKubectl bool
	out       string
	exit      int
}

func newCheckWorld(t *testing.T) *checkWorld {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal("bash is not installed; the check script needs it")
	}
	root := t.TempDir()
	w := &checkWorld{t: t, dir: filepath.Join(root, "state"), bin: filepath.Join(root, "bin")}
	for _, d := range []string{w.dir, w.bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tool := range []string{"seq", "sleep", "grep", "tail", "cat", "ls", "sed", "env"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Fatalf("%s is not installed", tool)
		}
		if err := os.Symlink(p, filepath.Join(w.bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(bash, filepath.Join(w.bin, "bash")); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *checkWorld) write(name, content string) {
	w.t.Helper()
	if err := os.WriteFile(filepath.Join(w.dir, name), []byte(content), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

func (w *checkWorld) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(w.dir, name))
	return string(b)
}

func (w *checkWorld) exists(name string) bool {
	_, err := os.Stat(filepath.Join(w.dir, name))
	return err == nil
}

func (w *checkWorld) calls() []string {
	return strings.Split(strings.TrimSpace(w.read("calls")), "\n")
}

// called says whether some kubectl call held every part.
func (w *checkWorld) called(parts ...string) bool {
	for _, c := range w.calls() {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(" "+c+" ", p)
		}
		if all {
			return true
		}
	}
	return false
}

// run runs the script once, with no wait: the pods are read once and judged.
func (w *checkWorld) run(t *testing.T, script string) {
	t.Helper()
	if !w.noKubectl {
		if err := os.WriteFile(filepath.Join(w.bin, "kubectl"), []byte(fakeKubectl), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(filepath.Join(w.bin, "bash"), "-c", script)
	cmd.Env = []string{"PATH=" + w.bin, "GPUWORKS_FAKE=" + w.dir, "GPUWORKS_CHECK_WAIT=0", "HOME=" + w.dir}
	out, err := cmd.CombinedOutput()
	w.out, w.exit = string(out), 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		w.exit = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
}

// line is the output line for node ("-" for the whole check), or "".
func (w *checkWorld) line(node string) string {
	for _, l := range strings.Split(w.out, "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == node {
			return l
		}
	}
	return ""
}

func (w *checkWorld) wantCode(t *testing.T, node string, code CheckCode, detail string) {
	t.Helper()
	l := w.line(node)
	f := strings.Fields(l)
	if len(f) < 2 || f[1] != string(code) || !strings.Contains(l, detail) {
		t.Errorf("%s: want %s %q, got %q\nfull output:\n%s", node, code, detail, l, w.out)
	}
}

func precheckScript(t *testing.T) string {
	t.Helper()
	s, err := Precheck(precheckSample())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func verifyScript(t *testing.T) string {
	t.Helper()
	s, err := Verify("gpuworks-system")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const (
	nodeCP     = "gwks-precheck-control-plane"
	nodeWorker = "gwks-precheck-worker"
)

func TestPrecheckEveryNodeOK(t *testing.T) {
	w := newCheckWorld(t)
	w.write("desired", "2")
	w.write("pods", podLines(t, "ready"))
	w.run(t, precheckScript(t))
	if w.exit != 0 {
		t.Fatalf("exit %d\n%s", w.exit, w.out)
	}
	w.wantCode(t, nodeCP, CheckOK, "")
	w.wantCode(t, nodeWorker, CheckOK, "")
	if !strings.Contains(w.out, "2 of 2 nodes OK") {
		t.Errorf("no summary in\n%s", w.out)
	}
	// It made the namespace, so it takes it away again with its DaemonSet.
	if !strings.Contains(w.read("applied.0"), "kind: Namespace") || !strings.Contains(w.read("applied.1"), "kind: DaemonSet") {
		t.Errorf("applied %q then %q", w.read("applied.0"), w.read("applied.1"))
	}
	if !w.called("-n gpuworks-system delete daemonset gpuworks-precheck") || !w.called("delete namespace gpuworks-system") {
		t.Errorf("did not clean up: %q", w.calls())
	}
}

func TestPrecheckLeavesANamespaceItDidNotMake(t *testing.T) {
	w := newCheckWorld(t)
	w.write("ns", "Active")
	w.write("desired", "2")
	w.write("pods", podLines(t, "ready"))
	w.run(t, precheckScript(t))
	if w.exists("applied.1") || strings.Contains(w.read("applied.0"), "kind: Namespace") {
		t.Errorf("applied the namespace: %q", w.read("applied.0"))
	}
	if w.called("delete namespace") || !w.called("delete daemonset gpuworks-precheck") {
		t.Errorf("calls %q", w.calls())
	}
}

func TestPrecheckSaysWhyAnImagePullFailed(t *testing.T) {
	w := newCheckWorld(t)
	w.write("desired", "2")
	w.write("pods", podLines(t, "pull"))
	cause := `Failed to pull image "10.255.255.1:5000/gpuworks/gpuworks-agent:r1": dial tcp 10.255.255.1:5000: connect: no route to host`
	// The kubelet's events, as the API lists them: the cause, then the
	// back-off notes that say nothing of it.
	for _, pod := range []string{"pull-2wljm", "pull-4b6jv"} {
		w.write("events-Pod-"+pod+"-Failed", cause+"\nError: ErrImagePull\nError: ImagePullBackOff\n")
	}
	w.run(t, precheckScript(t))
	if w.exit != 1 {
		t.Errorf("exit %d", w.exit)
	}
	w.wantCode(t, nodeCP, CheckImagePullFailed, "connect: no route to host")
	w.wantCode(t, nodeWorker, CheckImagePullFailed, "connect: no route to host")
	if !strings.Contains(w.out, "0 of 2 nodes OK") {
		t.Errorf("summary\n%s", w.out)
	}
}

// Without the event (expired, or not readable) the pod's own message stands.
func TestPrecheckImagePullFailedWithoutEvents(t *testing.T) {
	w := newCheckWorld(t)
	w.write("desired", "2")
	w.write("pods", podLines(t, "pull"))
	w.run(t, precheckScript(t))
	w.wantCode(t, nodeCP, CheckImagePullFailed, "ErrImagePull: failed to pull and unpack image")
}

func TestPrecheckSaysANodeCannotReachTheServer(t *testing.T) {
	w := newCheckWorld(t)
	w.write("desired", "2")
	w.write("pods", podLines(t, "crash"))
	w.run(t, precheckScript(t))
	w.wantCode(t, nodeCP, CheckServerUnreachable, "gw.example:8800")
	w.wantCode(t, nodeWorker, CheckServerUnreachable, "gw.example:8800")
}

func TestPrecheckSaysWhyAPodIsNotScheduled(t *testing.T) {
	w := newCheckWorld(t)
	w.write("desired", "2")
	w.write("pods", podLines(t, "unsched"))
	w.run(t, precheckScript(t))
	w.wantCode(t, nodeWorker, CheckNotScheduled, "Insufficient memory")
}

func TestPrecheckUndecidedPodsAreStillStarting(t *testing.T) {
	w := newCheckWorld(t)
	w.write("desired", "2")
	w.write("pods", podLines(t, "starting"))
	w.run(t, precheckScript(t))
	w.wantCode(t, nodeWorker, CheckStillStarting, "run the check again")
	if w.exit != 1 {
		t.Errorf("exit %d", w.exit)
	}
}

func TestPrecheckNoNodeSelected(t *testing.T) {
	w := newCheckWorld(t)
	w.write("desired", "0")
	w.run(t, precheckScript(t))
	w.wantCode(t, "-", CheckNoNodeSelected, "matches no node")
	if w.exit != 1 {
		t.Errorf("exit %d", w.exit)
	}
}

// The controller has not counted the nodes yet when the wait ends.
func TestPrecheckDaemonSetNotObserved(t *testing.T) {
	w := newCheckWorld(t)
	w.write("desired", "")
	w.run(t, precheckScript(t))
	w.wantCode(t, "-", CheckStillStarting, "has not counted the nodes")
}

func TestPrecheckSaysWhyPodsWereNotCreated(t *testing.T) {
	w := newCheckWorld(t)
	w.write("desired", "3")
	w.write("pods", podLines(t, "ready"))
	w.write("events-DaemonSet-gpuworks-precheck-FailedCreate", `Error creating: pods "gpuworks-precheck-cq9kb" is forbidden: violates PodSecurity "restricted:latest": host namespaces (hostNetwork=true)`+"\n")
	w.run(t, precheckScript(t))
	w.wantCode(t, "-", CheckPodCreateFailed, "1 of 3 pods were not created: Error creating")
	w.wantCode(t, nodeWorker, CheckOK, "")
	if w.exit != 1 {
		t.Errorf("exit %d", w.exit)
	}
}

func TestPrecheckMissingPullSecretStopsBeforeApplying(t *testing.T) {
	p := precheckSample()
	p.ImagePullSecret = "regcred"
	script, err := Precheck(p)
	if err != nil {
		t.Fatal(err)
	}
	w := newCheckWorld(t)
	w.run(t, script)
	w.wantCode(t, "-", CheckPullSecretMissing, "regcred")
	if w.exists("applied.0") || w.called("delete") {
		t.Errorf("acted after the check failed: %q", w.calls())
	}
	w = newCheckWorld(t)
	w.write("ns", "Active")
	w.write("secret", "")
	w.write("desired", "2")
	w.write("pods", podLines(t, "ready"))
	w.run(t, script)
	if w.exit != 0 {
		t.Errorf("with the Secret: exit %d\n%s", w.exit, w.out)
	}
}

func TestPrecheckApplyFailureCleansUp(t *testing.T) {
	w := newCheckWorld(t)
	w.write("ns", "Active")
	w.write("apply-fails", `Error from server (Forbidden): daemonsets.apps is forbidden: User "dev" cannot create resource "daemonsets"`)
	w.run(t, precheckScript(t))
	w.wantCode(t, "-", CheckApplyFailed, "cannot create resource")
	if w.exit != 1 || !w.called("delete daemonset gpuworks-precheck") {
		t.Errorf("exit %d, calls %q", w.exit, w.calls())
	}
}

func TestCheckKubectlMissingOrFailing(t *testing.T) {
	w := newCheckWorld(t)
	w.noKubectl = true
	w.run(t, precheckScript(t))
	w.wantCode(t, "-", CheckKubectlMissing, "not on PATH")
	if w.exit != 2 {
		t.Errorf("exit %d", w.exit)
	}
	w = newCheckWorld(t)
	w.write("unreachable", "")
	w.run(t, verifyScript(t))
	w.wantCode(t, "-", CheckKubectlFailed, "connection to the server localhost:8080 was refused")
	if w.exit != 2 || w.called("get daemonset") {
		t.Errorf("exit %d, calls %q", w.exit, w.calls())
	}
}

func TestVerifyReportsTheAgentPods(t *testing.T) {
	w := newCheckWorld(t)
	w.write("desired", "2")
	w.write("pods", podLines(t, "ready"))
	w.run(t, verifyScript(t))
	if w.exit != 0 {
		t.Fatalf("exit %d\n%s", w.exit, w.out)
	}
	w.wantCode(t, nodeWorker, CheckOK, "")
	if !w.called("-n gpuworks-system get pods -l app.kubernetes.io/name=gpuworks-agent") {
		t.Errorf("did not read the agents' pods: %q", w.calls())
	}
	// It reads only.
	if w.exists("applied.0") || w.called("delete") {
		t.Errorf("changed the cluster: %q", w.calls())
	}
}

// An agent that exits is no unreachable server: only the pre-check's
// container exits with that meaning.
func TestVerifyACrashingAgentIsAContainerFailure(t *testing.T) {
	w := newCheckWorld(t)
	w.write("desired", "2")
	w.write("pods", podLines(t, "crash"))
	w.run(t, verifyScript(t))
	w.wantCode(t, nodeWorker, CheckContainerFailed, "exit code 42; kubectl -n gpuworks-system logs crash-")
}

func TestVerifyWithoutTheDaemonSet(t *testing.T) {
	w := newCheckWorld(t)
	w.run(t, verifyScript(t))
	w.wantCode(t, "-", CheckDaemonSetMissing, "apply the manifest first")
	if w.exit != 1 {
		t.Errorf("exit %d", w.exit)
	}
}
