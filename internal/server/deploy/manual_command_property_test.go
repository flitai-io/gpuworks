package deploy

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

// manualWorld is the ranks of one deployment that would share a manual
// command: a multi-node group, or the role units of a PD deployment (each
// unit itself one or more nodes).
type manualWorld struct {
	engine Engine
	ranks  []ResolveLaunchInput
}

func (w manualWorld) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "engine=%s", w.engine.Name())
	for _, rank := range w.ranks {
		p := rank.Params
		role := ""
		if p.PD != nil {
			role = p.PD.Mode + "/" + p.PD.Role
		}
		fmt.Fprintf(&b, "\n  rank=%d/%d headless=%v api=%d master=%s:%d addr=%s pd=%s extra=%q template=%q", p.NodeRank, p.NNodes, p.Headless, p.APIPort, p.MasterAddr, p.MasterPort, p.NodeAddress, role, p.ExtraArgs, p.TemplateArgs)
	}
	return b.String()
}

// trickyArgs are user arguments whose text a shell or the variable expansion
// could mangle on the way from an engine argv to a script and back.
var trickyArgs = [][]string{
	nil,
	{"--chat-template", "/model/it's a template.jinja"},
	{"--override-generation-config", `{"temperature": 0.7, "stop": ["</s>", "$END"]}`},
	{"--log-format", "%(asctime)s ${levelname} `x` \\n"},
	{"--note", "a b\tc;d|e&f>g"},
	{"--shell-var", "${FOO}/$BAR"},
	{"--empty", ""},
	{"--bare-platform-name", "${MODEL_PATH}/${NODE_RANK}"},
}

func genManualWorld(t *testing.T) *rapid.Generator[manualWorld] {
	return rapid.Custom(func(rt *rapid.T) manualWorld {
		engine := rapid.SampledFrom([]Engine{vLLMEngine{}, sglangEngine{}}).Draw(rt, "engine")
		quick := materializedConfig(t, engine.Name())
		extra := rapid.SampledFrom(trickyArgs).Draw(rt, "extra")
		var template []string
		if rapid.Bool().Draw(rt, "templateVariables") {
			// A template may place launch facts itself; a draft keeps the
			// rank-owned ones as references.
			template = []string{"--metadata", "rank=${NODE_RANK}/${NNODES} model=${MODEL_PATH}"}
		}
		network := ComputeNetwork{}
		if rapid.Bool().Draw(rt, "ethernet") {
			network = ComputeNetwork{Type: netplan.TypeHostEthernet, Interface: "eth1"}
		}
		var modes []string
		for _, name := range pdmode.Names() {
			if strings.HasPrefix(name, engine.Name()+"-") {
				modes = append(modes, name)
			}
		}
		pdMode := ""
		if len(modes) > 0 && rapid.Bool().Draw(rt, "pd") {
			pdMode = rapid.SampledFrom(modes).Draw(rt, "pdMode")
		}
		world := manualWorld{engine: engine}
		nextAddress := 1
		unit := func(role string, nnodes, perNode int) {
			masterPort := rapid.IntRange(20000, 29999).Draw(rt, "masterPort")
			masterAddr := fmt.Sprintf("10.0.0.%d", nextAddress)
			for rank := 0; rank < nnodes; rank++ {
				address := fmt.Sprintf("10.0.0.%d", nextAddress)
				nextAddress++
				params := Params{
					ModelPath: "/model", ServedName: "qwen", AssignedGPUs: perNode, TensorParallel: perNode * nnodes,
					Quick: quick, ExtraArgs: extra, TemplateArgs: template, ComputeNetwork: network,
					NNodes: nnodes, NodeRank: rank, Headless: rank > 0, NodeAddress: address,
				}
				if nnodes > 1 {
					params.MasterAddr, params.MasterPort = masterAddr, masterPort
				}
				if rank == 0 {
					params.APIPort = rapid.IntRange(30000, 39999).Draw(rt, "apiPort")
				}
				if role != "" {
					params.Quick = PDRoleLaunchConfig(quick, role)
					params.PD = &PDLaunch{Mode: pdMode, Role: role, SideChannelHost: address, SideChannelPort: rapid.IntRange(40000, 49999).Draw(rt, "sideChannelPort")}
				}
				world.ranks = append(world.ranks, ResolveLaunchInput{Engine: engine, Params: params})
			}
		}
		if pdMode == "" {
			unit("", rapid.IntRange(2, 4).Draw(rt, "nnodes"), rapid.SampledFrom([]int{1, 2}).Draw(rt, "gpusPerNode"))
			return world
		}
		mode, _ := pdmode.Lookup(pdMode)
		for _, role := range mode.Roles {
			// The units of one role share its shape: node count and degree.
			nnodes := rapid.IntRange(1, 2).Draw(rt, "unitNodes")
			perNode := rapid.SampledFrom([]int{1, 2}).Draw(rt, "gpusPerNode")
			for range rapid.IntRange(1, 2).Draw(rt, "units") {
				unit(role, nnodes, perNode)
			}
		}
		return world
	})
}

// executedArgv runs a manual launch the way the agent's container would
// (/bin/sh -c script) with the engine executable replaced by a program that
// reports the argv it was started with.
func executedArgv(t *testing.T, stubs string, launch ResolvedLaunch) ([]string, error) {
	t.Helper()
	if !slices.Equal(launch.Entrypoint, []string{"/bin/sh"}) || len(launch.Argv) != 2 || launch.Argv[0] != "-c" {
		return nil, fmt.Errorf("manual launch is not a shell script: %q %q", launch.Entrypoint, launch.Argv)
	}
	command := exec.Command("/bin/sh", "-c", launch.Argv[1])
	command.Env = []string{"PATH=" + stubs}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("script failed: %v\nstderr: %s\nscript:\n%s", err, stderr.String(), launch.Argv[1])
	}
	fields := strings.Split(stdout.String(), "\x00")
	return fields[:len(fields)-1], nil
}

// engineStubs is a PATH directory in which every executable an engine command
// starts with prints its own argv, NUL-terminated.
func engineStubs(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		script := "#!/bin/sh\nprintf '%s\\0' " + name + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestManualDraftRunsTheEngineCommandOnEveryRank is the contract of a manual
// command shared by several ranks: starting from the draft, each rank's shell
// executes exactly the argv the engine would have built for that rank, in
// exactly the engine's environment. It runs the script in a real shell, so
// quoting, the case statement and the per-rank expansion are all on the path.
//
//	go test ./internal/server/deploy -run TestManualDraftRunsTheEngineCommandOnEveryRank -rapid.checks=1000
func TestManualDraftRunsTheEngineCommandOnEveryRank(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	stubs := engineStubs(t, "vllm", "python3", "python", "sglang")
	worlds := genManualWorld(t)
	rapid.Check(t, func(rt *rapid.T) {
		world := worlds.Draw(rt, "world")
		draft, ok, err := ManualCommandDraft(world.ranks)
		if err != nil || !ok {
			rt.Fatalf("no draft (ok=%v err=%v) for\n%s", ok, err, world)
		}
		pd := world.ranks[0].Params.PD != nil
		nnodes := 0
		for _, rank := range world.ranks {
			nnodes = max(nnodes, rank.Params.NNodes)
		}
		if missing := ManualCommandRankBlind(draft, nnodes, pd); len(missing) > 0 {
			rt.Fatalf("the draft is refused as rank-blind (missing %v):\n%s", missing, draft)
		}
		for _, input := range world.ranks {
			engineLaunch, err := ResolveLaunch(input)
			if err != nil {
				rt.Fatalf("engine launch: %v\n%s", err, world)
			}
			manual := input
			manual.ContainerOverrides = ContainerOverrides{CommandMode: string(CommandManual), CmdDisplay: draft}
			manualLaunch, err := ResolveLaunch(manual)
			if err != nil {
				rt.Fatalf("manual launch: %v\n%s\ndraft:\n%s", err, world, draft)
			}
			got, err := executedArgv(t, stubs, manualLaunch)
			if err != nil {
				rt.Fatalf("rank %d: %v\n%s", input.Params.NodeRank, err, world)
			}
			if want := launchArguments(engineLaunch); !slices.Equal(got, want) {
				rt.Fatalf("rank %d executes\n  %q\nwant the engine argv\n  %q\n%s\ndraft:\n%s", input.Params.NodeRank, got, want, world, draft)
			}
			// The SGLang severity config is the one thing an engine launch has
			// that a manual one never gets: the platform does not rewrite a
			// command its author owns (docs/explanation/sglang-logging.md).
			delete(engineLaunch.Env, sglangLoggingEnv)
			if !maps.Equal(manualLaunch.Env, engineLaunch.Env) {
				rt.Fatalf("rank %d environment\n  %v\nwant the engine environment\n  %v\n%s", input.Params.NodeRank, manualLaunch.Env, engineLaunch.Env, world)
			}
		}
	})
}
