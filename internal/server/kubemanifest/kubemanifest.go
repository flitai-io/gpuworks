// Package kubemanifest renders the manifest a Kubernetes cluster applies to
// join: one DaemonSet whose agents trade the cluster's enrollment token for
// node tokens, plus the namespace, identity and permissions they need. It is
// a pure function of its parameters, with no knowledge of the store or of
// how the manifest is served.
package kubemanifest

import (
	"bytes"
	"fmt"
	"net"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
)

// Names every rendered object uses. The agent finds its own DaemonSet by
// them; the instance pods' PriorityClass, a cluster-wide object, is named
// per namespace (PriorityClass) and handed to the agent.
const (
	AgentName           = "gpuworks-agent"
	EnrollSecret        = "gpuworks-enroll"
	PriorityClassPrefix = "gpuworks-inference"
	// DefaultStateDir is where the agent keeps its state on the host unless
	// the cluster names another directory (a host that already runs an agent
	// of its own keeps that one's). It is mounted at the same path inside the
	// agent so every path the agent hands an instance pod is a host path too.
	DefaultStateDir = "/opt/gpuworks"
	// PodResourcesDir is kubelet's PodResources socket directory.
	PodResourcesDir = "/var/lib/kubelet/pod-resources"
	// DevicePluginDir is the kubelet's device plugin directory: the agent
	// registers a plugin there that hands instance pods their cards' device
	// nodes.
	DevicePluginDir = "/var/lib/kubelet/device-plugins"
)

// Params are a cluster's manifest parameters.
type Params struct {
	ClusterName string
	Namespace   string
	ServerURL   string
	Token       string
	AgentImage  string
	// NodeSelector narrows the nodes the agents run on; every agent finds
	// out for itself which accelerator its node has.
	NodeSelector map[string]string
	// PoolLabel and PoolIDs select the nodes of the node pools an entry made
	// from a cloud cluster holds: the DaemonSet requires the node's PoolLabel
	// label to be one of PoolIDs (a nodeAffinity "In"), beside NodeSelector.
	// Both or neither.
	PoolLabel string
	PoolIDs   []string
	// ImagePullSecret names a docker-registry Secret in Namespace the agent
	// image is pulled with; the agents hand it on to the pods they start
	// from that image.
	ImagePullSecret string
	// ContainerdSocket overrides the containerd socket the agent finds on
	// its own (the usual, k3s and RKE2 places); empty lets it look.
	ContainerdSocket string
	// StateDir is the agent's directory on the host; empty is DefaultStateDir.
	StateDir string
	// APIServer is the API server address the agents use instead of the
	// cluster's "kubernetes" Service: for nodes where nothing serves Service
	// addresses (no kube-proxy or replacement, as on KubeEdge or OpenYurt
	// edge nodes). "https://host:port" or "host:port"; empty uses the Service.
	APIServer string
	// Priority is the value of the hold and instance pods' PriorityClass;
	// AllowPreemption lets the scheduler evict lower-priority pods for a hold
	// pod, and off renders preemptionPolicy: Never.
	Priority        int
	AllowPreemption bool
}

// PriorityClass names the PriorityClass of the hold and instance pods of the
// agents in namespace: PriorityClasses are cluster-wide, and two entries of
// one cluster (on different nodes) each have a namespace of their own.
func PriorityClass(namespace string) string { return PriorityClassPrefix + "-" + namespace }

// RemoveCommands take an entry's agents out of a cluster with kubectl, in
// order, as the server does for a cluster it can reach: the DaemonSet first
// (the hold and instance pods go with their agents), then the cluster-wide
// objects named for the namespace, then the namespace with all that is left
// in it. They need no enrollment token, so they work for a revoked entry.
func RemoveCommands(namespace string) []string {
	ns := quoteShell(namespace)
	role := quoteShell(AgentName + "-" + namespace)
	return []string{
		"kubectl -n " + ns + " delete daemonset " + AgentName + " --ignore-not-found --wait",
		"kubectl delete clusterrolebinding " + role + " --ignore-not-found",
		"kubectl delete clusterrole " + role + " --ignore-not-found",
		"kubectl delete priorityclass " + quoteShell(PriorityClass(namespace)) + " --ignore-not-found",
		"kubectl delete namespace " + ns + " --ignore-not-found",
	}
}

// quoteShell quotes s for a POSIX shell when it is more than a DNS name.
func quoteShell(s string) string {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}

// PriorityMax is the largest priority a user's PriorityClass may have;
// Kubernetes keeps the values above it for its own system classes.
const PriorityMax = 1000000000

// ParseAPIServer splits an API server address into host and port.
func ParseAPIServer(s string) (host, port string, err error) {
	s = strings.TrimSpace(s)
	addr := s
	if rest, ok := strings.CutPrefix(s, "https://"); ok {
		addr = strings.TrimSuffix(rest, "/")
		if !strings.Contains(strings.TrimPrefix(addr, "["), "]:") && strings.Count(addr, ":") == 0 {
			addr += ":443"
		}
	} else if strings.Contains(s, "://") {
		return "", "", fmt.Errorf("API server %q: only https is served", s)
	}
	host, port, err = net.SplitHostPort(addr)
	if err != nil || host == "" || strings.ContainsAny(host, "/ ") {
		return "", "", fmt.Errorf("API server %q: want https://host:port or host:port", s)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", "", fmt.Errorf("API server %q: port %q is not a port", s, port)
	}
	return host, port, nil
}

func (p Params) stateDir() string {
	if p.StateDir == "" {
		return DefaultStateDir
	}
	return p.StateDir
}

var (
	dnsLabel  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	dnsName   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	labelPart = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
)

// ParseNodeSelector reads "key=value,key2=value2" into a selector.
func ParseNodeSelector(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		name := k
		if i := strings.LastIndex(k, "/"); i >= 0 {
			name = k[i+1:]
		}
		if !ok || len(name) > 63 || !labelPart.MatchString(name) || len(v) > 63 || (v != "" && !labelPart.MatchString(v)) {
			return nil, fmt.Errorf("node selector %q: want key=value label pairs", part)
		}
		out[k] = v
	}
	return out, nil
}

// Validate reports the first parameter the manifest cannot be rendered with.
func (p Params) Validate() error {
	switch {
	case len(p.Namespace) > 63 || !dnsLabel.MatchString(p.Namespace):
		return fmt.Errorf("namespace %q is not a DNS label", p.Namespace)
	case !strings.HasPrefix(p.ServerURL, "http://") && !strings.HasPrefix(p.ServerURL, "https://"):
		return fmt.Errorf("server URL %q must start with http:// or https://", p.ServerURL)
	case strings.TrimSpace(p.Token) == "":
		return fmt.Errorf("enrollment token is empty")
	case strings.TrimSpace(p.AgentImage) == "":
		return fmt.Errorf("agent image is empty")
	case p.ImagePullSecret != "" && !ValidSecretName(p.ImagePullSecret):
		return fmt.Errorf("image pull secret %q is not a Secret name", p.ImagePullSecret)
	case (p.PoolLabel == "") != (len(p.PoolIDs) == 0):
		return fmt.Errorf("a node pool selection needs both the label that carries the pool id and the pool ids")
	case p.PoolLabel != "" && !validLabelKey(p.PoolLabel):
		return fmt.Errorf("node pool label %q is not a label key", p.PoolLabel)
	case p.ContainerdSocket != "" && !cleanAbs(p.ContainerdSocket):
		return fmt.Errorf("containerd socket %q must be an absolute path", p.ContainerdSocket)
	case p.StateDir != "" && (!cleanAbs(p.StateDir) || p.StateDir == "/"):
		return fmt.Errorf("state directory %q must be an absolute path below /", p.StateDir)
	case p.Priority < 0 || p.Priority > PriorityMax:
		return fmt.Errorf("priority %d must be between 0 and %d", p.Priority, PriorityMax)
	}
	if p.APIServer != "" {
		if _, _, err := ParseAPIServer(p.APIServer); err != nil {
			return err
		}
	}
	for _, id := range p.PoolIDs {
		if len(id) > 63 || !labelPart.MatchString(id) {
			return fmt.Errorf("node pool id %q cannot be a label value", id)
		}
	}
	return nil
}

// validLabelKey says whether s is a label key: a name, optionally behind a
// DNS subdomain prefix and a slash.
func validLabelKey(s string) bool {
	prefix, name, hasPrefix := strings.Cut(s, "/")
	if !hasPrefix {
		name = prefix
	} else if len(prefix) > 253 || !dnsName.MatchString(prefix) {
		return false
	}
	return len(name) <= 63 && labelPart.MatchString(name)
}

// ValidSecretName says whether s can name a Secret (a DNS subdomain).
func ValidSecretName(s string) bool { return len(s) <= 253 && dnsName.MatchString(s) }

// ValidRuntimeClass says whether s can name a RuntimeClass (a DNS label); the
// empty string is valid and leaves the choice to each agent. A RuntimeClass is
// no part of the manifest: the server hands it to the agents live.
func ValidRuntimeClass(s string) bool {
	return s == "" || (len(s) <= 63 && dnsLabel.MatchString(s))
}

func cleanAbs(s string) bool {
	return strings.HasPrefix(s, "/") && path.Clean(s) == s && !strings.ContainsAny(s, "\"\n")
}

// Render returns the manifest as one multi-document YAML stream.
func Render(p Params) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	keys := make([]string, 0, len(p.NodeSelector))
	for k := range p.NodeSelector {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	type kv struct{ K, V string }
	selector := make([]kv, 0, len(keys))
	for _, k := range keys {
		selector = append(selector, kv{k, p.NodeSelector[k]})
	}
	poolIDs := append([]string(nil), p.PoolIDs...)
	sort.Strings(poolIDs)
	// The agent carries no vendor's mounts: it reads the host's files
	// through the host's root and runs vendor CLIs there (hostPID). The
	// NVIDIA variables only take effect under NVIDIA's runtime, where they
	// put the driver's nvidia-smi into the agent's container.
	env := []kv{{"NVIDIA_DRIVER_CAPABILITIES", "compute,utility"}, {"NVIDIA_VISIBLE_DEVICES", "all"}}
	apiHost, apiPort := "", ""
	if p.APIServer != "" {
		apiHost, apiPort, _ = ParseAPIServer(p.APIServer)
	}
	var buf bytes.Buffer
	err := manifest.Execute(&buf, map[string]any{
		"APIHost": apiHost, "APIPort": apiPort,
		"P": p, "Selector": selector, "PoolIDs": poolIDs, "Agent": AgentName, "Secret": EnrollSecret,
		"Priority": PriorityClass(p.Namespace), "StateDir": p.stateDir(), "PodResources": PodResourcesDir,
		"DevicePlugins": DevicePluginDir, "Env": env,
	})
	return buf.String(), err
}

// q quotes a value as a YAML double-quoted scalar (JSON string syntax is a
// subset of it), so no parameter can change the document's structure.
func q(s string) string { return strconv.Quote(s) }

var manifest = template.Must(template.New("manifest").Funcs(template.FuncMap{"q": q}).Parse(`# GPUWorks agents for cluster {{q .P.ClusterName}}.
# Apply with kubectl apply -f; delete the same file to remove every agent and
# instance pod GPUWorks started (the pods are owned by the DaemonSet).
apiVersion: v1
kind: Namespace
metadata:
  name: {{q .P.Namespace}}
  labels:
    pod-security.kubernetes.io/enforce: privileged
    app.kubernetes.io/managed-by: gpuworks
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{.Agent}}
  namespace: {{q .P.Namespace}}
---
apiVersion: v1
kind: Secret
metadata:
  name: {{.Secret}}
  namespace: {{q .P.Namespace}}
type: Opaque
stringData:
  token: {{q .P.Token}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: {{.Agent}}
  namespace: {{q .P.Namespace}}
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch", "create", "delete", "patch"]
  - apiGroups: [""]
    resources: ["pods/log"]
    verbs: ["get"]
  - apiGroups: ["apps"]
    resources: ["daemonsets"]
    resourceNames: ["{{.Agent}}"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {{.Agent}}
  namespace: {{q .P.Namespace}}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: {{.Agent}}
subjects:
  - kind: ServiceAccount
    name: {{.Agent}}
    namespace: {{q .P.Namespace}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{q (printf "%s-%s" .Agent .P.Namespace)}}
rules:
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list", "watch"]
  # Other namespaces' pods on the agent's own node: where HAMi or ACK GPU
  # sharing allocates cards, their allocation annotations are the only record
  # of who holds how much of a card.
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list"]
  - apiGroups: [""]
    resources: ["namespaces"]
    resourceNames: ["kube-system"]
    verbs: ["get"]
  - apiGroups: ["node.k8s.io"]
    resources: ["runtimeclasses"]
    resourceNames: ["nvidia"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: {{q (printf "%s-%s" .Agent .P.Namespace)}}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: {{q (printf "%s-%s" .Agent .P.Namespace)}}
subjects:
  - kind: ServiceAccount
    name: {{.Agent}}
    namespace: {{q .P.Namespace}}
---
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: {{q .Priority}}
value: {{.P.Priority}}
{{- if not .P.AllowPreemption}}
preemptionPolicy: Never
{{- end}}
globalDefault: false
description: "GPUWorks hold and inference pods of namespace {{.P.Namespace}}."
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: {{.Agent}}
  namespace: {{q .P.Namespace}}
  labels:
    app.kubernetes.io/name: {{.Agent}}
    app.kubernetes.io/managed-by: gpuworks
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: {{.Agent}}
  updateStrategy:
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/name: {{.Agent}}
        app.kubernetes.io/managed-by: gpuworks
    spec:
      serviceAccountName: {{.Agent}}
{{- if .P.ImagePullSecret}}
      imagePullSecrets:
        - name: {{q .P.ImagePullSecret}}
{{- end}}
      hostNetwork: true
      hostPID: true
      # The node's own resolver, as an agent installed on the host has: the
      # agent reaches the server and model sources outside the cluster, and
      # the API server by address.
      dnsPolicy: Default
      priorityClassName: system-node-critical
{{- if .Selector}}
      nodeSelector:
{{- range .Selector}}
        {{q .K}}: {{q .V}}
{{- end}}
{{- end}}
{{- if .PoolIDs}}
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
              - matchExpressions:
                  - key: {{q .P.PoolLabel}}
                    operator: In
                    values:
{{- range .PoolIDs}}
                      - {{q .}}
{{- end}}
{{- end}}
      tolerations:
        - operator: Exists
      terminationGracePeriodSeconds: 30
      containers:
        - name: agent
          image: {{q .P.AgentImage}}
          imagePullPolicy: IfNotPresent
          args:
            - run
            - {{q (printf "--server=%s" .P.ServerURL)}}
            - {{q (printf "--state=%s" .StateDir)}}
            - "--driver=kube"
          env:
            - name: GPUWORKS_KUBE_ENROLL_TOKEN
              valueFrom:
                secretKeyRef:
                  name: {{.Secret}}
                  key: token
            - name: GPUWORKS_KUBE_NODE_NAME
              valueFrom:
                fieldRef:
                  fieldPath: spec.nodeName
            - name: GPUWORKS_KUBE_POD_NAME
              valueFrom:
                fieldRef:
                  fieldPath: metadata.name
            - name: GPUWORKS_KUBE_NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
            - name: GPUWORKS_KUBE_PRIORITY_CLASS
              value: {{q .Priority}}
{{- if .APIHost}}
            - name: KUBERNETES_SERVICE_HOST
              value: {{q .APIHost}}
            - name: KUBERNETES_SERVICE_PORT
              value: {{q .APIPort}}
{{- end}}
{{- if .P.ContainerdSocket}}
            - name: GPUWORKS_KUBE_CONTAINERD_SOCKET
              value: {{q .P.ContainerdSocket}}
{{- end}}
{{- range .Env}}
            - name: {{q .K}}
              value: {{q .V}}
{{- end}}
          securityContext:
            privileged: true
          volumeMounts:
            - name: state
              mountPath: {{q .StateDir}}
              mountPropagation: Bidirectional
            - name: pod-resources
              mountPath: {{.PodResources}}
              readOnly: true
            - name: device-plugins
              mountPath: {{.DevicePlugins}}
      volumes:
        - name: state
          hostPath:
            path: {{q .StateDir}}
            type: DirectoryOrCreate
        - name: pod-resources
          hostPath:
            path: {{.PodResources}}
            type: DirectoryOrCreate
        - name: device-plugins
          hostPath:
            path: {{.DevicePlugins}}
            type: DirectoryOrCreate
`))
