package udocker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordWithoutGrant is a workload record as the agent before typed grants
// wrote it (captured from that agent's rememberSpec for a Hygon workload):
// the devices are options and no vendor is named.
const recordWithoutGrant = `{"Name":"demo","Image":"registry.example/engine:1","Cmd":["serve"],"Entrypoint":null,"Env":{"HIP_VISIBLE_DEVICES":"1","HYGON_VISIBLE_DEVICES":"1"},"Mounts":null,"GPUIndexes":null,"DockerArgs":["--ipc=host","--device","/dev/kfd:/dev/kfd:rwm","--device","/dev/mkfd:/dev/mkfd:rwm","--device","/dev/dri/renderD129:/dev/dri/renderD129:rwm","--group-add","39"],"Labels":null,"HostNetwork":false,"ShmBytes":0,"Ports":[8000],"PreparationNotes":null}`

// After an upgrade, a terminal in a container the earlier agent started is
// built from that agent's record: its devices are bound from its options
// and it is not prepared as an NVIDIA workload.
func TestRecordFromBeforeGrantsStillOpensATerminal(t *testing.T) {
	t.Setenv(launcherEnv, "udocker")
	base := t.TempDir()
	d := New(base)
	if err := os.MkdirAll(d.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.SpecPath("demo"), []byte(recordWithoutGrant), 0o600); err != nil {
		t.Fatal(err)
	}
	d.provisioned = true
	d.nvidiaSMIPath = "/host/bin/nvidia-smi"
	d.prootPath = filepath.Join(base, "udocker", "bin", "proot-x86_64")
	d.udockerScript = filepath.Join(base, "offline", "udocker", "udocker")
	argv, err := d.ExecPTY(context.Background(), "demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{"--volume=/dev/kfd:/dev/kfd", "--volume=/dev/mkfd:/dev/mkfd", "--volume=/dev/dri/renderD129:/dev/dri/renderD129", "--env=HIP_VISIBLE_DEVICES=1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q: %s", want, joined)
		}
	}
	for _, leaked := range []string{"NCCL_IB_DISABLE", "nvidia-smi"} {
		if strings.Contains(joined, leaked) {
			t.Fatalf("an earlier agent's Hygon record got NVIDIA setup (%q): %s", leaked, joined)
		}
	}
}
