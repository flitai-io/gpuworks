package kubemanifest

import "strconv"

// PodState is one pod of a DaemonSet as the API server reports it, reduced
// to what the checks judge by: the same fields check.sh reads.
type PodState struct {
	Name string
	// Node is the pod's node, or the node the DaemonSet meant it for while
	// it is not scheduled.
	Node  string
	Ready bool
	// Reason and Message are the container's waiting reason and message.
	Reason  string
	Message string
	// ExitCode is the container's last exit code, if it has exited.
	ExitCode *int
	// Unscheduled is why the scheduler has not placed the pod, if it has not.
	Unscheduled string
	// PullError is the kubelet's event that says why the image was not
	// pulled; the waiting message only says it backs off.
	PullError string
}

// RolloutState is a DaemonSet and its pods, as the API server reports them.
type RolloutState struct {
	Namespace string
	Name      string
	Found     bool
	// Desired is how many pods the DaemonSet wants, nil until its
	// controller has counted the nodes.
	Desired *int
	Pods    []PodState
	// CreateError is the DaemonSet's last FailedCreate event, when it could
	// not create some of its pods.
	CreateError string
}

// CheckRow is one line of a check: a node (empty for the whole DaemonSet),
// its pod, a code and the detail Kubernetes gave.
type CheckRow struct {
	Node   string
	Pod    string
	Code   CheckCode
	Detail string
}

func pullFailed(reason string) bool {
	switch reason {
	case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull":
		return true
	}
	return false
}

// decided says whether a pod's state is final enough to report, as check.sh
// decides it: running, or failed in a way waiting longer does not change.
func (p PodState) decided() bool {
	return p.Ready || pullFailed(p.Reason) || p.ExitCode != nil || p.Unscheduled != ""
}

// JudgeAgents says how the agents of an applied manifest stand, node by
// node, with the codes and rules of the check after an apply (Verify), so
// that the console and the script agree. done is false while waiting longer
// may still change the answer.
func JudgeAgents(s RolloutState) (rows []CheckRow, done bool) {
	if !s.Found {
		return []CheckRow{{Code: CheckDaemonSetMissing, Detail: "no DaemonSet " + s.Name + " in namespace " + s.Namespace}}, true
	}
	if s.Desired != nil && *s.Desired == 0 {
		return []CheckRow{{Code: CheckNoNodeSelected, Detail: "the DaemonSet's node selection matches no node"}}, true
	}
	podsDecided := true
	for _, p := range s.Pods {
		podsDecided = podsDecided && p.decided()
		row := CheckRow{Node: p.Node, Pod: p.Name}
		if row.Node == "" {
			row.Node = p.Name
		}
		switch {
		case p.Ready:
			row.Code = CheckOK
		case pullFailed(p.Reason):
			row.Code, row.Detail = CheckImagePullFailed, p.PullError
			if row.Detail == "" {
				row.Detail = p.Reason + ": " + p.Message
			}
		case p.Unscheduled != "":
			row.Code, row.Detail = CheckNotScheduled, p.Unscheduled
		case p.ExitCode != nil || p.Reason == "CrashLoopBackOff":
			code := "?"
			if p.ExitCode != nil {
				code = strconv.Itoa(*p.ExitCode)
			}
			row.Code, row.Detail = CheckContainerFailed, "exit code "+code+"; kubectl -n "+s.Namespace+" logs "+p.Name+" --previous"
		case p.Reason != "" && p.Reason != "ContainerCreating" && p.Reason != "PodInitializing":
			row.Code, row.Detail = CheckContainerFailed, p.Reason+": "+p.Message
		default:
			row.Code, row.Detail = CheckStillStarting, "still pulling the image or starting"
		}
		rows = append(rows, row)
	}
	switch {
	case s.Desired == nil:
		done = false
		rows = append(rows, CheckRow{Code: CheckStillStarting, Detail: "the DaemonSet controller has not counted the nodes yet"})
	case len(s.Pods) < *s.Desired:
		detail := s.CreateError
		if detail == "" {
			detail = "see kubectl -n " + s.Namespace + " describe daemonset " + s.Name
		}
		rows = append(rows, CheckRow{Code: CheckPodCreateFailed,
			Detail: strconv.Itoa(*s.Desired-len(s.Pods)) + " of " + strconv.Itoa(*s.Desired) + " pods were not created: " + detail})
		// Pods the DaemonSet failed to create do not come by waiting.
		done = podsDecided && s.CreateError != ""
	default:
		done = podsDecided
	}
	return rows, done
}
