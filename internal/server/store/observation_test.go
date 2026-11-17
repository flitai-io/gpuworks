package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func openObservationStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "observation.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func observationModel(id, contract string) InferenceModel {
	return InferenceModel{ModelID: id, Source: "user", Specs: []InferenceSpecRow{{
		Name: "general", Engine: "callable", Image: "img", Quantization: "bf16",
		WeightSources: `[]`, GPUs: `{}`, Args: `[]`, Env: `{}`, APIContract: contract,
	}}}
}

func waitTemplateEvent(t *testing.T, sub *bus.Subscription) string {
	t.Helper()
	select {
	case ev := <-sub.C():
		return ev.Key
	case <-time.After(2 * time.Second):
		t.Fatal("no inference template event")
		return ""
	}
}

// Every catalog write announces which template rows changed, after commit, so
// the controller can re-resolve the observation of their running instances.
func TestCatalogWritesPublishTheChangedTemplates(t *testing.T) {
	st := openObservationStore(t)
	sub := st.Bus.Subscribe(8, InferenceTemplateTopic)
	defer sub.Close()

	if err := st.ReconcileInferenceCatalog([]InferenceModel{{ModelID: "org/up", Source: "upstream", Specs: observationModel("x", "").Specs}}); err != nil {
		t.Fatal(err)
	}
	if key := waitTemplateEvent(t, sub); key != InferenceTemplatesAll {
		t.Fatalf("reconcile key = %q", key)
	}

	if err := st.ImportUserInferenceCatalog([]InferenceModel{observationModel("org/custom", "")}, false); err != nil {
		t.Fatal(err)
	}
	model, err := st.InferenceModelByModelID("org/custom")
	if err != nil {
		t.Fatal(err)
	}
	if key := waitTemplateEvent(t, sub); key != strconv.FormatUint(uint64(model.ID), 10) {
		t.Fatalf("import key = %q, want the row %d", key, model.ID)
	}

	spec := model.Specs[0]
	spec.ID, spec.APIContract = 0, `{"operations":[{"type":"openai.chat"}]}`
	if err := st.UpsertInferenceSpec(&spec); err != nil {
		t.Fatal(err)
	}
	if key := waitTemplateEvent(t, sub); key != strconv.FormatUint(uint64(model.ID), 10) {
		t.Fatalf("upsert key = %q", key)
	}

	// A refused write publishes nothing.
	if err := st.ImportUserInferenceCatalog([]InferenceModel{observationModel("org/custom", "")}, false); !errors.Is(err, ErrInferenceCatalogConflict) {
		t.Fatalf("conflict = %v", err)
	}
	select {
	case ev := <-sub.C():
		t.Fatalf("event after a refused import: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestTemplateSpecAPIContractReadsOneSpecByIdentity(t *testing.T) {
	st := openObservationStore(t)
	contract := `{"operations":[{"type":"openai.chat"}],"metrics":{"map":{"x":"requests_running"}}}`
	if err := st.ImportUserInferenceCatalog([]InferenceModel{observationModel("org/tf", contract)}, false); err != nil {
		t.Fatal(err)
	}
	model, err := st.InferenceModelByModelID("org/tf")
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.TemplateSpecAPIContract(model.ID, "user", "general")
	if err != nil || got != contract {
		t.Fatalf("contract = %q, %v", got, err)
	}
	for _, miss := range []struct {
		id           uint
		source, name string
	}{{model.ID, "upstream", "general"}, {model.ID, "user", "other"}, {model.ID + 99, "user", "general"}} {
		if _, err := st.TemplateSpecAPIContract(miss.id, miss.source, miss.name); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%+v: err = %v, want ErrNotFound", miss, err)
		}
	}
	plan := explainPlan(t, st, "SELECT api_contract FROM inference_specs WHERE model_id = 1 AND source = 'user' AND name = 'general' LIMIT 1")
	if !strings.Contains(plan, "USING INDEX") || strings.Contains(plan, "SCAN") {
		t.Fatalf("spec lookup plan = %s", plan)
	}
}

func explainPlan(t *testing.T, st *Store, query string) string {
	t.Helper()
	var rows []struct {
		Detail string
	}
	if err := st.DB().Raw("EXPLAIN QUERY PLAN " + query).Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	details := make([]string, len(rows))
	for i, row := range rows {
		details[i] = row.Detail
	}
	return strings.Join(details, "; ")
}

func observedInstance(t *testing.T, st *Store, deploymentID uint, uuid string, spec proto.InstanceDesiredState) Instance {
	t.Helper()
	spec.UUID = uuid
	raw, _ := json.Marshal(spec)
	instance, err := st.CreateInstance(&Instance{UUID: uuid, DeploymentID: deploymentID, State: proto.InstRunning, DesiredStateJSON: string(raw)})
	if err != nil {
		t.Fatal(err)
	}
	return *instance
}

func TestDeploymentsForObservationScopesByTemplateAndSkipsDeleting(t *testing.T) {
	st := openObservationStore(t)
	create := func(name string, template uint, state DeploymentState) *Deployment {
		dep, err := st.CreateDeployment(&Deployment{Name: name, TemplateID: template, State: state})
		if err != nil {
			t.Fatal(err)
		}
		return dep
	}
	a := create("a", 7, DeployActive)
	b := create("b", 8, DeployActive)
	empty := create("no-instances", 7, DeployActive)
	deleting := create("deleting", 7, DeployDeleting)
	observedInstance(t, st, a.ID, "a-1", proto.InstanceDesiredState{})
	observedInstance(t, st, a.ID, "a-2", proto.InstanceDesiredState{})
	observedInstance(t, st, b.ID, "b-1", proto.InstanceDesiredState{})
	observedInstance(t, st, deleting.ID, "d-1", proto.InstanceDesiredState{})
	_ = empty

	summary := func(observed []ObservedDeployment) string {
		var parts []string
		for _, o := range observed {
			var uuids []string
			for _, in := range o.Instances {
				uuids = append(uuids, in.UUID)
			}
			sort.Strings(uuids)
			parts = append(parts, o.Deployment.Name+"="+strings.Join(uuids, ","))
		}
		sort.Strings(parts)
		return strings.Join(parts, " ")
	}
	for _, tc := range []struct {
		templates []uint
		want      string
	}{
		{[]uint{7}, "a=a-1,a-2"},
		{[]uint{8, 9}, "b=b-1"},
		{nil, "a=a-1,a-2 b=b-1"},
		{[]uint{}, ""},
	} {
		got, err := st.DeploymentsForObservation(tc.templates)
		if err != nil {
			t.Fatal(err)
		}
		if summary(got) != tc.want {
			t.Fatalf("templates %v = %q, want %q", tc.templates, summary(got), tc.want)
		}
	}

	// The template-scoped read uses the template index, the instance read the
	// deployment index: a template edit never scans the fleet.
	for _, query := range []string{
		"SELECT * FROM deployments WHERE desired_state <> 'deleting' AND template_id IN (7)",
		"SELECT * FROM instances WHERE deployment_id IN (1,2)",
	} {
		if plan := explainPlan(t, st, query); !strings.Contains(plan, "USING INDEX") {
			t.Fatalf("%s: plan = %s", query, plan)
		}
	}
}

func TestRewriteObservationsStoresOnlyChangedInstancesAtomically(t *testing.T) {
	st := openObservationStore(t)
	dep, err := st.CreateDeployment(&Deployment{Name: "obs"})
	if err != nil {
		t.Fatal(err)
	}
	launch := proto.InstanceDesiredState{Engine: "callable", MetricPath: "/metrics", MetricPrefix: "gpuworks_callable"}
	first := observedInstance(t, st, dep.ID, "o-1", launch)
	second := observedInstance(t, st, dep.ID, "o-2", launch)
	observation := &proto.InstanceObservation{MetricPath: "/metrics", MetricRules: []proto.MetricRule{{Source: "x", Series: "gpuworks_infer_requests_running"}}}

	// A pass that changes nothing leaves the instance generation alone; one
	// that rewrites a row moves it, though it publishes no event.
	gen := st.Bus.Generation("instance")
	if _, err := st.RewriteObservations([]Instance{first, second}, func(*Instance, *proto.InstanceDesiredState) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if got := st.Bus.Generation("instance"); got != gen {
		t.Fatalf("unchanged pass moved the generation %d -> %d", gen, got)
	}
	changed, err := st.RewriteObservations([]Instance{first, second}, func(instance *Instance, spec *proto.InstanceDesiredState) bool {
		if instance.UUID != "o-1" {
			return false
		}
		spec.Observation = observation
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0].UUID != "o-1" {
		t.Fatalf("changed = %+v", changed)
	}
	if got := st.Bus.Generation("instance"); got != gen+1 {
		t.Fatalf("rewriting pass: generation %d -> %d, want one step", gen, got)
	}
	if spec, _ := changed[0].DesiredState(); spec.Observation == nil || spec.Engine != "callable" {
		t.Fatalf("returned state = %+v", spec)
	}
	stored := func(uuid string) proto.InstanceDesiredState {
		instance, err := st.InstanceByUUID(uuid)
		if err != nil {
			t.Fatal(err)
		}
		spec, err := instance.DesiredState()
		if err != nil {
			t.Fatal(err)
		}
		return spec
	}
	if spec := stored("o-1"); spec.Observation == nil || spec.Observation.MetricRules[0].Source != "x" || spec.MetricPrefix != "gpuworks_callable" {
		t.Fatalf("stored o-1 = %+v", spec)
	}
	if spec := stored("o-2"); spec.Observation != nil {
		t.Fatalf("unchanged o-2 rewritten: %+v", spec)
	}

	// A failure part way rolls the whole pass back: the instance written
	// before the failing one keeps its old state.
	corrupt := second
	corrupt.DesiredStateJSON = "{"
	if _, err := st.RewriteObservations([]Instance{first, corrupt}, func(_ *Instance, spec *proto.InstanceDesiredState) bool {
		spec.Observation = &proto.InstanceObservation{MetricPath: "/rolled-back"}
		return true
	}); err == nil {
		t.Fatal("a corrupt desired state did not fail the pass")
	}
	if spec := stored("o-1"); spec.Observation == nil || spec.Observation.MetricPath != "/metrics" {
		t.Fatalf("o-1 after rollback = %+v", spec.Observation)
	}
}

func TestTemplateSpecAPIContractsReadsManySpecsInOneQuery(t *testing.T) {
	st := openObservationStore(t)
	first := `{"operations":[{"type":"openai.chat"}],"request_stats":{"tensorfold.prefill_s":"prefill_seconds"}}`
	second := `{"operations":[{"type":"openai.chat"}]}`
	if err := st.ImportUserInferenceCatalog([]InferenceModel{observationModel("org/a", first), observationModel("org/b", second)}, false); err != nil {
		t.Fatal(err)
	}
	a, err := st.InferenceModelByModelID("org/a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.InferenceModelByModelID("org/b")
	if err != nil {
		t.Fatal(err)
	}
	keyA := TemplateSpecKey{TemplateID: a.ID, Source: "user", Name: "general"}
	keyB := TemplateSpecKey{TemplateID: b.ID, Source: "user", Name: "general"}
	missing := TemplateSpecKey{TemplateID: a.ID, Source: "upstream", Name: "general"}
	got, err := st.TemplateSpecAPIContracts([]TemplateSpecKey{keyA, keyB, missing, keyA})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[keyA] != first || got[keyB] != second {
		t.Fatalf("contracts = %+v", got)
	}
	if empty, err := st.TemplateSpecAPIContracts(nil); err != nil || len(empty) != 0 {
		t.Fatalf("no keys = %+v, %v", empty, err)
	}
	plan := explainPlan(t, st, "SELECT model_id, source, name, api_contract FROM inference_specs WHERE model_id IN (1, 2)")
	if !strings.Contains(plan, "USING INDEX") {
		t.Fatalf("batched spec plan = %s", plan)
	}
}
