package templateexport

import (
	"context"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/healthcheck"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// CheckTimeout bounds each operation request an export sends.
const CheckTimeout = 60 * time.Second

// ProbeReply is what one request through the gateway answered.
type ProbeReply struct {
	NoReplica    bool
	HTTPStatus   int
	InstanceUUID string
	Message      string
}

// Prober sends one request to a deployment through the gateway, the way the
// health check does, but pinned to the instance whose runtime facts the
// export describes. It does not fall back to another replica.
type Prober func(ctx context.Context, deploymentID uint, instanceUUID, path string, payload map[string]any) ProbeReply

// Checks sends the smallest request each cheap operation of contract takes -
// chat, text completion and embeddings, with the health check's own bodies -
// and lists every other operation as not tested: a video, image or speech
// request is slow or costly, and the export says so instead of skipping the
// template.
func Checks(ctx context.Context, probe Prober, deploymentID uint, instanceUUID string, contract inferenceapi.Contract) []Check {
	checks := make([]Check, 0, len(contract.Operations))
	for _, op := range contract.Operations {
		check := Check{ID: CheckOperationReply, Operation: op.Type, Endpoint: op.Endpoint, Status: proto.CheckSkip}
		kind, cheap := checkKind(op.Type)
		switch {
		case op.Endpoint == "":
			check.Code = messages.CheckTemplateOperationNoEndpoint
		case !cheap || probe == nil:
			check.Code = messages.CheckTemplateOperationNotProbed
		}
		if check.Code != "" {
			checks = append(checks, check)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, CheckTimeout)
		reply := probe(cctx, deploymentID, instanceUUID, op.Endpoint, healthcheck.Payload(kind))
		contextErr := cctx.Err()
		cancel()
		check.Status, check.HTTPStatus, check.Evidence = proto.CheckFail, reply.HTTPStatus, reply.Message
		switch {
		case reply.NoReplica:
			check.Code = messages.CheckTemplateOperationNoReplica
		case contextErr != nil:
			check.Code = messages.CheckTemplateOperationTimedOut
			if check.Evidence == "" {
				check.Evidence = contextErr.Error()
			}
		case reply.HTTPStatus != 0 && (reply.HTTPStatus < http.StatusOK || reply.HTTPStatus >= http.StatusMultipleChoices):
			// Evidence is the error body the gateway or engine answered.
			check.Code = messages.CheckTemplateOperationHTTPError
		case reply.Message != "" || reply.HTTPStatus == 0:
			// The request failed, or a 2xx arrived and its body then broke
			// off: the status proves nothing.
			check.Code = messages.CheckTemplateOperationReplyIncomplete
		default:
			check.Status, check.Code = proto.CheckPass, messages.CheckTemplateOperationAnswered
		}
		checks = append(checks, check)
	}
	return checks
}

func checkKind(operation string) (healthcheck.Kind, bool) {
	switch operation {
	case inferenceapi.OperationChat:
		return healthcheck.KindChat, true
	case inferenceapi.OperationCompletions:
		return healthcheck.KindCompletions, true
	case inferenceapi.OperationEmbeddings:
		return healthcheck.KindEmbeddings, true
	case inferenceapi.OperationDecide:
		return healthcheck.KindDecision, true
	}
	return "", false
}
