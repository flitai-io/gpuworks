package kubeapply

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/kubemanifest"
)

// The parts of a pod the checks judge by (kubemanifest.PodState), as the
// API server sends them.
type podList struct {
	Items []pod `json:"items"`
}

type pod struct {
	Metadata struct {
		Name              string  `json:"name"`
		DeletionTimestamp *string `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec struct {
		NodeName string `json:"nodeName"`
		Affinity *struct {
			NodeAffinity *struct {
				Required *struct {
					Terms []struct {
						MatchFields []struct {
							Values []string `json:"values"`
						} `json:"matchFields"`
					} `json:"nodeSelectorTerms"`
				} `json:"requiredDuringSchedulingIgnoredDuringExecution"`
			} `json:"nodeAffinity"`
		} `json:"affinity"`
	} `json:"spec"`
	Status struct {
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"conditions"`
		ContainerStatuses []struct {
			Ready     bool           `json:"ready"`
			State     containerState `json:"state"`
			LastState containerState `json:"lastState"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

type containerState struct {
	Waiting *struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"waiting"`
	Terminated *struct {
		ExitCode int `json:"exitCode"`
	} `json:"terminated"`
}

// state reduces a pod to what the checks judge by, reading the same fields
// check.sh's go-template reads.
func (p pod) state() kubemanifest.PodState {
	s := kubemanifest.PodState{Name: p.Metadata.Name, Node: p.Spec.NodeName}
	if s.Node == "" && p.Spec.Affinity != nil && p.Spec.Affinity.NodeAffinity != nil && p.Spec.Affinity.NodeAffinity.Required != nil {
		for _, t := range p.Spec.Affinity.NodeAffinity.Required.Terms {
			for _, f := range t.MatchFields {
				s.Node += strings.Join(f.Values, "")
			}
		}
	}
	if len(p.Status.ContainerStatuses) > 0 {
		c := p.Status.ContainerStatuses[0]
		s.Ready = c.Ready
		if c.State.Waiting != nil {
			s.Reason, s.Message = c.State.Waiting.Reason, c.State.Waiting.Message
		}
		switch {
		case c.State.Terminated != nil:
			s.ExitCode = &c.State.Terminated.ExitCode
		case c.LastState.Terminated != nil:
			s.ExitCode = &c.LastState.Terminated.ExitCode
		}
	}
	for _, c := range p.Status.Conditions {
		if c.Type == "PodScheduled" && c.Status == "False" {
			s.Unscheduled = c.Message
		}
	}
	return s
}

// AgentRollout reads how DaemonSet name in namespace and its pods stand, for
// kubemanifest.JudgeAgents. It only reads: the DaemonSet, its pods, and the
// events that say why an image was not pulled or a pod not created.
func (c *Client) AgentRollout(ctx context.Context, namespace, name string) (kubemanifest.RolloutState, error) {
	out := kubemanifest.RolloutState{Namespace: namespace, Name: name}
	ns := url.PathEscape(namespace)
	raw, err := c.do(ctx, http.MethodGet, "/apis/apps/v1/namespaces/"+ns+"/daemonsets/"+url.PathEscape(name), nil, "", nil)
	if isNotFound(err) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	var ds struct {
		Status struct {
			ObservedGeneration     int64 `json:"observedGeneration"`
			DesiredNumberScheduled int   `json:"desiredNumberScheduled"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &ds); err != nil {
		return out, err
	}
	out.Found = true
	if ds.Status.ObservedGeneration > 0 {
		desired := ds.Status.DesiredNumberScheduled
		out.Desired = &desired
	}
	raw, err = c.do(ctx, http.MethodGet, "/api/v1/namespaces/"+ns+"/pods", url.Values{"labelSelector": {"app.kubernetes.io/name=" + name}}, "", nil)
	if err != nil {
		return out, err
	}
	var pods podList
	if err := json.Unmarshal(raw, &pods); err != nil {
		return out, err
	}
	for _, p := range pods.Items {
		// A pod going away is no answer for its node.
		if p.Metadata.DeletionTimestamp != nil {
			continue
		}
		s := p.state()
		if s.Reason == "ErrImagePull" || s.Reason == "ImagePullBackOff" {
			if s.PullError, err = c.longestEvent(ctx, ns, "Pod", s.Name, "Failed"); err != nil {
				return out, err
			}
		}
		out.Pods = append(out.Pods, s)
	}
	if out.Desired != nil && len(out.Pods) < *out.Desired {
		if out.CreateError, err = c.longestEvent(ctx, ns, "DaemonSet", name, "FailedCreate"); err != nil {
			return out, err
		}
	}
	return out, nil
}

// longestEvent is the message of an event of kind/name with reason that says
// the most: the longest. For a pod's Failed events that is the kubelet's pull
// error; the others ("Error: ErrImagePull") only repeat the pod's reason.
// Without such events it is "".
func (c *Client) longestEvent(ctx context.Context, ns, kind, name, reason string) (string, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/v1/namespaces/"+ns+"/events", url.Values{
		"fieldSelector": {"involvedObject.kind=" + kind + ",involvedObject.name=" + name + ",reason=" + reason},
	}, "", nil)
	if err != nil {
		return "", err
	}
	var events struct {
		Items []struct {
			Message string `json:"message"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &events); err != nil {
		return "", err
	}
	most := ""
	for _, e := range events.Items {
		if len(e.Message) >= len(most) {
			most = e.Message
		}
	}
	return most, nil
}
