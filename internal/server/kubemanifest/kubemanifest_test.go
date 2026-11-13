package kubemanifest

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var update = flag.Bool("update", false, "rewrite testdata/manifest.golden")

func sample() Params {
	return Params{
		ClusterName: "prod", Namespace: "gpuworks-system", ServerURL: "https://gw.example:8800",
		Token: "enroll-token", AgentImage: "registry.example/gpuworks/gpuworks-agent:v1.2.3",
		NodeSelector: map[string]string{"nvidia.com/gpu.present": "true", "gpuworks.ai/pool": "a"},
		Priority:     10000,
	}
}

func docs(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := yaml.NewDecoder(strings.NewReader(s))
	for {
		var d map[string]any
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("manifest is not valid YAML: %v\n%s", err, s)
		}
		out = append(out, d)
	}
}

func dig(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("at %v: not a map: %#v", path, v)
			}
			v = m[k]
		case int:
			l, ok := v.([]any)
			if !ok || k >= len(l) {
				t.Fatalf("at %v: not a list long enough: %#v", path, v)
			}
			v = l[k]
		}
	}
	return v
}

func TestRenderGolden(t *testing.T) {
	got, err := Render(sample())
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "manifest.golden")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal([]byte(got), want) {
		t.Fatalf("manifest differs from %s (run with -update after checking):\n%s", golden, got)
	}
}

func TestRenderedObjects(t *testing.T) {
	out, err := Render(sample())
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]map[string]any{}
	for _, d := range docs(t, out) {
		kinds[d["kind"].(string)] = d
	}
	for _, k := range []string{"Namespace", "ServiceAccount", "Secret", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding", "PriorityClass", "DaemonSet"} {
		if kinds[k] == nil {
			t.Fatalf("manifest lacks a %s", k)
		}
	}
	if got := dig(t, kinds["Namespace"], "metadata", "labels", "pod-security.kubernetes.io/enforce"); got != "privileged" {
		t.Fatalf("namespace PSA label = %v", got)
	}
	ds := kinds["DaemonSet"]
	spec := dig(t, ds, "spec", "template", "spec")
	if dig(t, spec, "hostNetwork") != true || dig(t, spec, "dnsPolicy") != "Default" || spec.(map[string]any)["runtimeClassName"] != nil ||
		dig(t, spec, "nodeSelector", "gpuworks.ai/pool") != "a" || dig(t, spec, "serviceAccountName") != AgentName {
		t.Fatalf("daemonset pod spec = %#v", spec)
	}
	args := dig(t, spec, "containers", 0, "args").([]any)
	want := []string{"run", "--server=https://gw.example:8800", "--state=/opt/gpuworks", "--driver=kube"}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args = %v, want %v", args, want)
		}
	}
	mounts := dig(t, spec, "containers", 0, "volumeMounts").([]any)
	if dig(t, mounts[0], "mountPath") != DefaultStateDir || dig(t, mounts[0], "mountPropagation") != "Bidirectional" {
		t.Fatalf("state mount = %v", mounts[0])
	}
	if dig(t, kinds["Secret"], "stringData", "token") != "enroll-token" {
		t.Fatal("secret does not carry the token")
	}
}

func TestRenderWithoutOptionalFields(t *testing.T) {
	p := sample()
	p.NodeSelector = nil
	out, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs(t, out) {
		if d["kind"] != "DaemonSet" {
			continue
		}
		spec := dig(t, d, "spec", "template", "spec").(map[string]any)
		if _, ok := spec["runtimeClassName"]; ok {
			t.Fatal("runtimeClassName rendered without one")
		}
		if _, ok := spec["nodeSelector"]; ok {
			t.Fatal("nodeSelector rendered without one")
		}
		if _, ok := spec["imagePullSecrets"]; ok {
			t.Fatal("imagePullSecrets rendered without one")
		}
	}
}

func envOf(t *testing.T, spec any) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, e := range dig(t, spec, "containers", 0, "env").([]any) {
		v, _ := dig(t, e, "value").(string)
		out[dig(t, e, "name").(string)] = v
	}
	return out
}

func TestRenderImagePullSecret(t *testing.T) {
	p := sample()
	p.ImagePullSecret = "acr-credential.prod"
	out, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs(t, out) {
		if d["kind"] != "DaemonSet" {
			continue
		}
		spec := dig(t, d, "spec", "template", "spec")
		if dig(t, spec, "imagePullSecrets", 0, "name") != "acr-credential.prod" {
			t.Fatalf("imagePullSecrets = %v", dig(t, spec, "imagePullSecrets"))
		}
		// A named RuntimeClass puts nvidia-smi in the agent's container.
		if _, ok := envOf(t, spec)["GPUWORKS_KUBE_HOST_CLIS"]; ok {
			t.Fatal("agent under a RuntimeClass runs CLIs on the host")
		}
	}
}

// TestRenderCarriesNoVendor: one manifest serves every vendor. The agent
// runs under the default runtime even when the entry names a RuntimeClass
// (that is for NVIDIA instance pods, and the agent passes it on), mounts no
// vendor's directories, and runs vendor CLIs on the host by itself; only
// NVIDIA's variables are set, which act only under NVIDIA's runtime.
func TestRenderCarriesNoVendor(t *testing.T) {
	p := sample()
	p.StateDir, p.ContainerdSocket = "/opt/gpuworks-kube", "/run/k3s/containerd/containerd.sock"
	p.APIServer = "https://10.0.0.5:6443"
	out, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	var ds map[string]any
	for _, d := range docs(t, out) {
		if d["kind"] == "DaemonSet" {
			ds = d
		}
	}
	spec := dig(t, ds, "spec", "template", "spec").(map[string]any)
	if _, ok := spec["runtimeClassName"]; ok {
		t.Fatal("the agent must run under the node's default runtime")
	}
	c := dig(t, spec, "containers", 0).(map[string]any)
	if dig(t, c, "args", 2) != "--state=/opt/gpuworks-kube" {
		t.Fatalf("args = %v", c["args"])
	}
	env := map[string]any{}
	for _, e := range c["env"].([]any) {
		env[dig(t, e, "name").(string)] = e.(map[string]any)["value"]
	}
	if _, ok := env["GPUWORKS_KUBE_HOST_CLIS"]; ok {
		t.Fatal("the manifest still decides where vendor CLIs run")
	}
	if env["NVIDIA_VISIBLE_DEVICES"] != "all" || env["NVIDIA_DRIVER_CAPABILITIES"] != "compute,utility" ||
		env["GPUWORKS_KUBE_CONTAINERD_SOCKET"] != "/run/k3s/containerd/containerd.sock" ||
		env["KUBERNETES_SERVICE_HOST"] != "10.0.0.5" || env["KUBERNETES_SERVICE_PORT"] != "6443" {
		t.Fatalf("env = %v", env)
	}
	mounts := map[string]map[string]any{}
	for _, m := range c["volumeMounts"].([]any) {
		mounts[dig(t, m, "mountPath").(string)] = m.(map[string]any)
	}
	if len(mounts) != 3 || mounts["/opt/gpuworks-kube"] == nil || mounts[DevicePluginDir] == nil || mounts[PodResourcesDir] == nil {
		t.Fatalf("mounts = %v, want only the state, device plugin and pod resources directories", mounts)
	}
	if len(spec["volumes"].([]any)) != 3 {
		t.Fatalf("volumes = %v", spec["volumes"])
	}
}

func TestValuesCannotChangeTheStructure(t *testing.T) {
	p := sample()
	p.ClusterName = "x\n---\nkind: Pod"
	p.Token = "a\"\nb: c"
	out, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	ds := docs(t, out)
	if len(ds) != 9 {
		t.Fatalf("documents = %d, want 9", len(ds))
	}
	for _, d := range ds {
		if d["kind"] == "Secret" && dig(t, d, "stringData", "token") != p.Token {
			t.Fatalf("token round trip = %q", dig(t, d, "stringData", "token"))
		}
	}
}

func TestValidate(t *testing.T) {
	for name, mut := range map[string]func(*Params){
		"namespace":    func(p *Params) { p.Namespace = "Bad_NS" },
		"server":       func(p *Params) { p.ServerURL = "gw.example" },
		"token":        func(p *Params) { p.Token = " " },
		"image":        func(p *Params) { p.AgentImage = "" },
		"pullsecret":   func(p *Params) { p.ImagePullSecret = "Reg Cred" },
		"pullsecret-l": func(p *Params) { p.ImagePullSecret = strings.Repeat("a", 254) },
		"socket":       func(p *Params) { p.ContainerdSocket = "run/containerd.sock" },
		"state":        func(p *Params) { p.StateDir = "/" },
		"state-rel":    func(p *Params) { p.StateDir = "opt/x" },
		"state-dirty":  func(p *Params) { p.StateDir = "/opt/../x" },
		"apiserver":    func(p *Params) { p.APIServer = "http://10.0.0.5:6443" },
		"apiserver-np": func(p *Params) { p.APIServer = "10.0.0.5" },
	} {
		p := sample()
		mut(&p)
		if _, err := Render(p); err == nil {
			t.Fatalf("%s: invalid parameters rendered", name)
		}
	}
}

func TestParseNodeSelector(t *testing.T) {
	got, err := ParseNodeSelector(" nvidia.com/gpu.present=true , pool=a ,")
	if err != nil || len(got) != 2 || got["nvidia.com/gpu.present"] != "true" || got["pool"] != "a" {
		t.Fatalf("ParseNodeSelector = %v, %v", got, err)
	}
	if got, err := ParseNodeSelector(""); err != nil || len(got) != 0 {
		t.Fatalf("empty = %v, %v", got, err)
	}
	for _, bad := range []string{"novalue", "a=b c", "=x"} {
		if _, err := ParseNodeSelector(bad); err == nil {
			t.Fatalf("ParseNodeSelector(%q) accepted", bad)
		}
	}
}

func TestParseAPIServer(t *testing.T) {
	for in, want := range map[string][2]string{
		"https://10.0.0.5:6443":     {"10.0.0.5", "6443"},
		"https://api.example/":      {"api.example", "443"},
		"10.0.0.5:6443":             {"10.0.0.5", "6443"},
		"[fd00::1]:6443":            {"fd00::1", "6443"},
		"https://[fd00::1]:6443":    {"fd00::1", "6443"},
		" https://k8s.local:16443 ": {"k8s.local", "16443"},
	} {
		host, port, err := ParseAPIServer(in)
		if err != nil || host != want[0] || port != want[1] {
			t.Errorf("ParseAPIServer(%q) = %q %q %v", in, host, port, err)
		}
	}
	for _, bad := range []string{"http://10.0.0.5:6443", "10.0.0.5", "host:0", "host:99999", ":6443", "https://a b:1"} {
		if _, _, err := ParseAPIServer(bad); err == nil {
			t.Errorf("ParseAPIServer(%q) accepted", bad)
		}
	}
}

// TestRenderWithoutAPIServer: the Service the kubelet injects is used.
func TestRenderWithoutAPIServer(t *testing.T) {
	out, err := Render(sample())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "KUBERNETES_SERVICE_HOST") {
		t.Fatal("API server override rendered without one")
	}
}

func poolSample() Params {
	p := sample()
	p.NodeSelector = nil
	p.PoolLabel = "alibabacloud.com/nodepool-id"
	p.PoolIDs = []string{"np-l20", "np-a10", "np-h20"} // deliberately unsorted: the manifest must not depend on the order
	return p
}

// An entry made from a cloud cluster selects its node pools with a required
// nodeAffinity over the pool-id label; the manifest is the same whatever order
// the pools were stored in.
func TestRenderPoolsGolden(t *testing.T) {
	got, err := Render(poolSample())
	if err != nil {
		t.Fatal(err)
	}
	reordered := poolSample()
	reordered.PoolIDs = []string{"np-h20", "np-l20", "np-a10"}
	again, err := Render(reordered)
	if err != nil || again != got {
		t.Fatalf("the manifest depends on the order of the pools (err %v)", err)
	}
	golden := filepath.Join("testdata", "manifest_pools.golden")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal([]byte(got), want) {
		t.Fatalf("manifest differs from %s (run with -update after checking):\n%s", golden, got)
	}
}

func TestRenderPoolsSelectWithRequiredNodeAffinityBesideTheSelector(t *testing.T) {
	spec := func(p Params) map[string]any {
		out, err := Render(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range docs(t, out) {
			if d["kind"] == "DaemonSet" {
				return dig(t, d, "spec", "template", "spec").(map[string]any)
			}
		}
		t.Fatal("no DaemonSet")
		return nil
	}
	pools := spec(poolSample())
	if _, ok := pools["nodeSelector"]; ok {
		t.Fatal("an entry of pools renders a nodeSelector it does not have")
	}
	term := dig(t, pools, "affinity", "nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms", 0, "matchExpressions", 0)
	if dig(t, term, "key") != "alibabacloud.com/nodepool-id" || dig(t, term, "operator") != "In" {
		t.Fatalf("expression = %#v", term)
	}
	values := dig(t, term, "values").([]any)
	if len(values) != 3 || values[0] != "np-a10" || values[1] != "np-h20" || values[2] != "np-l20" {
		t.Fatalf("values = %v, want the pools sorted", values)
	}
	if n := len(dig(t, pools, "affinity", "nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms").([]any)); n != 1 {
		t.Fatalf("%d terms; one term means every expression must hold", n)
	}

	both := poolSample()
	both.NodeSelector = map[string]string{"gpuworks.ai/pool": "a"}
	if got := spec(both); dig(t, got, "nodeSelector", "gpuworks.ai/pool") != "a" || dig(t, got, "affinity", "nodeAffinity") == nil {
		t.Fatalf("a selector and pools both apply: %#v", got)
	}
	if _, ok := spec(sample())["affinity"]; ok {
		t.Fatal("an entry by hand got an affinity")
	}
}

func TestRenderRefusesAPoolSelectionItCannotRenderSafely(t *testing.T) {
	for name, mutate := range map[string]func(*Params){
		"pools without a label":              func(p *Params) { p.PoolLabel = "" },
		"a label without pools":              func(p *Params) { p.PoolIDs = nil },
		"a label that is no key":             func(p *Params) { p.PoolLabel = "not a key" },
		"a prefix that is no DNS name":       func(p *Params) { p.PoolLabel = "Bad_Prefix/np-id" },
		"a pool id with a space":             func(p *Params) { p.PoolIDs = []string{"np 1"} },
		"a pool id that closes the document": func(p *Params) { p.PoolIDs = []string{"x\n---\nkind: Pod"} },
		"a pool id past 63 bytes":            func(p *Params) { p.PoolIDs = []string{strings.Repeat("a", 64)} },
	} {
		p := poolSample()
		mutate(&p)
		if out, err := Render(p); err == nil {
			t.Errorf("%s was rendered:\n%s", name, out)
		}
	}
	for _, ok := range []string{"alibabacloud.com/nodepool-id", "doks.digitalocean.com/node-pool-id", "plain-key"} {
		p := poolSample()
		p.PoolLabel = ok
		if _, err := Render(p); err != nil {
			t.Errorf("label %q refused: %v", ok, err)
		}
	}
}

// The agent reads other namespaces' pods on its own node (the allocation
// annotations HAMi and ACK GPU sharing write are the only record of who holds
// how much of a card), and may create, delete and patch pods only in its own
// namespace: the cluster-wide grant is read-only.
func TestRenderedRBACLetsTheAgentReadPodsClusterWideButWriteOnlyItsOwn(t *testing.T) {
	out, err := Render(sample())
	if err != nil {
		t.Fatal(err)
	}
	verbs := func(kind string) map[string][]string {
		got := map[string][]string{}
		for _, d := range docs(t, out) {
			if d["kind"] != kind {
				continue
			}
			for _, rule := range d["rules"].([]any) {
				r := rule.(map[string]any)
				for _, res := range r["resources"].([]any) {
					for _, v := range r["verbs"].([]any) {
						got[res.(string)] = append(got[res.(string)], v.(string))
					}
				}
			}
		}
		return got
	}
	cluster, role := verbs("ClusterRole"), verbs("Role")
	if strings.Join(cluster["pods"], ",") != "get,list" {
		t.Fatalf("cluster-wide pod verbs = %v, want get,list only", cluster["pods"])
	}
	if got := strings.Join(role["pods"], ","); got != "get,list,watch,create,delete,patch" {
		t.Fatalf("namespaced pod verbs = %s", got)
	}
}

// The hold and instance pods' PriorityClass is named per namespace, carries
// the entry's priority, and does not preempt unless the entry allows it; the
// agent learns its name from the DaemonSet.
func TestPriorityClassFollowsTheEntry(t *testing.T) {
	find := func(ds []map[string]any, kind string) map[string]any {
		for _, d := range ds {
			if d["kind"] == kind {
				return d
			}
		}
		t.Fatalf("no %s", kind)
		return nil
	}
	p := sample()
	p.Namespace, p.Priority = "gw-ascend", 42
	out, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	pc := find(docs(t, out), "PriorityClass")
	if pc["metadata"].(map[string]any)["name"] != "gpuworks-inference-gw-ascend" || pc["value"] != 42 || pc["preemptionPolicy"] != "Never" {
		t.Fatalf("PriorityClass = %v", pc)
	}
	if !strings.Contains(out, "- name: GPUWORKS_KUBE_PRIORITY_CLASS\n              value: \"gpuworks-inference-gw-ascend\"") {
		t.Fatal("the agent is not told the PriorityClass")
	}
	p.AllowPreemption = true
	out, err = Render(p)
	if err != nil {
		t.Fatal(err)
	}
	if pc := find(docs(t, out), "PriorityClass"); pc["preemptionPolicy"] != nil {
		t.Fatalf("preemption allowed, but preemptionPolicy = %v", pc["preemptionPolicy"])
	}
	for _, bad := range []int{-1, PriorityMax + 1} {
		p.Priority = bad
		if _, err := Render(p); err == nil {
			t.Fatalf("priority %d rendered", bad)
		}
	}
}

// The removal commands name exactly the objects the manifest makes outside
// the namespace, and the namespace last.
func TestRemoveCommandsNameTheManifestsClusterWideObjects(t *testing.T) {
	p := sample()
	p.Namespace = "gpuworks-a"
	out, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	cmds := RemoveCommands(p.Namespace)
	want := []string{
		"kubectl -n gpuworks-a delete daemonset gpuworks-agent --ignore-not-found --wait",
		"kubectl delete clusterrolebinding gpuworks-agent-gpuworks-a --ignore-not-found",
		"kubectl delete clusterrole gpuworks-agent-gpuworks-a --ignore-not-found",
		"kubectl delete priorityclass gpuworks-inference-gpuworks-a --ignore-not-found",
		"kubectl delete namespace gpuworks-a --ignore-not-found",
	}
	if strings.Join(cmds, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands:\n%s", strings.Join(cmds, "\n"))
	}
	for _, name := range []string{`name: "gpuworks-agent-gpuworks-a"`, `name: "gpuworks-inference-gpuworks-a"`, `name: gpuworks-agent`} {
		if !strings.Contains(out, name) {
			t.Fatalf("manifest has no %s", name)
		}
	}
	if q := quoteShell("a b'c"); q != `'a b'\''c'` {
		t.Fatalf("quote: %s", q)
	}
}
