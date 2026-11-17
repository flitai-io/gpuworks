package hostproc

import (
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	oci "gpuworks.ai/gpuworks/internal/agent/runtime/udocker/native"
)

// fakeFS says which paths exist: the package's files under /env/ROOT.
func fakeFS(paths ...string) func(string) bool {
	return func(p string) bool { return slices.Contains(paths, p) }
}

var testDirs = instanceDirs{Home: "/state/i/home", Tmp: "/state/i/tmp", Cache: "/state/cache/env-1"}

func plan(t *testing.T, config oci.ImageConfig, spec runtime.WorkloadSpec, exists func(string) bool) launch {
	t.Helper()
	if spec.Name == "" {
		spec.Name = "w"
	}
	got, err := planLaunch("/env/ROOT", config, spec, testDirs, func(string) string { return "" }, exists)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func envOf(l launch) map[string]string {
	env := map[string]string{}
	for _, kv := range l.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	return env
}

func TestPlanLaunchFollowsDockerEntrypointSemantics(t *testing.T) {
	exists := fakeFS("/env/ROOT/bin/engine", "/env/ROOT/bin/other", "/env/ROOT/bin")
	config := oci.ImageConfig{Entrypoint: []string{"engine"}, Cmd: []string{"--default"}, Env: []string{"PATH=/bin"}}
	for _, tc := range []struct {
		name string
		spec runtime.WorkloadSpec
		want []string
	}{
		{"image defaults", runtime.WorkloadSpec{}, []string{"engine", "--default"}},
		{"cmd replaces the image cmd", runtime.WorkloadSpec{Cmd: []string{"--mine"}}, []string{"engine", "--mine"}},
		{"entrypoint drops the image cmd", runtime.WorkloadSpec{Entrypoint: []string{"other"}}, []string{"other"}},
		{"entrypoint and cmd", runtime.WorkloadSpec{Entrypoint: []string{"other"}, Cmd: []string{"x"}}, []string{"other", "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := plan(t, config, tc.spec, exists)
			if !slices.Equal(got.Args, tc.want) {
				t.Fatalf("Args = %v, want %v", got.Args, tc.want)
			}
		})
	}
	if _, err := planLaunch("/env/ROOT", oci.ImageConfig{}, runtime.WorkloadSpec{Name: "w", Image: "i"}, testDirs, func(string) string { return "" }, exists); err == nil {
		t.Fatal("a workload with no command was planned")
	}
}

func TestPlanLaunchTranslatesContainerPaths(t *testing.T) {
	exists := fakeFS("/env/ROOT/bin/engine", "/env/ROOT/bin", "/env/ROOT/opt/adapter.py", "/env/ROOT/lib")
	spec := runtime.WorkloadSpec{
		Entrypoint: []string{"engine"},
		Cmd: []string{
			"/opt/adapter.py",        // a package file
			"--model=/model",         // a mount, as a flag value
			"/model/sub/config.json", // under a mount
			"/models-extra",          // a longer sibling of /model is not /model
			"/kv/cache",              // the more specific mount wins
			"/etc/hosts",             // a host path stays
			"relative/path",          // not a path to translate
			"cd /model && ./run /kv", // a script: mount targets as words
			"--weird=/model:/opt/adapter.py",
		},
		Env: map[string]string{
			"MODEL_DIR":      "/model",
			"PYTHONPATH":     "/lib:/model/py",
			"GREETING":       "hello /model",
			"PLAIN":          "value",
			"LD_PRELOAD":     "/usr/lib/libhook.so",
			"XDG_CACHE_HOME": "/custom/cache",
		},
		Mounts: []runtime.Mount{
			{Source: "/data/models/qwen", Target: "/model"},
			{Source: "/data/kv", Target: "/kv"},
			{Source: "/data/kvcache", Target: "/kv/cache"},
		},
	}
	got := plan(t, oci.ImageConfig{Env: []string{"PATH=/bin:/usr/local/bin"}}, spec, exists)
	want := []string{
		"engine",
		"/env/ROOT/opt/adapter.py",
		"--model=/data/models/qwen",
		"/data/models/qwen/sub/config.json",
		"/models-extra",
		"/data/kvcache",
		"/etc/hosts",
		"relative/path",
		"cd /data/models/qwen && ./run /data/kv",
		"--weird=/data/models/qwen:/env/ROOT/opt/adapter.py",
	}
	if !slices.Equal(got.Args, want) {
		t.Fatalf("Args =\n%q\nwant\n%q", got.Args, want)
	}
	if got.Path != "/env/ROOT/bin/engine" {
		t.Fatalf("Path = %q", got.Path)
	}
	env := envOf(got)
	for key, value := range map[string]string{
		"MODEL_DIR":      "/data/models/qwen",
		"PYTHONPATH":     "/env/ROOT/lib:/data/models/qwen/py",
		"GREETING":       "hello /data/models/qwen",
		"PLAIN":          "value",
		"HOME":           testDirs.Home,
		"TMPDIR":         testDirs.Tmp,
		"XDG_CACHE_HOME": "/custom/cache", // the template's choice is kept
		"LANG":           "en_US.UTF-8",
		// The image's /usr/local/bin is not in the package, so it is dropped
		// rather than leading to the host's Homebrew.
		"PATH": "/env/ROOT/bin:/usr/bin:/bin:/usr/sbin:/sbin",
	} {
		if env[key] != value {
			t.Errorf("%s = %q, want %q", key, env[key], value)
		}
	}
	if _, ok := env["LD_PRELOAD"]; ok {
		t.Errorf("LD_PRELOAD reached the workload: %v", env)
	}
	if got.Dir != testDirs.Home {
		t.Errorf("Dir = %q, want the instance home when the image names no working directory", got.Dir)
	}
}

func TestPlanLaunchInheritsOnlyTheHostIdentityLocaleAndProxy(t *testing.T) {
	host := map[string]string{
		"USER": "alice", "LANG": "zh_CN.UTF-8", "HTTPS_PROXY": "http://127.0.0.1:7894",
		"PATH": "/opt/homebrew/bin:/usr/bin", "PYTHONHOME": "/opt/homebrew/Frameworks/Python", "DYLD_LIBRARY_PATH": "/opt/homebrew/lib",
		"HOME": "/Users/alice",
	}
	got, err := planLaunch("/env/ROOT", oci.ImageConfig{Entrypoint: []string{"/bin/sh"}}, runtime.WorkloadSpec{Name: "w"}, testDirs,
		func(key string) string { return host[key] }, fakeFS())
	if err != nil {
		t.Fatal(err)
	}
	env := envOf(got)
	if env["USER"] != "alice" || env["LANG"] != "zh_CN.UTF-8" || env["HTTPS_PROXY"] != "http://127.0.0.1:7894" {
		t.Fatalf("inherited = %v", env)
	}
	for _, key := range []string{"PYTHONHOME", "DYLD_LIBRARY_PATH"} {
		if _, ok := env[key]; ok {
			t.Errorf("%s leaked from the agent: %v", key, env)
		}
	}
	if env["HOME"] != testDirs.Home || env["PATH"] != hostPATH {
		t.Fatalf("HOME/PATH = %q %q", env["HOME"], env["PATH"])
	}
}

func TestPlanLaunchWorkingDirectory(t *testing.T) {
	exists := fakeFS("/env/ROOT/app", "/env/ROOT/bin/engine")
	got := plan(t, oci.ImageConfig{Entrypoint: []string{"/bin/engine"}, WorkingDir: "/app"}, runtime.WorkloadSpec{}, exists)
	if got.Dir != "/env/ROOT/app" || got.Path != "/env/ROOT/bin/engine" {
		t.Fatalf("Dir/Path = %q %q", got.Dir, got.Path)
	}
	// A working directory the package lacks is a container path the host does
	// not have; the instance home stands in.
	got = plan(t, oci.ImageConfig{Entrypoint: []string{"/bin/engine"}, WorkingDir: "/workspace"}, runtime.WorkloadSpec{}, exists)
	if got.Dir != testDirs.Home {
		t.Fatalf("Dir = %q", got.Dir)
	}
}

func TestScriptTranslatesEachMountWordOnce(t *testing.T) {
	m := newPathMap("", []runtime.Mount{
		{Source: "/kv/m", Target: "/model"}, // its source starts with another target
		{Source: "/data/kv", Target: "/kv"},
	}, func(string) bool { return false })
	for script, want := range map[string]string{
		"cp /model /model/a":  "cp /kv/m /kv/m/a",
		"ls /model /kv":       "ls /kv/m /data/kv",
		`x="/model";y=/kv`:    `x="/kv/m";y=/data/kv`,
		"echo /modelx /kv2 /": "echo /modelx /kv2 /",
	} {
		if got := m.script(script); got != want {
			t.Errorf("script(%q) = %q, want %q", script, got, want)
		}
	}
}

// The value of --served-model-name is the name the gateway sends, not a path:
// a launch with no alias names the model after its mount target ("/model"),
// and translating that to the host path would leave the engine answering
// under a name the gateway never asks for.
func TestPlanLaunchKeepsServedModelNameVerbatim(t *testing.T) {
	spec := runtime.WorkloadSpec{
		Entrypoint: []string{"vllm", "serve", "/model"},
		Cmd:        []string{"--served-model-name", "/model", "--tokenizer", "/model", "--served-model-name=/model/sub"},
		Mounts:     []runtime.Mount{{Source: "/Users/x/m", Target: "/model"}},
	}
	exists := fakeFS("/env/ROOT/bin/vllm", "/env/ROOT/bin/python3", "/env/ROOT/bin")
	config := oci.ImageConfig{Env: []string{"PATH=/bin"}}
	got := plan(t, config, spec, exists)
	want := []string{"vllm", "serve", "/Users/x/m", "--served-model-name", "/model", "--tokenizer", "/Users/x/m", "--served-model-name=/model/sub"}
	if !slices.Equal(got.Args, want) {
		t.Fatalf("Args = %v, want %v", got.Args, want)
	}

	sglang := runtime.WorkloadSpec{
		Entrypoint: []string{"python3", "-m", "sglang.launch_server"},
		Cmd:        []string{"--model-path", "/model", "--served-model-name", "/model"},
		Mounts:     []runtime.Mount{{Source: "/Users/x/m", Target: "/model"}},
	}
	got = plan(t, config, sglang, exists)
	want = []string{"python3", "-m", "sglang.launch_server", "--model-path", "/Users/x/m", "--served-model-name", "/model"}
	if !slices.Equal(got.Args, want) {
		t.Fatalf("Args = %v, want %v", got.Args, want)
	}
}

// A manual command reaches the agent as one shell script ("/bin/sh -c ...").
// Its --served-model-name value is a name in every spelling a script can
// give it, while the model path words around it are still translated.
func TestScriptKeepsServedModelNameVerbatim(t *testing.T) {
	m := newPathMap("", []runtime.Mount{{Source: "/Users/x/m", Target: "/model"}}, func(string) bool { return false })
	for script, want := range map[string]string{
		"set -a\nvllm serve /model --served-model-name /model --tokenizer /model":                      "set -a\nvllm serve /Users/x/m --served-model-name /model --tokenizer /Users/x/m",
		"vllm serve /model --served-model-name=/model":                                                 "vllm serve /Users/x/m --served-model-name=/model",
		`vllm serve "/model" --served-model-name "/model"`:                                             `vllm serve "/Users/x/m" --served-model-name "/model"`,
		"vllm serve /model --served-model-name='/model' --port 8000":                                   "vllm serve /Users/x/m --served-model-name='/model' --port 8000",
		"python3 -m sglang.launch_server --model-path /model \\\n  --served-model-name \\\n  /model\n": "python3 -m sglang.launch_server --model-path /Users/x/m \\\n  --served-model-name \\\n  /model\n",
		"vllm serve /model --served-model-name /model/sub; ls /model":                                  "vllm serve /Users/x/m --served-model-name /model/sub; ls /Users/x/m",
		`vllm serve /model "--served-model-name" /model`:                                               `vllm serve /Users/x/m "--served-model-name" /model`,
		"vllm serve /model '--served-model-name'=/model":                                               "vllm serve /Users/x/m '--served-model-name'=/model",
	} {
		if got := m.script(script); got != want {
			t.Errorf("script(%q)\n got %q\nwant %q", script, got, want)
		}
	}

	spec := runtime.WorkloadSpec{
		Entrypoint: []string{"/bin/sh", "-c"},
		Cmd:        []string{"set -a\nexec vllm serve /model --served-model-name /model"},
		Mounts:     []runtime.Mount{{Source: "/Users/x/m", Target: "/model"}},
	}
	got := plan(t, oci.ImageConfig{Env: []string{"PATH=/bin"}}, spec, fakeFS("/env/ROOT/bin/sh", "/env/ROOT/bin"))
	if want := "set -a\nexec vllm serve /Users/x/m --served-model-name /model"; got.Args[len(got.Args)-1] != want {
		t.Fatalf("script = %q, want %q", got.Args[len(got.Args)-1], want)
	}
}

// A mount target is a word when what follows it ends the word: a slash, a
// blank (space, tab, newline), a quote or a shell operator. A longer word that
// only begins with the target is another path and is left alone.
func TestScriptFindsTheEndOfAMountWord(t *testing.T) {
	m := newPathMap("", []runtime.Mount{{Source: "/Users/x/m", Target: "/model"}}, func(string) bool { return false })
	for script, want := range map[string]string{
		"set -a\nvllm serve /model\n":                              "set -a\nvllm serve /Users/x/m\n",
		"set -a\nls /model\nexec vllm serve /model --port 8000":    "set -a\nls /Users/x/m\nexec vllm serve /Users/x/m --port 8000",
		"vllm serve /model\t--port 1":                              "vllm serve /Users/x/m\t--port 1",
		"ls /model&& vllm serve /model|tee log":                    "ls /Users/x/m&& vllm serve /Users/x/m|tee log",
		"cat </model >/dev/null":                                   "cat </Users/x/m >/dev/null",
		"vllm serve /modeltmp --port 8000":                         "vllm serve /modeltmp --port 8000",
		"vllm serve /modelnew":                                     "vllm serve /modelnew",
		"vllm serve /model\\x":                                     "vllm serve /model\\x",
		"vllm serve /model \\\n  --tokenizer /model\\\n  --port 1": "vllm serve /Users/x/m \\\n  --tokenizer /Users/x/m\\\n  --port 1",
	} {
		if got := m.script(script); got != want {
			t.Errorf("script(%q)\n got %q\nwant %q", script, got, want)
		}
	}
}
