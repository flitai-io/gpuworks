package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestEvaluateLoadsEligibilityInventoryOnceForValidAndInvalidDrafts(t *testing.T) {
	h, _ := eligibleNodeFixture(t)
	models, err := h.d.Store.ListModels()
	if err != nil || len(models) == 0 {
		t.Fatalf("model fixture: %v", err)
	}
	var queries atomic.Int64
	const callback = "test:eligibility-inventory-count"
	if err := h.d.Store.DB().Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if _, allNodes := tx.Statement.Dest.(*[]store.Node); allNodes && tx.Statement.Table == "nodes" && tx.Statement.Clauses["WHERE"].Expression == nil {
			queries.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer h.d.Store.DB().Callback().Query().Remove(callback)
	for _, tc := range []struct{ name, extra string }{{"valid", ""}, {"invalid", `,"start_timeout_sec":-1`}} {
		t.Run(tc.name, func(t *testing.T) {
			queries.Store(0)
			body := fmt.Sprintf(`{"model_id":%d,"node_uuid":"eligible-node","engine":"vllm","quick_params":{"max_model_len":512}%s}`, models[0].ID, tc.extra)
			rec := httptest.NewRecorder()
			h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if got := queries.Load(); got != 1 {
				t.Fatalf("eligibility full-node inventory queries=%d, want 1", got)
			}
		})
	}
}
