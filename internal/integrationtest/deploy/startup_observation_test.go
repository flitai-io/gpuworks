package deploy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

type startupObservedInstance struct {
	UUID     string         `json:"uuid"`
	State    string         `json:"state"`
	Measured map[string]any `json:"measured"`
}

func startupObservedDeployment(t *testing.T, api *testenv.APIClient, id uint) []startupObservedInstance {
	t.Helper()
	response, err := api.Get(fmt.Sprintf("/api/v1/deployments/%d", id))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("deployment GET status=%d", response.StatusCode)
	}
	var out struct {
		Instances []startupObservedInstance `json:"instances"`
	}
	if err := json.NewDecoder(response.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Instances
}

func createStartupObservedDeployment(t *testing.T, env *testenv.Env, node *testenv.Node) (*testenv.APIClient, uint) {
	t.Helper()
	env.WaitNodeOnline(t, node.Name)
	model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "fixture/startup-observation", Revision: "main", Name: "startup-observation", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "startup-observation")}); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	body := []byte(fmt.Sprintf(`{"name":"startup-observation","served_name":"startup-observation","model_id":%d,"node_uuid":%q,"engine":"vllm","image":"example/startup:v1","quick_params":{"max_model_len":512}}`, model.ID, node.UUID))
	response, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%s err=%v", response.StatusCode, data, err)
	}
	var out struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.ID == 0 {
		t.Fatalf("create body=%s err=%v", data, err)
	}
	return api, out.ID
}

// The public response retains the original successful startup measurement
// after an agent restart; adopting the same container must not replace it
// with the short time needed to probe an already-running service.
func TestStartupObservationIsMeasuredOnceAndSurvivesAgentAdoption(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("startup-observation", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("fake"))
	var healthRequests atomic.Int32
	node.Driver.SetEngineHandler(func(inst testenv.EngineInstance) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/health":
				healthRequests.Add(1)
				time.Sleep(100 * time.Millisecond)
				w.WriteHeader(http.StatusOK)
			case "/v1/models":
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": inst.Model}}})
			default:
				http.NotFound(w, r)
			}
		})
	})
	node.Driver.OnLogs(func(_ string, follow bool, _ int) (io.ReadCloser, error, bool) {
		if follow {
			return nil, nil, false
		}
		return io.NopCloser(strings.NewReader("Model loading took 1.0 GiB and 0.02 seconds\ntorch.compile takes 0.01 s in total\nGraph capturing finished in 0.01 secs, took 0.01 GiB\n")), nil, true
	})
	api, id := createStartupObservedDeployment(t, env, node)
	var running startupObservedInstance
	env.Eventually(t, 20*time.Second, func() bool {
		instances := startupObservedDeployment(t, api, id)
		if len(instances) == 1 && instances[0].State == proto.InstRunning {
			running = instances[0]
			return true
		}
		return false
	})
	elapsed, known := running.Measured["StartupSeconds"].(float64)
	if !known || elapsed < 0.1 {
		t.Fatalf("new startup duration=%v, want observed >=100ms; measured=%v", running.Measured["StartupSeconds"], running.Measured)
	}
	for field, want := range map[string]float64{"ModelLoadingSeconds": 0.02, "CompilationSeconds": 0.01, "GraphCaptureSeconds": 0.01} {
		if got := running.Measured[field]; got != want {
			t.Errorf("%s=%v want=%v", field, got, want)
		}
	}
	response, err := api.Get("/api/v1/instances/" + running.UUID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	var timeline struct {
		Items []struct {
			Params map[string]any `json:"params"`
		} `json:"items"`
	}
	err = json.NewDecoder(response.Body).Decode(&timeline)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("timeline status=%d err=%v", response.StatusCode, err)
	}
	var healthStart bool
	for _, event := range timeline.Items {
		if event.Params["phase"] == proto.PhaseHealthCheck && event.Params["state"] == proto.InstStarting {
			healthStart = true
		}
	}
	if !healthStart {
		t.Fatal("a sub-10-second startup lost its initial health-wait milestone")
	}
	before := healthRequests.Load()
	// Model the last control-plane startup status being replayed after the
	// transport stops, while the runtime still holds the healthy container.
	// The next running API response must therefore follow adoption's report.
	env.StopNode(t, node.Name)
	if err := env.Store().SetInstanceState(running.UUID, proto.InstStarting, "waiting after agent restart"); err != nil {
		t.Fatal(err)
	}
	env.RestartNode(t, node.Name)
	env.WaitNodeOnline(t, node.Name)
	env.Eventually(t, 20*time.Second, func() bool { return healthRequests.Load() > before })
	// The report follows the health request. Wait for a fresh public instance
	// response after the response/identity check, rather than inspecting agent state.
	env.Eventually(t, 10*time.Second, func() bool {
		instances := startupObservedDeployment(t, api, id)
		return len(instances) == 1 && instances[0].UUID == running.UUID && instances[0].State == proto.InstRunning && instances[0].Measured["StartupSeconds"] == elapsed
	})
}

func TestStartupObservationDoesNotTurnFailureOrCancellationIntoReadyTime(t *testing.T) {
	for _, cancelStartup := range []bool{false, true} {
		name := "timeout"
		if cancelStartup {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			env := testenv.New(t)
			env.SetControllerRetryPolicy(time.Hour, 1)
			env.SetControllerStartTimeout(1)
			node := env.AddNode("startup-"+name, testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("fake"))
			var healthRequests atomic.Int32
			node.Driver.SetEngineHandler(func(testenv.EngineInstance) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/health" {
						http.NotFound(w, r)
						return
					}
					healthRequests.Add(1)
					if cancelStartup {
						<-r.Context().Done()
						return
					}
					w.WriteHeader(http.StatusServiceUnavailable)
				})
			})
			api, id := createStartupObservedDeployment(t, env, node)
			env.Eventually(t, 20*time.Second, func() bool { return healthRequests.Load() > 0 })
			if cancelStartup {
				response, err := api.PostJSON(fmt.Sprintf("/api/v1/deployments/%d/stop", id), nil)
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if response.StatusCode != http.StatusOK {
					t.Fatalf("stop status=%d", response.StatusCode)
				}
			}
			env.Eventually(t, 20*time.Second, func() bool {
				instances := startupObservedDeployment(t, api, id)
				for _, instance := range instances {
					if _, present := instance.Measured["StartupSeconds"]; present {
						t.Fatalf("%s produced a successful startup time: %+v", name, instance)
					}
					if (!cancelStartup && instance.State != proto.InstFailed) || (cancelStartup && instance.State != proto.InstStopped) {
						return false
					}
					if !cancelStartup {
						elapsed, known := instance.Measured["StartupTimeoutSeconds"].(float64)
						if !known || elapsed <= 0 || instance.Measured["Found"] != false {
							t.Fatalf("timeout lost its censored observation or invented memory: %+v", instance)
						}
					} else if _, present := instance.Measured["StartupTimeoutSeconds"]; present {
						t.Fatalf("cancellation was presented as a health timeout: %+v", instance)
					}
				}
				return cancelStartup || len(instances) > 0
			})
			if !cancelStartup {
				instances := startupObservedDeployment(t, api, id)
				env.StopNode(t, node.Name)
				// The controller's retirement replaces the state and message.
				// This fixture transition must not erase the measured lower bound.
				if err := env.Store().SetInstanceState(instances[0].UUID, proto.InstStopped, "retired attempt"); err != nil {
					t.Fatal(err)
				}
				retired := startupObservedDeployment(t, api, id)
				if len(retired) != 1 || retired[0].State != proto.InstStopped || retired[0].Measured["StartupTimeoutSeconds"] != instances[0].Measured["StartupTimeoutSeconds"] {
					t.Fatalf("retirement erased the startup timeout: %+v", retired)
				}
			}
		})
	}
}
