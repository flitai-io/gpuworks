package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/templateexport"
)

// A deployment of the built-in Qwen3-8B whose user raised max_model_len and
// added an argument is saved as a template, the YAML goes through the real
// evaluate and import routes into a store holding the whole built-in catalog,
// and a deployment made from the saved spec runs the same engine command.
func TestQwen3TemplateExportImportsAndRedeploysTheSameCommand(t *testing.T) {
	catalog, err := seed.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	var model inferencecatalog.InferenceModel
	for _, candidate := range catalog.Models {
		if candidate.ModelID == "qwen/qwen3-8b" {
			model = candidate
		}
	}
	if model.ModelID == "" {
		t.Fatal("qwen/qwen3-8b is not in the built-in catalog")
	}
	for _, specName := range []string{"bf16-vllm-nvidia-general", "bf16-sglang-nvidia-general"} {
		t.Run(specName, func(t *testing.T) {
			index := slices.IndexFunc(model.Specs, func(spec inferencecatalog.InferenceSpec) bool { return spec.Name == specName })
			if index < 0 {
				t.Fatalf("qwen/qwen3-8b has no spec %s", specName)
			}
			spec := model.Specs[index]
			// The create path: the form states max_model_len, the template's
			// args and the dictionary defaults do the rest.
			created, err := deploy.Materialize(deploy.MaterializeInput{Engine: spec.Engine, TemplateArgs: spec.Args, Raw: map[string]any{"max_model_len": 16384}})
			if err != nil {
				t.Fatal(err)
			}
			userArgs := []string{"--seed", "42"}
			out, err := templateexport.Build(templateexport.Input{
				DeploymentID: 21, DeploymentName: "qwen3-8b", Engine: spec.Engine, Image: spec.Image,
				Quick: created.Quick, TemplateArgs: created.TemplateResidual, UserArgs: userArgs, TemplateEnv: spec.Env,
				Template: &model, TemplateSpec: &spec,
				GPUs: []templateexport.GPUFact{{Name: "NVIDIA H20", Vendor: "nvidia", MemoryMiB: 97871, ComputeCap: "9.0"}}, GPUCount: 1,
				Checks: []templateexport.Check{{ID: templateexport.CheckOperationReply, Operation: "openai.chat", Endpoint: "/v1/chat/completions", Status: proto.CheckPass, Code: messages.CheckTemplateOperationAnswered, HTTPStatus: 200}},
			})
			if err != nil {
				t.Fatal(err)
			}

			st, router, token := catalogRouter(t)
			rows, err := inferencecatalog.ToFlatStoreModels(catalog)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.ReconcileInferenceCatalog(rows); err != nil {
				t.Fatal(err)
			}
			if status, body := postYAML(router, token, "/api/v1/inference-templates/evaluate", out.YAML); status != http.StatusOK || !strings.Contains(body, `"valid":true`) {
				t.Fatalf("evaluate = %d %s", status, body)
			}
			if status, body := postYAML(router, token, "/api/v1/inference-templates/import", out.YAML); status != http.StatusOK {
				t.Fatalf("import = %d %s", status, body)
			}
			row, err := st.InferenceModelByModelID("qwen/qwen3-8b")
			if err != nil {
				t.Fatal(err)
			}
			upstream, saved := 0, false
			for _, stored := range row.Specs {
				if stored.Source == "upstream" {
					upstream++
				}
				saved = saved || (stored.Source == "user" && stored.Name == out.Model.Specs[0].Name)
			}
			if upstream != len(model.Specs) || !saved {
				t.Fatalf("specs after import: %d upstream (want %d), saved user spec %v", upstream, len(model.Specs), saved)
			}

			// A deployment made from the saved spec, through the create path.
			savedSpec := out.Model.Specs[0]
			again, err := deploy.Materialize(deploy.MaterializeInput{Engine: savedSpec.Engine, TemplateArgs: savedSpec.Args})
			if err != nil {
				t.Fatal(err)
			}
			before := engineCommand(t, spec.Engine, created.Quick, created.TemplateResidual, userArgs)
			after := engineCommand(t, savedSpec.Engine, again.Quick, again.TemplateResidual, nil)
			if !slices.Equal(before, after) {
				t.Fatalf("command from the saved spec differs:\n%v\nwant\n%v", after, before)
			}
		})
	}
}

// engineCommand is the engine's command line for one placeholder launch, its
// tokens sorted: engines read flags by name, so the order the layers emit them
// in is not part of what the command means.
func engineCommand(t *testing.T, engine string, quick deploy.LaunchConfig, templateArgs, userArgs []string) []string {
	t.Helper()
	selected, err := deploy.SelectEngine(engine)
	if err != nil {
		t.Fatal(err)
	}
	launch := selected.Build(deploy.Params{ModelPath: "/model", ServedName: "qwen3-8b", APIPort: 30000, AssignedGPUs: 1, TensorParallel: 1,
		Quick: quick, TemplateArgs: templateArgs, ExtraArgs: userArgs, PositionalModel: true})
	command := append(append([]string(nil), launch.Entrypoint...), launch.Argv...)
	sort.Strings(command)
	return command
}

func catalogRouter(t *testing.T) (*store.Store, http.Handler, string) {
	t.Helper()
	f := newTemplateFixture(t)
	signer, token := adminSession(t, f.st)
	return f.st, api.Router(api.Deps{Store: f.st, AuthSigner: signer, CatalogTemplates: catalogTemplates{store: f.st,
		probe: func(context.Context, uint, string, string, map[string]any) templateexport.ProbeReply {
			return templateexport.ProbeReply{HTTPStatus: 200}
		}}}), token
}

// adminSession signs a management session for a fresh administrator.
func adminSession(t *testing.T, st *store.Store) (*auth.TokenSigner, string) {
	t.Helper()
	signer, err := auth.NewTokenSigner(filepath.Join(t.TempDir(), "auth-secret"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.EnsureAdmin("admin", "gpuworks-test-password-2026"); err != nil {
		t.Fatal(err)
	}
	user, err := st.AuthenticateUser("admin", "gpuworks-test-password-2026")
	if err != nil {
		t.Fatal(err)
	}
	_, session, err := st.CreateManagementSession(user, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Sign(auth.Claims{UserID: user.ID, Username: user.Username, Role: user.Role, SessionID: session.TokenHash})
	if err != nil {
		t.Fatal(err)
	}
	return signer, token
}

func postYAML(router http.Handler, token, path, document string) (int, string) {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(document))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/yaml")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}
