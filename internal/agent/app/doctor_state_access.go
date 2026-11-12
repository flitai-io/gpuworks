package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	udockerdrv "gpuworks.ai/gpuworks/internal/agent/runtime/udocker"
	"gpuworks.ai/gpuworks/internal/proto"
)

// DoctorForState is Doctor for the CLI, which runs beside the agent rather
// than inside it and so has no driver. The agent's state directory says
// whether the runveil/udocker runtime has been set up on this node (its
// udocker directory exists); only then is the state directory's access
// checked, with the same runtime verdict the agent and the node page use. A
// node that never used the runtime is not judged on it.
func DoctorForState(ctx context.Context, w io.Writer, stateDir string) {
	_, _ = io.WriteString(w, doctorReportForState(ctx, gpu.New(), stateDir).Report)
}

// doctorRuntimeAccess is the runtime's verdict source for a state directory;
// a test supplies its own.
var doctorRuntimeAccess = func(stateDir string) runtime.StateAccessReporter { return udockerdrv.New(stateDir) }

func doctorReportForState(ctx context.Context, p gpu.Prober, stateDir string) proto.DoctorReport {
	var access runtime.StateAccessReporter
	if info, err := os.Stat(filepath.Join(stateDir, "udocker")); err == nil && info.IsDir() {
		access = doctorRuntimeAccess(stateDir)
	}
	return buildDoctorReportWith(ctx, p, nil, access, stateDir)
}

// doctorStateAccess reports an operator directory above the agent's state
// directory that the identity workloads run as cannot search. The runtime
// opens the state directory itself and only warns about the directories above
// it, so without this finding every launch fails with "permission denied" on
// the runtime's own binaries while the doctor reads clean. The verdict is the
// runtime's own (runtime.StateAccessReporter), the one its launch-time warning
// uses.
func doctorStateAccess(r *proto.DoctorReport, reporter runtime.StateAccessReporter) {
	if reporter == nil {
		return
	}
	stateDir, blocked := reporter.StateAccessProblem()
	if blocked == nil {
		return
	}
	identity := blocked.Owner.Username
	if identity == "" {
		identity = "the runtime user"
	}
	r.Findings = append(r.Findings, proto.DoctorFinding{
		Code: proto.DoctorRuntimeStateUntraversable, Level: proto.DoctorLevelError,
		Probe: fmt.Sprintf("search permission on every directory above %s for %s (uid %d, gid %d)", stateDir, identity, blocked.Owner.UID, blocked.Owner.GID),
		Evidence: []proto.DoctorField{
			{Name: "directory", Value: blocked.Dir},
			{Name: "mode", Value: fmt.Sprintf("%04o", blocked.Mode)},
			{Name: "owner", Value: fmt.Sprintf("%d:%d", blocked.DirUID, blocked.DirGID)},
			{Name: "user", Value: identity},
			{Name: "state_dir", Value: stateDir},
			{Name: "fix", Value: blocked.FixCommand()},
		},
		Recheck: "namei -m " + stateDir,
	})
}
