package kubemanifest

import (
	"strings"
	"testing"
)

func intp(n int) *int { return &n }

func TestJudgeAgentsMatchesTheCheckAfterAnApply(t *testing.T) {
	exit42 := intp(42)
	for name, tc := range map[string]struct {
		state RolloutState
		codes []CheckCode
		done  bool
		has   string
	}{
		"no DaemonSet":     {RolloutState{Namespace: "ns", Name: AgentName}, []CheckCode{CheckDaemonSetMissing}, true, "no DaemonSet gpuworks-agent in namespace ns"},
		"no node selected": {RolloutState{Found: true, Desired: intp(0)}, []CheckCode{CheckNoNodeSelected}, true, ""},
		"not counted yet":  {RolloutState{Found: true}, []CheckCode{CheckStillStarting}, false, "has not counted"},
		"every node runs": {RolloutState{Found: true, Desired: intp(2), Pods: []PodState{{Name: "a", Node: "n1", Ready: true}, {Name: "b", Node: "n2", Ready: true}}},
			[]CheckCode{CheckOK, CheckOK}, true, ""},
		"pull failed with the event": {RolloutState{Found: true, Desired: intp(1), Pods: []PodState{{Name: "a", Node: "n1", Reason: "ImagePullBackOff", Message: "Back-off pulling image", PullError: "Failed to pull image: i/o timeout"}}},
			[]CheckCode{CheckImagePullFailed}, true, "i/o timeout"},
		"pull failed without the event": {RolloutState{Found: true, Desired: intp(1), Pods: []PodState{{Name: "a", Node: "n1", Reason: "ErrImagePull", Message: "dial tcp: no route"}}},
			[]CheckCode{CheckImagePullFailed}, true, "ErrImagePull: dial tcp: no route"},
		// The agent's exit code means nothing special: only the pre-check's does.
		"crashing agent": {RolloutState{Namespace: "ns", Found: true, Desired: intp(1), Pods: []PodState{{Name: "a", Node: "n1", Reason: "CrashLoopBackOff", ExitCode: exit42}}},
			[]CheckCode{CheckContainerFailed}, true, "exit code 42; kubectl -n ns logs a --previous"},
		"config error": {RolloutState{Found: true, Desired: intp(1), Pods: []PodState{{Name: "a", Node: "n1", Reason: "CreateContainerConfigError", Message: "secret not found"}}},
			[]CheckCode{CheckContainerFailed}, false, "CreateContainerConfigError: secret not found"},
		"not scheduled": {RolloutState{Found: true, Desired: intp(1), Pods: []PodState{{Name: "a", Node: "n1", Unscheduled: "0/2 nodes are available: 1 Insufficient memory"}}},
			[]CheckCode{CheckNotScheduled}, true, "Insufficient memory"},
		"still pulling": {RolloutState{Found: true, Desired: intp(1), Pods: []PodState{{Name: "a", Node: "n1", Reason: "ContainerCreating"}}},
			[]CheckCode{CheckStillStarting}, false, ""},
		"pods refused": {RolloutState{Found: true, Desired: intp(3), Pods: []PodState{{Name: "a", Node: "n1", Ready: true}}, CreateError: "violates PodSecurity"},
			[]CheckCode{CheckOK, CheckPodCreateFailed}, true, "2 of 3 pods were not created: violates PodSecurity"},
		// Pods may still be on their way while no event says they were refused.
		"pods not there yet": {RolloutState{Found: true, Desired: intp(2), Pods: []PodState{{Name: "a", Node: "n1", Ready: true}}},
			[]CheckCode{CheckOK, CheckPodCreateFailed}, false, ""},
		"refused but others undecided": {RolloutState{Found: true, Desired: intp(3), Pods: []PodState{{Name: "a", Node: "n1", Reason: "ContainerCreating"}}, CreateError: "quota"},
			[]CheckCode{CheckStillStarting, CheckPodCreateFailed}, false, "quota"},
	} {
		rows, done := JudgeAgents(tc.state)
		var codes []CheckCode
		var details []string
		for _, r := range rows {
			codes = append(codes, r.Code)
			details = append(details, r.Detail)
		}
		if strings.Join(codeStrings(codes), ",") != strings.Join(codeStrings(tc.codes), ",") || done != tc.done || !strings.Contains(strings.Join(details, "\n"), tc.has) {
			t.Errorf("%s: got %v %v %q, want %v %v %q", name, codes, done, details, tc.codes, tc.done, tc.has)
		}
		for _, c := range codes {
			if !containsCode(VerifyCodes, c) {
				t.Errorf("%s: %s is not a code of the check after an apply", name, c)
			}
		}
	}
}

func TestJudgeAgentsNamesTheNodeOrThePod(t *testing.T) {
	rows, _ := JudgeAgents(RolloutState{Found: true, Desired: intp(1), Pods: []PodState{{Name: "gpuworks-agent-x", Ready: true}}})
	if rows[0].Node != "gpuworks-agent-x" || rows[0].Pod != "gpuworks-agent-x" {
		t.Errorf("rows %+v", rows)
	}
}

func codeStrings(in []CheckCode) []string {
	out := make([]string, len(in))
	for i, c := range in {
		out[i] = string(c)
	}
	return out
}

func containsCode(list []CheckCode, c CheckCode) bool {
	for _, x := range list {
		if x == c {
			return true
		}
	}
	return false
}
