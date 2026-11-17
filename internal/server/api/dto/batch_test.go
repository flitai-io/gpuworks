package dto

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestBatchWire(t *testing.T) {
	created := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	started := created.Add(time.Second)
	done := created.Add(time.Hour)
	kept := done.Add(30 * 24 * time.Hour)
	for _, tc := range []struct {
		name string
		in   Batch
		wire string
	}{
		{
			name: "running batch leaves out the files and times it has not reached",
			in: Batch{PriorityClass: "batch",
				ID: "batch_1", Cancellable: true, Endpoint: "/v1/chat/completions", Status: "in_progress", InputFileID: "file-in", InputFilename: "reqs.jsonl",
				CompletionWindow: "24h", RequestCounts: BatchRequestCount{Total: 10, Completed: 4, Failed: 1}, Metadata: map[string]string{"team": "nlp"},
				UserID: 3, CreatedAt: created, ExpiresAt: created.Add(24 * time.Hour), InProgressAt: &started,
			},
			wire: `{"id":"batch_1","endpoint":"/v1/chat/completions","status":"in_progress","cancellable":true,"input_file_id":"file-in","input_filename":"reqs.jsonl","completion_window":"24h","priority_class":"batch","request_counts":{"total":10,"completed":4,"failed":1},"metadata":{"team":"nlp"},"user_id":3,"created_at":"2026-10-28T09:00:00Z","expires_at":"2026-10-29T09:00:00Z","in_progress_at":"2026-10-28T09:00:01Z"}`,
		},
		{
			name: "failed batch carries its failure; metadata is an object even when empty",
			in: Batch{PriorityClass: "batch",
				ID: "batch_2", Endpoint: "/v1/embeddings", Status: "failed", InputFileID: "file-in", CompletionWindow: "24h",
				Metadata: map[string]string{}, Failure: &BatchFailure{Code: "input_file_unreadable", Message: "gone"},
				UserID: 1, APIKeyID: 8, CreatedAt: created, ExpiresAt: created.Add(24 * time.Hour), FailedAt: &done,
			},
			wire: `{"id":"batch_2","endpoint":"/v1/embeddings","status":"failed","cancellable":false,"input_file_id":"file-in","completion_window":"24h","priority_class":"batch","request_counts":{"total":0,"completed":0,"failed":0},"metadata":{},"failure":{"code":"input_file_unreadable","message":"gone"},"user_id":1,"api_key_id":8,"created_at":"2026-10-28T09:00:00Z","expires_at":"2026-10-29T09:00:00Z","failed_at":"2026-10-28T10:00:00Z"}`,
		},
		{
			name: "completed batch names its files",
			in: Batch{PriorityClass: "batch",
				ID: "batch_3", Endpoint: "/v1/completions", Status: "completed", InputFileID: "file-in", OutputFileID: "file-out", ErrorFileID: "file-err",
				CompletionWindow: "24h", RequestCounts: BatchRequestCount{Total: 2, Completed: 1, Failed: 1}, Metadata: map[string]string{},
				UserID: 1, CreatedAt: created, ExpiresAt: created.Add(24 * time.Hour), InProgressAt: &started, FinalizingAt: &done, CompletedAt: &done,
				OutputExpiresAt: &kept,
			},
			wire: `{"id":"batch_3","endpoint":"/v1/completions","status":"completed","cancellable":false,"input_file_id":"file-in","output_file_id":"file-out","error_file_id":"file-err","completion_window":"24h","priority_class":"batch","request_counts":{"total":2,"completed":1,"failed":1},"metadata":{},"output_expires_at":"2026-11-27T10:00:00Z","user_id":1,"created_at":"2026-10-28T09:00:00Z","expires_at":"2026-10-29T09:00:00Z","in_progress_at":"2026-10-28T09:00:01Z","finalizing_at":"2026-10-28T10:00:00Z","completed_at":"2026-10-28T10:00:00Z"}`,
		},
		{
			name: "a batch given a model names it, and a create response carries its warnings",
			in: Batch{PriorityClass: "high",
				ID: "batch_4", Cancellable: true, Endpoint: "/v1/chat/completions", Status: "validating", InputFileID: "file-in", CompletionWindow: "24h", Model: "qwen-chat",
				Metadata: map[string]string{}, UserID: 1, CreatedAt: created, ExpiresAt: created.Add(24 * time.Hour),
				Warnings: []messages.Warning{{Code: "BATCH_MODEL_SLEEP_HELD", Params: map[string]any{"model": "qwen-chat"}}},
			},
			wire: `{"id":"batch_4","endpoint":"/v1/chat/completions","status":"validating","cancellable":true,"input_file_id":"file-in","completion_window":"24h","model":"qwen-chat","priority_class":"high","request_counts":{"total":0,"completed":0,"failed":0},"metadata":{},"warnings":[{"code":"BATCH_MODEL_SLEEP_HELD","params":{"model":"qwen-chat"}}],"user_id":1,"created_at":"2026-10-28T09:00:00Z","expires_at":"2026-10-29T09:00:00Z"}`,
		},
		{
			name: "a create response warns about the held models of its file as a list",
			in: Batch{PriorityClass: "batch",
				ID: "batch_6", Endpoint: "/v1/chat/completions", Status: "validating", Cancellable: true, InputFileID: "file-in", CompletionWindow: "24h",
				Metadata: map[string]string{}, UserID: 1, CreatedAt: created, ExpiresAt: created.Add(24 * time.Hour),
				Warnings: []messages.Warning{{Code: "BATCH_FILE_MODELS_SLEEP_HELD", Params: map[string]any{"models": []any{"a", "b"}, "more": float64(0)}}},
			},
			wire: `{"id":"batch_6","endpoint":"/v1/chat/completions","status":"validating","cancellable":true,"input_file_id":"file-in","completion_window":"24h","priority_class":"batch","request_counts":{"total":0,"completed":0,"failed":0},"metadata":{},"warnings":[{"code":"BATCH_FILE_MODELS_SLEEP_HELD","params":{"models":["a","b"],"more":0}}],"user_id":1,"created_at":"2026-10-28T09:00:00Z","expires_at":"2026-10-29T09:00:00Z"}`,
		},
		{
			name: "a batch with a longer completion window expires that much later",
			in: Batch{PriorityClass: "batch",
				ID: "batch_5", Cancellable: true, Endpoint: "/v1/embeddings", Status: "validating", InputFileID: "file-in", CompletionWindow: "72h",
				Metadata: map[string]string{}, UserID: 1, CreatedAt: created, ExpiresAt: created.Add(72 * time.Hour),
			},
			wire: `{"id":"batch_5","endpoint":"/v1/embeddings","status":"validating","cancellable":true,"input_file_id":"file-in","completion_window":"72h","priority_class":"batch","request_counts":{"total":0,"completed":0,"failed":0},"metadata":{},"user_id":1,"created_at":"2026-10-28T09:00:00Z","expires_at":"2026-10-31T09:00:00Z"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.in)
			if err != nil || string(raw) != tc.wire {
				t.Fatalf("marshal\n got %s\nwant %s (%v)", raw, tc.wire, err)
			}
			var back Batch
			if err := json.Unmarshal([]byte(tc.wire), &back); err != nil || !reflect.DeepEqual(back, tc.in) {
				t.Fatalf("round trip = %+v (%v), want %+v", back, err, tc.in)
			}
		})
	}
}

func TestBatchPageWire(t *testing.T) {
	raw, err := json.Marshal(BatchPage{Items: []Batch{}, Page: 1, PageSize: 50, Total: 0})
	if err != nil || string(raw) != `{"items":[],"page":1,"page_size":50,"total":0}` {
		t.Fatalf("empty page = %s (%v)", raw, err)
	}
}
