// Package kubefake is an in-memory Kubernetes API server for the kube
// runtime's tests. It answers the calls kubeapi makes the way a real API
// server does — Status bodies with reason and code, AlreadyExists on a
// duplicate create, Invalid on a pod without containers, label and field
// selectors on lists — and stands in for the kubelet with a Kubelet hook
// that decides what a created pod becomes.
package kubefake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

// Server is the fake API server.
type Server struct {
	*httptest.Server
	mu    sync.Mutex
	pods  map[string]*kubeapi.Pod // ns/name
	nodes map[string]*kubeapi.Node
	// daemonSet is the agent DaemonSet the agent reads (its UID); it is
	// served as a generic JSON document.
	daemonSet map[string]any
	clusterID string
	rv        int
	// RuntimeClasses are the cluster's RuntimeClass names.
	RuntimeClasses map[string]bool
	// Logs is what a pod's log endpoint returns, by pod name.
	Logs map[string]string
	// Kubelet decides what a created pod becomes; nil makes it Running.
	Kubelet func(*kubeapi.Pod)
	// FailNext makes the next request whose "METHOD path" has this prefix
	// fail with the given status code, once.
	FailNext map[string]int
	// TerminationDelay is how long a deleted pod stays Terminating (it
	// keeps its deletionTimestamp and its place in lists and gets) before
	// the kubelet has stopped its containers and the pod is gone. Zero
	// removes it at once.
	TerminationDelay time.Duration
	// DenyClusterPods makes the cluster-wide pod list answer 403 Forbidden, as
	// it does for a service account without a ClusterRole that lists pods.
	DenyClusterPods bool
	// Requests records "METHOD path?query" of every request.
	Requests []string
}

// New starts a server with one node and an agent DaemonSet in ns.
func New(t testing.TB, ns, node string) *Server {
	s := &Server{pods: map[string]*kubeapi.Pod{}, nodes: map[string]*kubeapi.Node{}, RuntimeClasses: map[string]bool{}, Logs: map[string]string{}, FailNext: map[string]int{}, clusterID: "kube-system-uid"}
	n := &kubeapi.Node{Metadata: kubeapi.ObjectMeta{Name: node, UID: "node-uid-" + node}}
	n.Status.Allocatable = map[string]string{}
	n.Status.Capacity = map[string]string{}
	n.Status.NodeInfo.ContainerRuntimeVersion = "containerd://2.0.5"
	s.nodes[node] = n
	s.daemonSet = map[string]any{
		"metadata": map[string]any{"name": "gpuworks-agent", "namespace": ns, "uid": "ds-uid"},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{
			map[string]any{"name": "agent", "image": "gpuworks/gpuworks-agent:v1",
				"args": []any{"run", "--driver=kube"}, "env": []any{map[string]any{"name": "A", "value": "1"}}},
		}}}},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Client returns a kubeapi client for the server.
func (s *Server) Client() *kubeapi.Client {
	return kubeapi.New(s.URL, func() (string, error) { return "sa-token", nil }, s.Server.Client())
}

// Node returns the node for a test to change.
func (s *Server) Node(name string) *kubeapi.Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nodes[name]
}

// SetAllocatable sets a node's resource the way a device plugin does: its
// capacity and, with every device healthy, its allocatable quantity.
func (s *Server) SetAllocatable(node, resource, qty string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[node].Status.Allocatable[resource] = qty
	s.nodes[node].Status.Capacity[resource] = qty
}

// UpdateNode changes a stored node (what a device plugin, a scheduler
// installer or an operator does to it over time).
func (s *Server) UpdateNode(name string, fn func(*kubeapi.Node)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.nodes[name])
}

// SetNodeLabel sets a node label.
func (s *Server) SetNodeLabel(node, key, value string) {
	s.UpdateNode(node, func(n *kubeapi.Node) {
		if n.Metadata.Labels == nil {
			n.Metadata.Labels = map[string]string{}
		}
		n.Metadata.Labels[key] = value
	})
}

// SetNodeAnnotation sets a node annotation; an empty value removes it.
func (s *Server) SetNodeAnnotation(node, key, value string) {
	s.UpdateNode(node, func(n *kubeapi.Node) {
		if value == "" {
			delete(n.Metadata.Annotations, key)
			return
		}
		if n.Metadata.Annotations == nil {
			n.Metadata.Annotations = map[string]string{}
		}
		n.Metadata.Annotations[key] = value
	})
}

// SetCapacity sets a node's capacity and allocatable of resource apart, as
// a node reads when a plugin's devices are withdrawn (ACK's sharing mode
// keeps nvidia.com/gpu capacity 1 and makes allocatable 0).
func (s *Server) SetCapacity(node, resource, capacity, allocatable string) {
	s.UpdateNode(node, func(n *kubeapi.Node) {
		n.Status.Capacity[resource] = capacity
		n.Status.Allocatable[resource] = allocatable
	})
}

// SetRuntime sets the container runtime the node's kubelet reports.
func (s *Server) SetRuntime(node, version string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[node].Status.NodeInfo.ContainerRuntimeVersion = version
}

// SetUnschedulable cordons or uncordons a node.
func (s *Server) SetUnschedulable(node string, v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[node].Spec.Unschedulable = v
}

// Pod returns a copy of a pod, or nil.
func (s *Server) Pod(ns, name string) *kubeapi.Pod {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pods[ns+"/"+name]
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

// Pods returns copies of every pod.
func (s *Server) Pods() []kubeapi.Pod {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]kubeapi.Pod, 0, len(s.pods))
	for _, p := range s.pods {
		out = append(out, *p)
	}
	return out
}

// UpdatePod changes a stored pod (what the kubelet does over time).
func (s *Server) UpdatePod(ns, name string, fn func(*kubeapi.Pod)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.pods[ns+"/"+name]; p != nil {
		fn(p)
		s.rv++
		p.Metadata.ResourceVersion = strconv.Itoa(s.rv)
	}
}

// Running makes a pod's only container run.
func Running(p *kubeapi.Pod) {
	p.Status.Phase = "Running"
	p.Status.ContainerStatuses = []kubeapi.ContainerStatus{{
		Name: p.Spec.Containers[0].Name, ContainerID: "containerd://" + containerID(p.Metadata.UID), Ready: true,
		ImageID: "docker.io/library/" + p.Spec.Containers[0].Image + "@sha256:" + strings.Repeat("ab", 32),
		State:   kubeapi.ContainerState{Running: &kubeapi.StateRunning{StartedAt: "2026-10-19T00:00:00Z"}},
	}}
}

// containerID is a containerd container id (64 hex digits) derived from the pod uid.
func containerID(uid string) string {
	return fmt.Sprintf("%064x", []byte(uid))[:64]
}

var dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

func (s *Server) status(w http.ResponseWriter, code int, reason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{}, "status": "Failure", "message": msg, "reason": reason, "code": code})
}

func (s *Server) json(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.Requests = append(s.Requests, r.Method+" "+r.URL.RequestURI())
	for prefix, code := range s.FailNext {
		if strings.HasPrefix(r.Method+" "+r.URL.Path, prefix) {
			delete(s.FailNext, prefix)
			s.mu.Unlock()
			s.status(w, code, http.StatusText(code), "injected failure")
			return
		}
	}
	s.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer sa-token" {
		s.status(w, http.StatusUnauthorized, "Unauthorized", "Unauthorized")
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(parts) == 4 && parts[0] == "api" && parts[2] == "namespaces" && r.Method == http.MethodGet:
		if parts[3] != "kube-system" {
			s.status(w, 404, "NotFound", fmt.Sprintf("namespaces %q not found", parts[3]))
			return
		}
		s.json(w, 200, kubeapi.Namespace{Metadata: kubeapi.ObjectMeta{Name: "kube-system", UID: s.clusterID}})
	case len(parts) == 4 && parts[0] == "api" && parts[2] == "nodes":
		s.mu.Lock()
		n := s.nodes[parts[3]]
		var cp kubeapi.Node
		if n != nil {
			cp = *n
		}
		s.mu.Unlock()
		if n == nil {
			s.status(w, 404, "NotFound", fmt.Sprintf("nodes %q not found", parts[3]))
			return
		}
		s.json(w, 200, cp)
	case len(parts) == 5 && parts[0] == "apis" && parts[1] == "node.k8s.io" && parts[3] == "runtimeclasses" && r.Method == http.MethodGet:
		s.mu.Lock()
		ok := s.RuntimeClasses[parts[4]]
		s.mu.Unlock()
		if !ok {
			s.status(w, 404, "NotFound", fmt.Sprintf("runtimeclasses.node.k8s.io %q not found", parts[4]))
			return
		}
		s.json(w, 200, map[string]any{"apiVersion": "node.k8s.io/v1", "kind": "RuntimeClass", "metadata": map[string]any{"name": parts[4]}, "handler": parts[4]})
	case len(parts) == 3 && parts[0] == "api" && parts[2] == "pods" && r.Method == http.MethodGet:
		s.listAllPods(w, r)
	case len(parts) == 7 && parts[0] == "apis" && parts[5] == "daemonsets":
		s.serveDaemonSet(w, r, parts[6])
	case len(parts) >= 5 && parts[0] == "api" && parts[2] == "namespaces" && parts[4] == "pods":
		s.servePods(w, r, parts[3], parts[5:])
	default:
		s.status(w, 404, "NotFound", "the server could not find the requested resource")
	}
}

// listAllPods answers the cluster-wide pod list.
func (s *Server) listAllPods(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	deny := s.DenyClusterPods
	list := kubeapi.PodList{Items: []kubeapi.Pod{}}
	for _, p := range s.pods {
		if matches(p, r.URL.Query().Get("labelSelector"), r.URL.Query().Get("fieldSelector")) {
			list.Items = append(list.Items, *p)
		}
	}
	s.mu.Unlock()
	if deny {
		s.status(w, 403, "Forbidden", `pods is forbidden: User "system:serviceaccount:gpuworks-system:gpuworks-agent" cannot list resource "pods" in API group "" at the cluster scope`)
		return
	}
	s.json(w, 200, list)
}

func (s *Server) serveDaemonSet(w http.ResponseWriter, r *http.Request, name string) {
	s.mu.Lock()
	dsName := s.daemonSet["metadata"].(map[string]any)["name"]
	s.mu.Unlock()
	if name != dsName {
		s.status(w, 404, "NotFound", fmt.Sprintf("daemonsets.apps %q not found", name))
		return
	}
	if r.Method != http.MethodGet {
		// The manifest's Role grants the agent only get on its DaemonSet.
		s.status(w, 403, "Forbidden", fmt.Sprintf("daemonsets.apps %q is forbidden: cannot %s", name, strings.ToLower(r.Method)))
		return
	}
	s.mu.Lock()
	raw, _ := json.Marshal(s.daemonSet)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func matches(p *kubeapi.Pod, labelSel, fieldSel string) bool {
	for _, term := range strings.Split(labelSel, ",") {
		if term == "" {
			continue
		}
		k, v, _ := strings.Cut(term, "=")
		if p.Metadata.Labels[k] != v {
			return false
		}
	}
	for _, term := range strings.Split(fieldSel, ",") {
		if term == "" {
			continue
		}
		k, v, _ := strings.Cut(term, "=")
		switch k {
		case "spec.nodeName":
			if p.Spec.NodeName != v {
				return false
			}
		case "metadata.name":
			if p.Metadata.Name != v {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (s *Server) servePods(w http.ResponseWriter, r *http.Request, ns string, rest []string) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		s.mu.Lock()
		list := kubeapi.PodList{Items: []kubeapi.Pod{}}
		for key, p := range s.pods {
			if strings.HasPrefix(key, ns+"/") && matches(p, r.URL.Query().Get("labelSelector"), r.URL.Query().Get("fieldSelector")) {
				list.Items = append(list.Items, *p)
			}
		}
		s.mu.Unlock()
		s.json(w, 200, list)
	case len(rest) == 0 && r.Method == http.MethodPost:
		var p kubeapi.Pod
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			s.status(w, 400, "BadRequest", err.Error())
			return
		}
		if !dns1123.MatchString(p.Metadata.Name) || len(p.Metadata.Name) > 253 {
			s.status(w, 422, "Invalid", fmt.Sprintf("Pod %q is invalid: metadata.name: Invalid value", p.Metadata.Name))
			return
		}
		if len(p.Spec.Containers) == 0 || p.Spec.Containers[0].Image == "" {
			s.status(w, 422, "Invalid", fmt.Sprintf("Pod %q is invalid: spec.containers: Required value", p.Metadata.Name))
			return
		}
		key := ns + "/" + p.Metadata.Name
		s.mu.Lock()
		s.rv++
		p.Metadata.Namespace = ns
		p.Metadata.UID = fmt.Sprintf("uid-%d", s.rv)
		p.Metadata.ResourceVersion = strconv.Itoa(s.rv)
		p.Status = kubeapi.PodStatus{Phase: "Pending"}
		exists := s.pods[key] != nil
		kubelet := s.Kubelet
		s.mu.Unlock()
		if exists {
			s.status(w, 409, "AlreadyExists", fmt.Sprintf("pods %q already exists", p.Metadata.Name))
			return
		}
		// The kubelet acts outside the server's lock: it may read the API.
		if kubelet != nil {
			kubelet(&p)
		} else {
			Running(&p)
		}
		s.mu.Lock()
		if s.pods[key] != nil {
			s.mu.Unlock()
			s.status(w, 409, "AlreadyExists", fmt.Sprintf("pods %q already exists", p.Metadata.Name))
			return
		}
		s.pods[key] = &p
		cp := p
		s.mu.Unlock()
		s.json(w, 201, cp)
	case len(rest) == 1 || (len(rest) == 2 && rest[1] == "log"):
		key := ns + "/" + rest[0]
		s.mu.Lock()
		p := s.pods[key]
		var cp kubeapi.Pod
		if p != nil {
			cp = *p
		}
		s.mu.Unlock()
		if p == nil {
			s.status(w, 404, "NotFound", fmt.Sprintf("pods %q not found", rest[0]))
			return
		}
		if len(rest) == 2 {
			w.Header().Set("Content-Type", "text/plain")
			text := s.Logs[rest[0]]
			if tail := r.URL.Query().Get("tailLines"); tail != "" {
				n, _ := strconv.Atoi(tail)
				lines := strings.SplitAfter(text, "\n")
				if lines[len(lines)-1] == "" {
					lines = lines[:len(lines)-1]
				}
				if n < len(lines) {
					lines = lines[len(lines)-n:]
				}
				text = strings.Join(lines, "")
			}
			_, _ = io.WriteString(w, text)
			return
		}
		switch r.Method {
		case http.MethodGet:
			s.json(w, 200, cp)
		case http.MethodDelete:
			s.deletePod(key, &cp)
			s.json(w, 200, cp)
		default:
			s.status(w, 405, "MethodNotAllowed", "method not allowed")
		}
	default:
		s.status(w, 404, "NotFound", "the server could not find the requested resource")
	}
}

// deletePod deletes the pod at key. With a TerminationDelay the pod first
// turns Terminating: it stays, with a deletionTimestamp, until the delay has
// passed, as a pod does while its containers are being stopped. Deleting a
// Terminating pod again does not extend the wait. cp is the pod as the
// response shows it.
func (s *Server) deletePod(key string, cp *kubeapi.Pod) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pods[key]
	if p == nil {
		return
	}
	if s.TerminationDelay <= 0 {
		delete(s.pods, key)
		return
	}
	if p.Metadata.DeletionTimestamp == nil {
		ts := time.Now().UTC().Format(time.RFC3339)
		p.Metadata.DeletionTimestamp = &ts
		s.rv++
		p.Metadata.ResourceVersion = strconv.Itoa(s.rv)
		uid := p.Metadata.UID
		time.AfterFunc(s.TerminationDelay, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if cur := s.pods[key]; cur != nil && cur.Metadata.UID == uid {
				delete(s.pods, key)
			}
		})
	}
	*cp = *p
}
