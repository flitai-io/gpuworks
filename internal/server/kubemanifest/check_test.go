package kubemanifest

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"text/template"
)

func precheckSample() Params {
	return Params{
		Namespace: "gpuworks-system", ServerURL: "https://gw.example:8800",
		AgentImage:   "registry.example/gpuworks/gpuworks-agent:r1",
		NodeSelector: map[string]string{"nvidia.com/gpu.present": "true", "gpuworks.ai/pool": "a"},
	}
}

// heredoc is the document the script reads into variable name.
func heredoc(t *testing.T, script, name string) string {
	t.Helper()
	start := "IFS= read -r -d '' " + name + " <<'GPUWORKS_YAML' || true\n"
	i := strings.Index(script, start)
	if i < 0 {
		t.Fatalf("no %s in the script", name)
	}
	rest := script[i+len(start):]
	j := strings.Index(rest, "\nGPUWORKS_YAML\n")
	if j < 0 {
		t.Fatalf("%s is not closed", name)
	}
	return rest[:j+1]
}

func TestPrecheckPlacesPodsAsTheAgentsDo(t *testing.T) {
	p := precheckSample()
	p.ImagePullSecret = "regcred"
	script, err := Precheck(p)
	if err != nil {
		t.Fatal(err)
	}
	ds := docs(t, heredoc(t, script, "DAEMONSET_YAML"))
	if len(ds) != 1 || ds[0]["kind"] != "DaemonSet" {
		t.Fatalf("want one DaemonSet, got %v", ds)
	}
	spec := dig(t, ds[0], "spec", "template", "spec").(map[string]any)
	manifest := docs(t, mustRender(t, withToken(p)))
	agent := dig(t, manifest[len(manifest)-1], "spec", "template", "spec").(map[string]any)
	// What decides where a pod lands, how it pulls and how it resolves
	// names is the agents' own.
	for _, k := range []string{"nodeSelector", "tolerations", "hostNetwork", "dnsPolicy", "imagePullSecrets"} {
		if !jsonEqual(spec[k], agent[k]) {
			t.Errorf("%s: pre-check %v, agents %v", k, spec[k], agent[k])
		}
	}
	c := dig(t, spec, "containers", 0).(map[string]any)
	ac := dig(t, agent, "containers", 0).(map[string]any)
	if c["image"] != ac["image"] || c["imagePullPolicy"] != ac["imagePullPolicy"] {
		t.Errorf("image %v %v, agents %v %v", c["image"], c["imagePullPolicy"], ac["image"], ac["imagePullPolicy"])
	}
	env := map[string]any{}
	for _, e := range c["env"].([]any) {
		env[e.(map[string]any)["name"].(string)] = e.(map[string]any)["value"]
	}
	if env["GPUWORKS_CHECK_HOST"] != "gw.example" || env["GPUWORKS_CHECK_PORT"] != "8800" || env["GPUWORKS_CHECK_EXIT"] != precheckUnreachableExit {
		t.Errorf("env %v", env)
	}
	// Nothing of the agent and no privilege: no service account token, no
	// security context, no host PID, no volumes.
	if spec["automountServiceAccountToken"] != false || spec["hostPID"] != nil || spec["volumes"] != nil || c["securityContext"] != nil {
		t.Errorf("pre-check pod asks for more than it needs: %v", spec)
	}
	ns := docs(t, heredoc(t, script, "NAMESPACE_YAML"))
	if !jsonEqual(dig(t, ns[0], "metadata", "labels"), dig(t, manifest[0], "metadata", "labels")) {
		t.Errorf("namespace labels differ from the manifest's: %v", ns[0])
	}
}

func TestPrecheckServerPortDefaultsByScheme(t *testing.T) {
	for url, want := range map[string]string{"http://gw.example": "gw.example:80", "https://gw.example/": "gw.example:443", "https://[fd00::1]:9443": "[fd00::1]:9443"} {
		p := precheckSample()
		p.ServerURL = url
		script, err := Precheck(p)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(script, "\nSERVER='"+want+"'\n") {
			t.Errorf("%s: want SERVER %s", url, want)
		}
	}
}

func TestPrecheckRefusesWhatTheManifestRefuses(t *testing.T) {
	for name, mut := range map[string]func(*Params){
		"namespace":  func(p *Params) { p.Namespace = "Bad_NS" },
		"server":     func(p *Params) { p.ServerURL = "gw.example:8800" },
		"no host":    func(p *Params) { p.ServerURL = "https://" },
		"image":      func(p *Params) { p.AgentImage = " " },
		"secret":     func(p *Params) { p.ImagePullSecret = "Not A Secret" },
		"node pools": func(p *Params) { p.PoolLabel, p.PoolIDs = "pool", []string{"a"} },
	} {
		p := precheckSample()
		mut(&p)
		if _, err := Precheck(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Verify("Bad_NS"); err == nil {
		t.Error("Verify accepted a bad namespace")
	}
}

// An image is only checked to be non-empty; no value can break out of its
// shell word or its YAML scalar.
func TestCheckScriptQuotesEveryValue(t *testing.T) {
	p := precheckSample()
	p.AgentImage = "a'b\"c$(touch x)\nGPUWORKS_YAML\n"
	script, err := Precheck(p)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-n", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
	ds := docs(t, heredoc(t, script, "DAEMONSET_YAML"))
	if got := dig(t, ds[0], "spec", "template", "spec", "containers", 0, "image"); got != p.AgentImage {
		t.Errorf("image in YAML %q", got)
	}
	w := newCheckWorld(t)
	w.write("desired", "0")
	w.run(t, script)
	if got := w.read("applied.1"); !strings.Contains(got, "kind: DaemonSet") || !strings.Contains(got, `image: "a'b\"c$(touch x)\nGPUWORKS_YAML\n"`) {
		t.Errorf("DaemonSet applied %q", got)
	}
}

// The script prints exactly the codes the console explains.
func TestCheckScriptPrintsTheListedCodes(t *testing.T) {
	printed := map[string]bool{}
	for _, m := range regexp.MustCompile(`say (?:-|"\$node") ([A-Z_]+)`).FindAllStringSubmatch(checkTemplate, -1) {
		printed[m[1]] = true
	}
	listed := map[string]bool{}
	for _, c := range append(slices.Clone(PrecheckCodes), VerifyCodes...) {
		listed[string(c)] = true
	}
	for c := range printed {
		if !listed[c] {
			t.Errorf("the script prints %s, which no code list has", c)
		}
	}
	for c := range listed {
		if !printed[c] {
			t.Errorf("%s is listed but never printed", c)
		}
	}
	for _, c := range []CheckCode{CheckServerUnreachable, CheckPullSecretMissing, CheckApplyFailed} {
		if slices.Contains(VerifyCodes, c) {
			t.Errorf("verify never prints %s", c)
		}
	}
	if slices.Contains(PrecheckCodes, CheckDaemonSetMissing) {
		t.Error("the pre-check never prints DAEMONSET_MISSING")
	}
}

// podTemplate is the go-template the script hands kubectl for its pods.
func podTemplate(t *testing.T) *template.Template {
	t.Helper()
	m := regexp.MustCompile(`(?m)^POD_TEMPLATE='(.*)'$`).FindStringSubmatch(checkTemplate)
	if m == nil {
		t.Fatal("no POD_TEMPLATE in check.sh")
	}
	// As kubectl runs it with --allow-missing-template-keys=true, over the
	// objects decoded from JSON.
	return template.Must(template.New("output").Option("missingkey=default").Parse(m[1]))
}

// podLines is what `kubectl get pods -o go-template=$POD_TEMPLATE` prints
// for the pod lists in testdata/check (captured from a kind cluster).
func podLines(t *testing.T, fixtures ...string) string {
	t.Helper()
	tpl := podTemplate(t)
	var b bytes.Buffer
	for _, f := range fixtures {
		raw, err := os.ReadFile(filepath.Join("testdata", "check", "pods-"+f+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var list any
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatal(err)
		}
		if err := tpl.Execute(&b, list); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	return b.String()
}

func TestPodTemplateReadsWhatTheScriptJudgesBy(t *testing.T) {
	type row struct{ node, ready, reason, code, sched string }
	for fixture, want := range map[string]row{
		"ready":    {"gwks-precheck-worker", "ready", "", "", ""},
		"pull":     {"gwks-precheck-control-plane", "", "ErrImagePull", "", ""},
		"crash":    {"gwks-precheck-worker", "", "CrashLoopBackOff", "42", ""},
		"unsched":  {"gwks-precheck-worker", "", "", "", "0/2 nodes are available: 1 Insufficient memory"},
		"starting": {"gwks-precheck-worker", "", "", "", ""},
	} {
		lines := strings.Split(strings.TrimSuffix(podLines(t, fixture), "\n"), "\n")
		if len(lines) != 2 {
			t.Fatalf("%s: want two pods, got %q", fixture, lines)
		}
		var found bool
		for _, l := range lines {
			f := strings.Split(l, "\x1f")
			if len(f) != 7 {
				t.Fatalf("%s: %d fields in %q", fixture, len(f), l)
			}
			if f[1] != want.node {
				continue
			}
			found = true
			if f[2] != want.ready || f[3] != want.reason || f[4] != want.code || !strings.HasPrefix(f[5], want.sched) {
				t.Errorf("%s: got %q, want %+v", fixture, f, want)
			}
		}
		if !found {
			t.Errorf("%s: no pod on %s in %q", fixture, want.node, lines)
		}
	}
}

// The node an unscheduled pod is meant for comes from the affinity the
// DaemonSet controller gives it.
func TestPodTemplateNamesTheNodeOfAnUnscheduledPod(t *testing.T) {
	nodes := []string{}
	for _, l := range strings.Split(strings.TrimSpace(podLines(t, "unsched")), "\n") {
		nodes = append(nodes, strings.Split(l, "\x1f")[1])
	}
	slices.Sort(nodes)
	if !slices.Equal(nodes, []string{"gwks-precheck-control-plane", "gwks-precheck-worker"}) {
		t.Errorf("nodes %v", nodes)
	}
}

func mustRender(t *testing.T, p Params) string {
	t.Helper()
	s, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func withToken(p Params) Params { p.Token = "t"; p.ClusterName = "c"; return p }

func jsonEqual(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

// The pre-check's container runs only what the agent image carries: bash and
// coreutils' timeout, both part of every Debian base. A base without them
// would turn every node's pre-check into CONTAINER_FAILED.
func TestAgentImageCarriesWhatThePrecheckRuns(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "build", "agent-image", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^ARG BASE_IMAGE=debian:`).Match(raw) {
		t.Error("the agent image is no longer Debian-based: check that it has bash and timeout, which the pre-check runs")
	}
}
