package kubemanifest

import (
	"bytes"
	_ "embed"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"text/template"
)

// The check script (check.sh) runs where kubectl reaches a cluster: before
// connecting it, as a pre-check that starts the agent image on every node the
// agents would run on, and after applying the manifest, to say how each
// node's agent pod stands. Each line it prints is a node, a CheckCode and the
// detail Kubernetes gave; the console explains the codes.
//
//go:embed check.sh
var checkTemplate string

const checkParamsMarker = "# @@PARAMS@@"

// PrecheckName names the pre-check's DaemonSet and the label its pods carry.
const PrecheckName = "gpuworks-precheck"

// precheckUnreachableExit is what the pre-check's container exits with when
// its node cannot open a TCP connection to the server.
const precheckUnreachableExit = "42"

// checkWaitSeconds is how long the script waits for the pods by default.
const checkWaitSeconds = 180

// CheckCode is how the check script says a node or the whole check stands.
type CheckCode string

// The codes the check script prints.
const (
	// CheckOK: the pre-check pod pulled the image and reached the server, or
	// the agent pod runs.
	CheckOK CheckCode = "OK"
	// CheckImagePullFailed: the node could not pull the image; the detail is
	// the pull error the kubelet reported.
	CheckImagePullFailed CheckCode = "IMAGE_PULL_FAILED"
	// CheckServerUnreachable: the pre-check pod could not open a TCP
	// connection to the server address from its node.
	CheckServerUnreachable CheckCode = "SERVER_UNREACHABLE"
	// CheckNotScheduled: the scheduler did not place the pod; the detail says why.
	CheckNotScheduled CheckCode = "NOT_SCHEDULED"
	// CheckPodCreateFailed: the DaemonSet could not create some of its pods
	// (Pod Security admission, a quota, …).
	CheckPodCreateFailed CheckCode = "POD_CREATE_FAILED"
	// CheckContainerFailed: the container exited or could not start.
	CheckContainerFailed CheckCode = "CONTAINER_FAILED"
	// CheckStillStarting: undecided when the wait ended, mostly a slow pull.
	CheckStillStarting CheckCode = "STILL_STARTING"
	// CheckNoNodeSelected: the node selection matches no node.
	CheckNoNodeSelected CheckCode = "NO_NODE_SELECTED"
	// CheckPullSecretMissing: the image pull Secret is not in the namespace.
	CheckPullSecretMissing CheckCode = "PULL_SECRET_MISSING"
	// CheckDaemonSetMissing: the agents' DaemonSet is not there to verify.
	CheckDaemonSetMissing CheckCode = "DAEMONSET_MISSING"
	// CheckApplyFailed: kubectl apply of the pre-check objects failed.
	CheckApplyFailed CheckCode = "APPLY_FAILED"
	// CheckKubectlMissing: kubectl is not on PATH.
	CheckKubectlMissing CheckCode = "KUBECTL_MISSING"
	// CheckKubectlFailed: kubectl cannot reach the cluster.
	CheckKubectlFailed CheckCode = "KUBECTL_FAILED"
)

// PrecheckCodes are the codes the pre-check prints, in the order the console
// explains them.
var PrecheckCodes = []CheckCode{
	CheckOK, CheckImagePullFailed, CheckServerUnreachable, CheckPullSecretMissing, CheckNoNodeSelected,
	CheckNotScheduled, CheckPodCreateFailed, CheckContainerFailed, CheckStillStarting,
	CheckApplyFailed, CheckKubectlMissing, CheckKubectlFailed,
}

// VerifyCodes are the codes the check after an apply prints.
var VerifyCodes = []CheckCode{
	CheckOK, CheckImagePullFailed, CheckNoNodeSelected, CheckNotScheduled, CheckPodCreateFailed,
	CheckContainerFailed, CheckStillStarting, CheckDaemonSetMissing, CheckKubectlMissing, CheckKubectlFailed,
}

// Precheck is the pre-check script for the parameters a cluster would be
// connected with. It needs no token: nothing in it joins the cluster, and the
// DaemonSet it starts runs no agent. The pool selection of a cloud entry is
// not part of it.
func Precheck(p Params) (string, error) {
	probe := p
	probe.Token = "x"
	if err := probe.Validate(); err != nil {
		return "", err
	}
	if len(p.PoolIDs) > 0 {
		return "", fmt.Errorf("a pre-check selects nodes by node selector, not by node pool")
	}
	host, port, err := serverHostPort(p.ServerURL)
	if err != nil {
		return "", err
	}
	var ns, ds bytes.Buffer
	if err := precheckNamespace.Execute(&ns, map[string]any{"P": p}); err != nil {
		return "", err
	}
	if err := precheckDaemonSet.Execute(&ds, map[string]any{
		"P": p, "Name": PrecheckName, "Selector": sortedSelector(p.NodeSelector),
		"Host": host, "Port": port, "Exit": precheckUnreachableExit,
	}); err != nil {
		return "", err
	}
	return renderCheck(map[string]string{
		"MODE": "precheck", "NS": p.Namespace, "DS": PrecheckName, "IMAGE": p.AgentImage,
		"SERVER": net.JoinHostPort(host, port), "PULL_SECRET": p.ImagePullSecret,
		"UNREACHABLE_EXIT": precheckUnreachableExit,
	}, map[string]string{"NAMESPACE_YAML": ns.String(), "DAEMONSET_YAML": ds.String()}), nil
}

// Verify is the script that says how the agents of an applied manifest in
// namespace stand on each node. It changes nothing in the cluster.
func Verify(namespace string) (string, error) {
	if len(namespace) > 63 || !dnsLabel.MatchString(namespace) {
		return "", fmt.Errorf("namespace %q is not a DNS label", namespace)
	}
	return renderCheck(map[string]string{
		"MODE": "verify", "NS": namespace, "DS": AgentName, "IMAGE": "", "SERVER": "",
		"PULL_SECRET": "", "UNREACHABLE_EXIT": "",
	}, nil), nil
}

// renderCheck fills check.sh's parameter block: shell-quoted values, and
// documents read from quoted here-documents (validated parameters render no
// line that could end one early).
func renderCheck(vars, docs map[string]string) string {
	var b strings.Builder
	for _, k := range []string{"MODE", "NS", "DS", "IMAGE", "SERVER", "PULL_SECRET", "UNREACHABLE_EXIT"} {
		fmt.Fprintf(&b, "%s=%s\n", k, shellWord(vars[k]))
	}
	fmt.Fprintf(&b, "WAIT=%d\n", checkWaitSeconds)
	for _, k := range []string{"NAMESPACE_YAML", "DAEMONSET_YAML"} {
		if docs[k] == "" {
			fmt.Fprintf(&b, "%s=''\n", k)
			continue
		}
		fmt.Fprintf(&b, "IFS= read -r -d '' %s <<'GPUWORKS_YAML' || true\n%sGPUWORKS_YAML\n", k, docs[k])
	}
	return strings.Replace(checkTemplate, checkParamsMarker, strings.TrimSuffix(b.String(), "\n"), 1)
}

// shellWord quotes s as one POSIX shell word.
func shellWord(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ServerAddress is the host:port the agents dial for serverURL, which the
// pre-check tests each node against.
func ServerAddress(serverURL string) (string, error) {
	host, port, err := serverHostPort(serverURL)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, port), nil
}

// serverHostPort is the host and port the agents dial for serverURL.
func serverHostPort(serverURL string) (string, string, error) {
	u, err := url.Parse(serverURL)
	if err != nil || u.Hostname() == "" {
		return "", "", fmt.Errorf("server URL %q has no host", serverURL)
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	return u.Hostname(), port, nil
}

// selectorEntry is one key and value, of a node selector or an environment.
type selectorEntry struct{ K, V string }

func sortedSelector(sel map[string]string) []selectorEntry {
	keys := make([]string, 0, len(sel))
	for k := range sel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]selectorEntry, 0, len(keys))
	for _, k := range keys {
		out = append(out, selectorEntry{k, sel[k]})
	}
	return out
}

// precheckProbe is the pre-check container's command: ready once its node
// opened a TCP connection to the server, the unreachable exit code otherwise.
// It uses only what the agent image carries (bash, coreutils' timeout).
const precheckProbe = `rm -f /tmp/reachable; ` +
	`if timeout 10 bash -c 'exec 3<>"/dev/tcp/$GPUWORKS_CHECK_HOST/$GPUWORKS_CHECK_PORT"' 2>/dev/null; then ` +
	`touch /tmp/reachable; echo "reached $GPUWORKS_CHECK_HOST:$GPUWORKS_CHECK_PORT"; exec sleep infinity; fi; ` +
	`echo "cannot reach $GPUWORKS_CHECK_HOST:$GPUWORKS_CHECK_PORT" >&2; exit $GPUWORKS_CHECK_EXIT`

var checkFuncs = template.FuncMap{"q": q, "probe": func() string { return q(precheckProbe) }}

// precheckNamespace is created, with the manifest's labels, only when the
// namespace is not there yet; the script removes it again afterwards.
var precheckNamespace = template.Must(template.New("ns").Funcs(checkFuncs).Parse(`apiVersion: v1
kind: Namespace
metadata:
  name: {{q .P.Namespace}}
  labels:
    pod-security.kubernetes.io/enforce: privileged
    app.kubernetes.io/managed-by: gpuworks
`))

// precheckDaemonSet places pods as the agents' DaemonSet does (node
// selection, tolerations, host network and resolver, the image and its pull
// Secret) but runs nothing of the agent and needs no privilege.
var precheckDaemonSet = template.Must(template.New("ds").Funcs(checkFuncs).Parse(`apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: {{.Name}}
  namespace: {{q .P.Namespace}}
  labels:
    app.kubernetes.io/name: {{.Name}}
    app.kubernetes.io/managed-by: gpuworks
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: {{.Name}}
  template:
    metadata:
      labels:
        app.kubernetes.io/name: {{.Name}}
        app.kubernetes.io/managed-by: gpuworks
    spec:
      automountServiceAccountToken: false
{{- if .P.ImagePullSecret}}
      imagePullSecrets:
        - name: {{q .P.ImagePullSecret}}
{{- end}}
      hostNetwork: true
      dnsPolicy: Default
{{- if .Selector}}
      nodeSelector:
{{- range .Selector}}
        {{q .K}}: {{q .V}}
{{- end}}
{{- end}}
      tolerations:
        - operator: Exists
      terminationGracePeriodSeconds: 0
      containers:
        - name: check
          image: {{q .P.AgentImage}}
          imagePullPolicy: IfNotPresent
          command: ["/bin/bash", "-c", {{probe}}]
          env:
            - name: GPUWORKS_CHECK_HOST
              value: {{q .Host}}
            - name: GPUWORKS_CHECK_PORT
              value: {{q .Port}}
            - name: GPUWORKS_CHECK_EXIT
              value: {{q .Exit}}
          readinessProbe:
            exec:
              command: ["test", "-f", "/tmp/reachable"]
            periodSeconds: 2
          resources:
            requests:
              cpu: 10m
              memory: 16Mi
            limits:
              memory: 64Mi
`))
