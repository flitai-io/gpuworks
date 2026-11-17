package engine

import (
	"context"
	"io"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/enginelog"
)

// exitTailLines is how much of a stopped container's output is read for the
// reason it stopped. A tensor-parallel vLLM prints each worker's traceback
// before the engine core's and the API server's wrappers, so the root cause
// can sit a few hundred lines above the end.
const exitTailLines = 1000

// exitOutput is what a stopped container's output says about why it
// stopped: the known failures it shows (enginelog.Classify) and the error the
// engine's failure began with (enginelog.RootCause, which looks past "See
// root cause above" wrappers). Removal deletes the output, so this is what is
// left of it on the failed instance.
type exitOutput struct {
	causes []enginelog.Cause
	root   string
}

func (e *Engine) readExitOutput(ctx context.Context, id string) (exitOutput, bool) {
	rc, err := e.Driver().Logs(ctx, id, false, exitTailLines)
	if err != nil {
		return exitOutput{}, false
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, 512*1024))
	if err != nil {
		return exitOutput{}, false
	}
	text := string(data)
	return exitOutput{causes: enginelog.Classify(text), root: enginelog.RootCause(text)}, true
}

// attach puts the output's causes and root-cause line on failure.
func (o exitOutput) attach(failure *proto.RuntimeFailure) {
	for _, c := range o.causes {
		cause := proto.FailureCause{Code: c.Code, Evidence: c.Lines(), GLIBC: c.GLIBC}
		if c.Code == enginelog.CodeDGPUMemoryLimit {
			device := c.Device
			cause.Device, cause.NeededBytes, cause.LimitBytes = &device, c.NeededBytes, c.LimitBytes
		}
		failure.Causes = append(failure.Causes, cause)
	}
	if o.root != "" {
		failure.Evidence = []string{o.root}
	}
}

// message is the English fallback for an exit: a missing glibc replaces it
// (the exit status says nothing more), a dGPU limit refusal or the root-cause
// line is added to base, and an exit that printed nothing says so, which a
// full disk also produces.
func (o exitOutput) message(base string) string {
	if c, ok := o.cause(enginelog.CodeGLIBCTooOld); ok {
		return c.Fallback()
	}
	if c, ok := o.cause(enginelog.CodeDGPUMemoryLimit); ok {
		return base + ": " + c.Fallback()
	}
	if o.root != "" {
		return base + ": " + o.root
	}
	if _, ok := o.cause(enginelog.CodeNoOutput); ok {
		return base + " with no output"
	}
	return base
}

func (o exitOutput) cause(code string) (enginelog.Cause, bool) {
	for _, c := range o.causes {
		if c.Code == code {
			return c, true
		}
	}
	return enginelog.Cause{}, false
}

// exitedFailure is the reason a container that stopped on its own stopped,
// in phase: killed for memory (oomMessage is the fallback) or exited
// (exitBase is the fallback the output's reason is added to). Either way the
// output's known failures and root cause go with it.
func (e *Engine) exitedFailure(ctx context.Context, id string, st runtime.Status, phase, oomMessage, exitBase string) (proto.RuntimeFailure, string) {
	exitCode := st.ExitCode
	failure := proto.RuntimeFailure{Code: proto.ErrorContainerExited, Phase: phase, ExitCode: &exitCode}
	oom := e.detectOOM(ctx, id, st)
	if oom {
		failure.Code, failure.OOMKilled = proto.ErrorContainerOOMKilled, true
	}
	out, _ := e.readExitOutput(ctx, id)
	out.attach(&failure)
	if oom {
		return failure, oomMessage
	}
	return failure, out.message(exitBase)
}

// attachOutput adds what a container that is still there printed to the
// reason it is failed for (a startup that timed out, an unhealthy worker):
// it is reclaimed next, and its output with it. Silence from a process that
// is still running is not the exit without output CodeNoOutput names.
func (e *Engine) attachOutput(ctx context.Context, id string, failure *proto.RuntimeFailure) {
	out, read := e.readExitOutput(ctx, id)
	if !read {
		return
	}
	if _, silent := out.cause(enginelog.CodeNoOutput); silent {
		out.causes = nil
	}
	out.attach(failure)
}
