package deployment

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A request that names a model is either bound to that model row or refused
// with a stable code; the server validates the user's choice and never
// substitutes it (docs/adr/0018-explicit-deployment-acquisition.md). The
// world draws the selected model's provider, repository id, revision and copy
// location, the spec's weight sources and the request's provider and
// placement independently - the identity bugs this guards against (a local
// copy dropped for the template's online source, an explicit model id
// replaced, a provider that differs from the selected model's) each needed a
// combination a hand-picked fixture did not have.
//
// Checked on every iteration:
//   - Evaluate of a request that names its model leaves the model and
//     model-file rows exactly as it found them (checkEvaluationRows).
//   - An evaluation refusal carries typed codes; one that names the
//     pinned node's acquisition and finds the selected copy usable there
//     forecasts no download.
//   - Create either refuses with a ProductError and writes no model row, or
//     stores the selected model row (in the deployment and its catalog
//     snapshot), registers no other model row, and - when the node it landed
//     on reads the selected copy already - records no new model file.
//   - A request that names no model (a third of the draws) is held to the
//     reuse rule instead (reusableRows), by Evaluate and by Create: a
//     registered row the node already reads and that serves the spec is not
//     registered again beside - the shape of an imported copy with no
//     revision next to a spec that pins one.
//
// go test ./internal/server/deployment/ -run TestSelectedModelIsNeverSubstituted -rapid.checks=1000 -v
func TestSelectedModelIsNeverSubstituted(t *testing.T) {
	var rows, evaluations, creates outcomeTally
	rapid.Check(t, func(rt *rapid.T) {
		w := genAcquisitionWorld(rt)
		rt.Logf("world: %s", w)
		f := buildWorld(rt, w)
		defer func() { _ = f.st.Close() }()
		ctx := context.Background()

		initial := f.snapshot(rt)
		resp, evalErr := f.svc.Evaluate(ctx, dto.EvaluateRequest{DeployRequest: f.request(w)})
		before := f.snapshot(rt)
		rows.add(checkEvaluationRows(rt, f, w, resp, initial, before))
		evaluations.add(checkSelectionEvaluation(rt, f, w, resp, evalErr))

		created, createErr := f.svc.Create(ctx, f.request(w))
		after := f.snapshot(rt)
		if createErr != nil {
			code, typed := stableCode(createErr)
			if !typed {
				rt.Fatalf("create refused without a stable code (kind %d): %v", KindOf(createErr), createErr)
			}
			if fmt.Sprint(after.models) != fmt.Sprint(before.models) {
				rt.Fatalf("refused create changed model rows: before %v after %v", before.models, after.models)
			}
			creates.add("refused " + code)
			return
		}
		node, err := f.st.NodeByID(created.NodeID)
		if err != nil {
			rt.Fatal(err)
		}
		if !w.NamesModel {
			creates.add(checkTemplateReuse(rt, f, w, before, after, created, node))
			return
		}
		if created.ModelID != f.selected.ID {
			rt.Fatalf("create bound model %d, the request selected %d", created.ModelID, f.selected.ID)
		}
		snapshot, err := deploy.DecodeCatalogSelection([]byte(created.CatalogSelectionSnapshot))
		if err != nil {
			rt.Fatal(err)
		}
		if !strings.EqualFold(snapshot.WeightProvider, f.selected.Source) || snapshot.WeightRepoID != f.selected.SourceID || snapshot.WeightRevision != f.selected.Revision {
			rt.Fatalf("catalog snapshot names %s/%s@%s, the selected model is %s/%s@%s", snapshot.WeightProvider, snapshot.WeightRepoID, snapshot.WeightRevision,
				f.selected.Source, f.selected.SourceID, f.selected.Revision)
		}
		if fmt.Sprint(after.models) != fmt.Sprint(before.models) {
			rt.Fatalf("create registered a model beside the selected one: before %v after %v", before.models, after.models)
		}
		usable := f.usableOn(rt, f.selected, node)
		if fresh := before.newFiles(after); usable && len(fresh) > 0 {
			rt.Fatalf("node %s reads the selected copy, yet create recorded %v", node.UUID, fresh)
		}
		creates.add(fmt.Sprintf("created usable-on-node=%t", usable))
	})
	rows.log(t, "evaluate rows")
	evaluations.log(t, "evaluate")
	creates.log(t, "create")
}

// checkSelectionEvaluation holds one evaluation to the selection property and
// names its outcome class.
func checkSelectionEvaluation(rt *rapid.T, f *worldFixture, w acquisitionWorld, resp dto.EvaluateResponse, err error) string {
	if err != nil {
		code, typed := stableCode(err)
		if !typed {
			rt.Fatalf("evaluate failed without a stable code: %v", err)
		}
		return "error " + code
	}
	if len(resp.Errors) > 0 {
		if !stableFieldErrors(resp.Errors) {
			rt.Fatalf("evaluate answered untyped field errors: %+v", resp.Errors)
		}
		return "field errors " + fieldErrorCodes(resp.Errors)
	}
	if resp.Acquisition != nil && w.NamesModel {
		for _, node := range resp.Acquisition.Nodes {
			if node.NodeUUID != f.target.UUID {
				continue
			}
			if f.usableOn(rt, f.selected, f.target) && node.WeightsPlan == forecastDownload {
				rt.Fatalf("the target reads the selected copy, yet evaluate forecasts a download from %s", node.WeightsProvider)
			}
		}
	}
	return fmt.Sprintf("ok=%t", resp.OK)
}

// checkEvaluationRows holds an evaluation to leaving no persistent trace. A
// request that names its model leaves every model and model-file row as it
// was. One that names none may register the row of the spec's weight source
// the preview resolves (evaluation has always done so; the create then binds
// it), but - when it names its node - never beside a reusable row
// (reusableRows) on that node, and it records no model file either way.
func checkEvaluationRows(rt *rapid.T, f *worldFixture, w acquisitionWorld, resp dto.EvaluateResponse, before, after rowSnapshot) string {
	if w.NamesModel {
		if !before.equal(after) {
			rt.Fatalf("evaluate changed persistent rows:\nbefore %+v\nafter  %+v", before, after)
		}
		return "rows unchanged"
	}
	if fmt.Sprint(before.files) != fmt.Sprint(after.files) {
		rt.Fatalf("evaluate changed model files:\nbefore %v\nafter  %v", before.files, after.files)
	}
	if fmt.Sprint(before.models) == fmt.Sprint(after.models) {
		return "rows unchanged"
	}
	if w.Shape.Automatic {
		// Accepted behaviour: an automatic draft has no node when evaluation
		// resolves the template's weight source, so it cannot see that the
		// node it then selects reads an imported copy, and registers the
		// source's own row beside it. The create picks its node first and
		// reuses the copy (checked below); the row evaluation left stays.
		return "no model named, automatic: evaluate registered the template's row (accepted)"
	}
	if reusable := reusableRows(rt, f, w, f.target); len(reusable) > 0 {
		rt.Fatalf("node %s reads registered row(s) %v that serve the spec, yet evaluate registered another: before %v after %v", f.target.UUID, reusable, before.models, after.models)
	}
	return "no model named: evaluate registered the template's row"
}

// reusableRows are the registered rows the reuse rule of ADR 0018
// (2026-10-27 addendum) says node must reuse: one node already reads, at the
// request's provider (any declared one when the request names none), whose
// repository matches a declared source by the server's own rule
// (TemplateWeightMatchesModel) and whose revision is that source's or unknown
// at a size the server accepts as the spec's (sameWeightSize).
func reusableRows(rt *rapid.T, f *worldFixture, w acquisitionWorld, node *store.Node) []uint {
	var reusable []uint
	for _, row := range []*store.Model{f.selected, f.decoy} {
		if row == nil || !f.usableOn(rt, row, node) || !sameWeightSize(row.WeightBytes, worldSpecWeightBytes) {
			continue
		}
		for _, source := range w.Sources {
			declared := weightSource{Provider: source.Provider, RepoID: worldTemplateRepo}
			if source.Pinned {
				declared.Revision = worldTemplateRevision
			}
			if w.RequestProvider == requestDeclaredProvider {
				requested := w.Sources[w.RequestAt].Provider
				if !strings.EqualFold(declared.Provider, requested) || !strings.EqualFold(row.Source, requested) {
					continue
				}
			}
			if TemplateWeightMatchesModel(declared, row) && (row.Revision == "" || row.Revision == declared.Revision) {
				reusable = append(reusable, row.ID)
				break
			}
		}
	}
	return reusable
}

// checkTemplateReuse holds a create that named no model to the reuse rule:
// with a reusable row (reusableRows) on the node it landed on, no new row is
// registered beside it.
func checkTemplateReuse(rt *rapid.T, f *worldFixture, w acquisitionWorld, before, after rowSnapshot, created *store.Deployment, node *store.Node) string {
	reusable := reusableRows(rt, f, w, node)
	if len(reusable) == 0 {
		return "template model, no reusable row"
	}
	if fmt.Sprint(after.models) != fmt.Sprint(before.models) {
		rt.Fatalf("node %s reads registered row(s) %v that serve the spec, yet create registered another: before %v after %v (deployment model %d)",
			node.UUID, reusable, before.models, after.models, created.ModelID)
	}
	return "template model, reused a registered row"
}

// The selection world's own generator, checked once so a draw that breaks the
// fixture (an upsert that merges the selected and decoy rows, say) is a
// failure here rather than a confusing property counterexample.
func TestAcquisitionWorldMaterializes(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		w := genAcquisitionWorld(rt)
		f := buildWorld(rt, w)
		defer func() { _ = f.st.Close() }()
		model, err := f.st.GetModel(f.selected.ID)
		if err != nil || model.Source != w.selectedModel().Source || model.SourceID != w.selectedModel().SourceID {
			rt.Fatalf("selected model = %+v, %v; want %+v", model, err, w.selectedModel())
		}
		ready := f.usableOn(rt, f.selected, f.target)
		if want := w.Copy == copyTargetLocal || w.Copy == copyShared; ready != want {
			rt.Fatalf("copy %s: target reads selected = %t, want %t", copyKindNames[w.Copy], ready, want)
		}
		if f.decoy != nil && !f.usableOn(rt, f.decoy, f.target) {
			rt.Fatalf("decoy %+v is not ready on the target", f.decoy)
		}
	})
}
