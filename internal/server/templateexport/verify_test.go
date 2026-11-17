package templateexport

import (
	"context"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestChecksRejectsAnInterruptedSuccessfulResponse(t *testing.T) {
	contract := inferenceapi.Contract{Operations: []inferenceapi.Operation{{Type: inferenceapi.OperationChat, Endpoint: "/v1/chat/completions"}}}
	ctx, cancel := context.WithCancel(context.Background())
	checks := Checks(ctx, func(context.Context, uint, string, string, map[string]any) ProbeReply {
		cancel()
		return ProbeReply{HTTPStatus: 200, Message: "context canceled"}
	}, 7, "inst", contract)
	if len(checks) != 1 || checks[0].Status != proto.CheckFail || checks[0].Code != messages.CheckTemplateOperationTimedOut || checks[0].Evidence != "context canceled" {
		t.Fatalf("an interrupted body must not verify a template: %+v", checks)
	}
}

func TestChecksTestsCheapOperationsAndListsTheRest(t *testing.T) {
	contract := inferenceapi.Contract{Operations: []inferenceapi.Operation{
		{Type: inferenceapi.OperationChat, Endpoint: "/v1/chat/completions"},
		{Type: inferenceapi.OperationEmbeddings, Endpoint: "/v1/embeddings"},
		{Type: "openai.video.generate", Endpoint: "/v1/videos"},
	}}
	var sent []string
	probe := func(ctx context.Context, id uint, _ string, path string, payload map[string]any) ProbeReply {
		sent = append(sent, path)
		if _, deadline := ctx.Deadline(); !deadline {
			t.Error("a check was sent without a deadline")
		}
		if path == "/v1/embeddings" {
			if payload["input"] == nil {
				t.Errorf("embedding payload = %v", payload)
			}
			return ProbeReply{HTTPStatus: 503, Message: "busy"}
		}
		return ProbeReply{HTTPStatus: 200}
	}
	checks := Checks(context.Background(), probe, 7, "inst", contract)
	if !slices.Equal(sent, []string{"/v1/chat/completions", "/v1/embeddings"}) {
		t.Fatalf("sent %q: a video request must not be sent", sent)
	}
	want := []Check{
		{ID: CheckOperationReply, Operation: inferenceapi.OperationChat, Endpoint: "/v1/chat/completions", Status: proto.CheckPass, Code: messages.CheckTemplateOperationAnswered, HTTPStatus: 200},
		{ID: CheckOperationReply, Operation: inferenceapi.OperationEmbeddings, Endpoint: "/v1/embeddings", Status: proto.CheckFail, Code: messages.CheckTemplateOperationHTTPError, HTTPStatus: 503, Evidence: "busy"},
		{ID: CheckOperationReply, Operation: "openai.video.generate", Endpoint: "/v1/videos", Status: proto.CheckSkip, Code: messages.CheckTemplateOperationNotProbed},
	}
	if !slices.Equal(checks, want) {
		t.Fatalf("checks = %+v\nwant     %+v", checks, want)
	}
	noReplica := Checks(context.Background(), func(context.Context, uint, string, string, map[string]any) ProbeReply {
		return ProbeReply{NoReplica: true}
	}, 7, "inst", contract)
	if noReplica[0].Status != proto.CheckFail || noReplica[0].Code != messages.CheckTemplateOperationNoReplica || noReplica[0].Message() != "no running replica answered openai.chat" {
		t.Fatalf("no replica = %+v", noReplica[0])
	}
}

func TestChecksNameWhyAnOperationWasNotSentAndAnHTTPError(t *testing.T) {
	contract := inferenceapi.Contract{Operations: []inferenceapi.Operation{
		{Type: inferenceapi.OperationChat, Endpoint: "/v1/chat/completions"},
		{Type: inferenceapi.OperationCompletions},
	}}
	checks := Checks(context.Background(), func(context.Context, uint, string, string, map[string]any) ProbeReply {
		return ProbeReply{HTTPStatus: 500}
	}, 7, "inst", contract)
	interrupted := Checks(context.Background(), func(context.Context, uint, string, string, map[string]any) ProbeReply {
		return ProbeReply{HTTPStatus: 200, Message: "the upstream response body was interrupted"}
	}, 7, "inst", contract)
	if interrupted[0].Status != proto.CheckFail || interrupted[0].Code != messages.CheckTemplateOperationReplyIncomplete || interrupted[0].Evidence == "" {
		t.Fatalf("interrupted 2xx = %+v", interrupted[0])
	}
	if checks[0].Status != proto.CheckFail || checks[0].Code != messages.CheckTemplateOperationHTTPError || checks[0].Evidence != "" {
		t.Fatalf("HTTP 500 = %+v", checks[0])
	}
	if checks[1].Status != proto.CheckSkip || checks[1].Code != messages.CheckTemplateOperationNoEndpoint {
		t.Fatalf("no endpoint = %+v", checks[1])
	}
	unprobed := Checks(context.Background(), nil, 7, "inst", contract)
	if unprobed[0].Status != proto.CheckSkip || unprobed[0].Code != messages.CheckTemplateOperationNotProbed {
		t.Fatalf("no prober = %+v", unprobed[0])
	}
	for _, check := range append(checks, unprobed...) {
		if check.Message() == check.Code {
			t.Errorf("%s has no English fallback", check.Code)
		}
	}
}

// A decision template is verified with the health check's one-question
// decision, the cheapest request it serves.
func TestChecksAskADecisionModelOneQuestion(t *testing.T) {
	contract := inferenceapi.Contract{Operations: []inferenceapi.Operation{{Type: inferenceapi.OperationDecide, Endpoint: "/v1/systemone"}}}
	var payloads []map[string]any
	checks := Checks(context.Background(), func(_ context.Context, _ uint, _ string, path string, payload map[string]any) ProbeReply {
		if path != "/v1/systemone" {
			t.Errorf("path = %s", path)
		}
		payloads = append(payloads, payload)
		return ProbeReply{HTTPStatus: 200}
	}, 7, "inst", contract)
	if len(checks) != 1 || checks[0].Status != proto.CheckPass || len(payloads) != 1 || payloads[0]["state"] != "1" || payloads[0]["questions"] == nil {
		t.Fatalf("checks = %+v, payloads = %v", checks, payloads)
	}
}
