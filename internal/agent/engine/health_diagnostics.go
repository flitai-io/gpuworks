package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"syscall"

	"gpuworks.ai/gpuworks/internal/proto"
)

// healthResultRefused is the result of a probe that found nothing listening
// on the port: with the host network the engine runs on, its API is not there.
const healthResultRefused = "connection_refused"

// Health URLs can contain query credentials. Only the requested endpoint's
// scheme, host and path belong in the startup diagnostic, never its body/query.
func healthDiagnosticTarget(u *url.URL) string {
	return u.Scheme + "://" + u.Host + u.EscapedPath()
}

func healthTransportResult(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return healthResultRefused
	}
	return "transport_error"
}

func healthDiagnosticDetail(target, result string) string {
	return "from=node requested_target=" + target + " result=" + result + "; readiness probe only; gateway and client path not tested"
}

// healthWaitMessage is the progress of a start that waits for its engine to
// answer healthy: what the last probe saw and, where it found nothing on the
// API port of a manual command that states none, that likely cause.
func healthWaitMessage(spec proto.InstanceDesiredState, target, result string) string {
	message := "waiting for engine health: " + healthDiagnosticDetail(target, result)
	if unstatedPortRefused(spec, result) {
		message += "; " + unstatedPortNote(spec.APIPort)
	}
	return message
}

// unstatedPortFailure names the likely cause of a start that ended without
// its engine answering (it timed out, or stalled: an engine idle on another
// port looks stalled) when the last probe found nothing on the API port of a
// manual command that states none. Any other failure is returned as it is.
func unstatedPortFailure(spec proto.InstanceDesiredState, lastResult string, failure proto.RuntimeFailure, message string) (proto.RuntimeFailure, string) {
	if failure.Code != proto.ErrorEngineHealthTimeout && failure.Code != proto.ErrorEngineStartStalled || !unstatedPortRefused(spec, lastResult) {
		return failure, message
	}
	failure.Code, failure.Port = proto.ErrorEnginePortUnstated, spec.APIPort
	return failure, message + "; " + unstatedPortNote(spec.APIPort)
}

// unstatedPortRefused says the probe found nothing listening on the API port
// of a launch whose manual command states no API port: the engine listens on
// its own default port, and waiting does not bring it to the assigned one.
func unstatedPortRefused(spec proto.InstanceDesiredState, result string) bool {
	return spec.APIPortUnstated && result == healthResultRefused
}

func unstatedPortNote(port int) string {
	return fmt.Sprintf("the manual command states no API port, so the engine is not listening on the assigned port %d", port)
}
