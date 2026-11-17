package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"pgregory.net/rapid"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Evaluate and create are two hand-written pipelines over the same request,
// and they have disagreed before: a TP 1 × DP 2 draft that evaluated clean
// and was refused at create, and a create preflight that judged the raw
// image where the evaluation judged the resolved one. On the drawn world
// (placement, replicas, TP/PP/DP stated in the placement, quick params or the
// template's own args, sleep, a dGPU memory limit, the nodes' card counts,
// sizes and occupancy, and the model-identity dimensions of the selection
// property):
//
//   - forward: when Evaluate reports nothing blocking (evaluationBlocks),
//     Create of the identical request on the identical world is not
//     refused as an invalid request; and when both succeed, the deployment
//     lands on the node Evaluate named, with Evaluate's replica count, and -
//     when the stored placement states its parallelism - with the TP, PP,
//     node count and GPUs per node Evaluate reported.
//   - reverse: when Create refuses a request at all, Evaluate reported
//     something blocking for it. The world is static - nothing else creates
//     between the two calls - so a conflict refusal (automatic placement
//     found no node, say) has nothing to race and must be foreseen too.
//
// An evaluation that blocks while create succeeds is tallied, not failed:
// neither direction asks for it.
//
// Create runs on a world Evaluate has already seen; Evaluate leaves no rows
// behind (TestSelectedModelIsNeverSubstituted), so the two read the same
// world.
//
// go test ./internal/server/deployment/ -run TestEvaluateAgreesWithCreate -rapid.checks=1000 -v
func TestEvaluateAgreesWithCreate(t *testing.T) {
	var outcomes, gpuChecks outcomeTally
	rapid.Check(t, func(rt *rapid.T) {
		w := genLaunchWorld(rt)
		rt.Logf("world: %s", w)
		f := buildWorld(rt, w)
		defer func() { _ = f.st.Close() }()
		ctx := context.Background()

		resp, evalErr := f.svc.Evaluate(ctx, dto.EvaluateRequest{DeployRequest: f.request(w)})
		blocking := evaluationBlocks(w, resp, evalErr)
		created, createErr := f.svc.Create(ctx, f.request(w))
		refusedInvalid := createErr != nil && KindOf(createErr) == KindInvalid

		evaluation := "evaluate ok"
		switch {
		case evalErr != nil:
			code, _ := stableCode(evalErr)
			evaluation = "evaluate error " + code
		case len(resp.Errors) > 0:
			evaluation = "evaluate field errors " + fieldErrorCodes(resp.Errors)
		case !resp.OK:
			evaluation = "evaluate not ok, no errors"
		case w.Shape.Automatic && resp.SelectedNode == nil:
			evaluation = "evaluate ok, no selected node"
		}
		creation := "create ok"
		if createErr != nil {
			code, _ := stableCode(createErr)
			creation = fmt.Sprintf("create kind %d %s", KindOf(createErr), code)
		}
		outcomes.add(evaluation + " | " + creation)

		if !blocking && refusedInvalid {
			rt.Fatalf("forward: evaluate reported nothing blocking, create refused the same request as invalid: %v", createErr)
		}
		if !blocking && createErr != nil {
			rt.Fatalf("reverse: create refused (kind %d), evaluate reported nothing blocking: %v", KindOf(createErr), createErr)
		}
		if blocking || createErr != nil {
			if blocking && createErr == nil {
				gpuChecks.add("evaluate blocked, create succeeded (" + evaluation + ")")
			}
			return
		}
		gpuChecks.add(compareEvaluationWithDeployment(rt, f, resp, created))
	})
	outcomes.log(t, "outcome")
	gpuChecks.log(t, "placement")
}

// evaluationBlocks is whether an evaluation tells the deploy form not to
// submit: an error, a field error, ok=false (what a draft that does not fit
// answers, with no field error), or an automatic draft with no selected node
// (ok stays true; the form reads selected_node and answers "no matching
// node").
func evaluationBlocks(w acquisitionWorld, resp dto.EvaluateResponse, err error) bool {
	return err != nil || len(resp.Errors) > 0 || !resp.OK || (w.Shape.Automatic && resp.SelectedNode == nil)
}

// compareEvaluationWithDeployment holds a stored deployment to the node,
// replicas and parallelism its evaluation answered, and names what could be
// compared.
func compareEvaluationWithDeployment(rt *rapid.T, f *worldFixture, resp dto.EvaluateResponse, created *store.Deployment) string {
	node, err := f.st.NodeByID(created.NodeID)
	if err != nil {
		rt.Fatal(err)
	}
	evaluated := f.target.UUID
	if resp.SelectedNode != nil {
		evaluated = resp.SelectedNode.UUID
	}
	if node.UUID != evaluated {
		rt.Fatalf("evaluate placed the draft on %s, create stored node %s", evaluated, node.UUID)
	}
	if resp.Replicas != created.Replicas {
		rt.Fatalf("evaluate answered %d replicas, create stored %d", resp.Replicas, created.Replicas)
	}
	var stored planning.Placement
	if created.Placement != "" && created.Placement != "null" {
		if err := json.Unmarshal([]byte(created.Placement), &stored); err != nil {
			rt.Fatal(err)
		}
	}
	spec, err := planning.DeploymentParallelism(created, stored)
	if err != nil {
		rt.Fatalf("stored deployment has no readable parallelism: %v", err)
	}
	if !spec.Stated() {
		if resp.Parallelism == nil {
			return "unstated, evaluate reported none"
		}
		return "unstated, create stores none to compare"
	}
	perNode, err := spec.GPUsPerNode()
	if err != nil {
		rt.Fatalf("stored parallelism %+v: %v", spec, err)
	}
	if resp.Parallelism == nil {
		rt.Fatalf("create stored parallelism %+v (%d GPUs per node), evaluate reported none", spec, perNode)
	}
	got := resp.Parallelism
	if got.TP != spec.TP || got.PP != spec.PP || got.Nodes != spec.NNodes || got.GPUsPerNode != perNode {
		rt.Fatalf("evaluate reported tp=%d pp=%d nodes=%d gpus/node=%d; create stored tp=%d pp=%d dp=%d nodes=%d gpus/node=%d",
			got.TP, got.PP, got.Nodes, got.GPUsPerNode, spec.TP, spec.PP, spec.DP, spec.NNodes, perNode)
	}
	return fmt.Sprintf("stated, %d GPUs per node agree", perNode)
}
