package deploy

import (
	"reflect"
	"testing"
)

func TestVLLMContextAutoFollowsTheImageVersion(t *testing.T) {
	command, err := ResolveEffectiveCommand(ContainerOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, engine, version, want string
		template, extra             []string
		raw                         map[string]any
		unset                       []string
	}{
		{name: "first supported release", version: "0.14.0", want: "auto"},
		{name: "modern release", version: "0.30.0", want: "auto"},
		{name: "vendor release", version: "0.26.0rc1", want: "auto"},
		{name: "old release", version: "0.13.0"},
		{name: "unknown release"},
		{name: "dated nightly is not a release", version: "nightly-20261116"},
		{name: "dated dev is not a release", version: "dev-2026-11-16"},
		{name: "sglang", engine: EngineSGLang, version: "0.5.21"},
		{name: "omni stages keep their own limits", version: "0.30.0", template: []string{"--omni"}},
		{name: "template limit", version: "0.30.0", template: []string{"--max-model-len", "32768"}, want: "32768"},
		{name: "explicit field", version: "0.30.0", raw: map[string]any{"max_model_len": 4096}, want: "4096"},
		{name: "explicit zero keeps engine default", version: "0.30.0", raw: map[string]any{"max_model_len": 0}},
		{name: "explicit unset", version: "0.30.0", unset: []string{"max_model_len"}},
		{name: "extra auto replaces template limit", version: "0.30.0", template: []string{"--max-model-len", "32768"}, extra: []string{"--max-model-len=auto"}, want: "auto"},
		{name: "extra negative sentinel", version: "0.30.0", extra: []string{"--max-model-len", "-1"}, want: "auto"},
		{name: "typed auto", version: "0.30.0", raw: map[string]any{"max_model_len": "auto"}, want: "auto"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := tc.engine
			if engine == "" {
				engine = EngineVLLM
			}
			out, err := Materialize(MaterializeInput{Engine: engine, Command: &command, EngineVersion: tc.version, TemplateArgs: tc.template, ExtraArgs: tc.extra, Raw: tc.raw, Unset: tc.unset})
			if err != nil {
				t.Fatal(err)
			}
			p := goldenParams(out.Quick)
			p.TemplateArgs, p.ExtraArgs = out.TemplateResidual, out.UserResidual
			launch := mustEngine(t, engine).Build(p)
			argv := append(append([]string(nil), launch.Entrypoint...), launch.Argv...)
			flag := "--max-model-len"
			if engine == EngineSGLang {
				flag = "--context-length"
			}
			got, count := "", 0
			for i, token := range argv {
				if token == flag {
					count++
					if i+1 < len(argv) {
						got = argv[i+1]
					}
				}
			}
			if got != tc.want || count > 1 {
				t.Fatalf("context=%q (%d flags), want %q: %v", got, count, tc.want, argv)
			}
			if tc.want == "auto" {
				if out.Quick.MaxModelLen != -1 {
					t.Fatalf("stored context=%d, want auto sentinel -1", out.Quick.MaxModelLen)
				}
				if err := ValidateLaunchConfigForEngine(engine, out.Quick); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestVLLMContextAutoPreservesStoredAndUnscopedLaunches(t *testing.T) {
	for _, in := range []MaterializeInput{
		{Engine: EngineVLLM},
		{Engine: EngineVLLM, EngineVersion: "0.30.0"},
		{Engine: EngineVLLM, EngineVersion: "0.30.0", Command: &EffectiveCommand{InterpretEngineDialect: true}, Base: &LaunchConfig{}},
	} {
		out, err := Materialize(in)
		if err != nil {
			t.Fatal(err)
		}
		if out.Quick.MaxModelLen != 0 {
			t.Fatalf("existing/unscoped launch gained context=%d", out.Quick.MaxModelLen)
		}
	}
	stored, _, err := NormalizeStoredLaunchConfig(StoredLaunchConfig{Engine: EngineVLLM, QuickParamsJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	if stored.QuickParamsJSON != `{}` {
		t.Fatalf("stored launch gained params=%s", stored.QuickParamsJSON)
	}
}

func TestVLLMContextAutoRoundTripsAsTemplate(t *testing.T) {
	exported, err := ExportLaunchArgs(EngineVLLM, LaunchConfig{MaxModelLen: -1}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(exported.Args, []string{"--max-model-len", "auto"}) {
		t.Fatalf("exported args=%v", exported.Args)
	}
	imported, err := Materialize(MaterializeInput{Engine: EngineVLLM, TemplateArgs: exported.Args, Base: &LaunchConfig{}})
	if err != nil {
		t.Fatal(err)
	}
	if imported.Quick.MaxModelLen != -1 || len(imported.TemplateResidual) != 0 {
		t.Fatalf("imported=%+v", imported)
	}
}

func TestContextAutoDoesNotPermitOtherNegativeValues(t *testing.T) {
	for _, engine := range []string{EngineVLLM, EngineSGLang} {
		for _, value := range []int{-2, -1} {
			if engine == EngineVLLM && value == -1 {
				continue
			}
			if _, err := Materialize(MaterializeInput{Engine: engine, Raw: map[string]any{"max_model_len": value}}); err == nil {
				t.Fatalf("%s accepted context=%d", engine, value)
			}
		}
	}
}
