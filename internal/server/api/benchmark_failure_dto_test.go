package api

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/bench/dataset"
)

// A failed run's code reaches the API as "failure": the code the last status
// report carried (persisted in Progress) or the server-restart code; an
// uncoded or unfinished run has none.
func TestBenchmarkDTOFailureWire(t *testing.T) {
	st, err := store.Open("file:benchmark_failure_dto?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	handlers := &apiHandlers{d: Deps{Store: st}}
	failed := proto.BenchmarkStatus{State: "failed", Message: "load the built-in multiturn dataset: EOF", Code: dataset.LoadErrorCode, Params: map[string]any{"dataset": "multiturn"}}
	progress, _ := json.Marshal(failed)
	uncoded, _ := json.Marshal(proto.BenchmarkStatus{State: "failed", Message: "all 3 warmup requests failed"})
	// Through the store, as a server-origin run's terminal report lands.
	run, err := st.CreateBenchmarkRun(&store.BenchmarkRun{Name: "dataset failure", TargetKind: "external", LoadOrigin: "server", State: "running"})
	if err != nil {
		t.Fatal(err)
	}
	reported := failed
	reported.ID = run.ID
	if err := st.ApplyBenchmarkStatus(0, reported); err != nil {
		t.Fatal(err)
	}
	persisted, err := st.GetBenchmarkRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := handlers.benchmarkDTO(persisted, false).Failure; got == nil || got.Code != dataset.LoadErrorCode || got.Params["dataset"] != "multiturn" || got.Message != failed.Message {
		t.Fatalf("persisted failure = %+v", got)
	}
	for _, tc := range []struct {
		name string
		run  store.BenchmarkRun
		want string
	}{
		{"coded", store.BenchmarkRun{State: "failed", Progress: string(progress), Error: failed.Message},
			`{"code":"BENCHMARK_DATASET_LOAD_FAILED","params":{"dataset":"multiturn"},"message":"load the built-in multiturn dataset: EOF"}`},
		{"server restarted", store.BenchmarkRun{State: "failed", Error: store.BenchmarkErrorServerRestarted},
			`{"code":"BENCHMARK_SERVER_RESTARTED","message":"The server restarted and interrupted the evaluation; completed points were retained."}`},
		{"uncoded", store.BenchmarkRun{State: "failed", Progress: string(uncoded), Error: "all 3 warmup requests failed"}, ``},
		{"done", store.BenchmarkRun{State: "done", Progress: string(progress)}, ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := tc.run
			raw, err := json.Marshal(handlers.benchmarkDTO(&run, false))
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			if got := string(body["failure"]); got != tc.want {
				t.Fatalf("failure = %s\nwant %s", got, tc.want)
			}
		})
	}
}
