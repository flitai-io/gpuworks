package deploy

import "testing"

func TestPlacementDataParallelUsesEngineFlags(t *testing.T) {
	for _, tc := range []struct {
		name, engine   string
		quick          int
		template, user []string
		want           int
	}{
		{"quick vllm", EngineVLLM, 2, nil, nil, 2},
		{"flag vllm", EngineVLLM, 1, nil, []string{"--data-parallel-size=2"}, 2},
		{"user overrides quick", EngineVLLM, 2, nil, []string{"--data-parallel-size=4"}, 4},
		{"sglang shared attention", EngineSGLang, 2, nil, []string{"--enable-dp-attention"}, 1},
		{"sglang separate copies", EngineSGLang, 1, nil, []string{"--dp-size", "2"}, 2},
		{"explicit agrees with quick", EngineVLLM, 2, nil, []string{"--data-parallel-size=2"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PlacementDataParallel(tc.engine, tc.quick, tc.template, tc.user)
			if err != nil || got != tc.want {
				t.Fatalf("got %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

func TestPlacementDataParallelRejectsUnknownSymbolicDegree(t *testing.T) {
	if _, err := PlacementDataParallel(EngineVLLM, 0, []string{"--data-parallel-size=$DP"}, nil); err == nil {
		t.Fatal("unknown DP allocated cards")
	}
}
