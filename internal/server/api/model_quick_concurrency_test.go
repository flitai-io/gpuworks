package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestQuickRegistrationConcurrentIdentityReturnsOneModel(t *testing.T) {
	st, err := store.Open(":memory:", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	entered, release := make(chan struct{}, 2), make(chan struct{})
	if err := st.DB().Callback().Create().Before("gorm:begin_transaction").Register("test:concurrent_model_insert", func(tx *gorm.DB) {
		if tx.Statement.Table != "models" {
			return
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			tx.AddError(errors.New("concurrent insert barrier timed out"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer st.DB().Callback().Create().Remove("test:concurrent_model_insert")
	h := &apiHandlers{d: Deps{Store: st, FetchModelMeta: func(context.Context, string, string, string, string, []string) (*modelmeta.Meta, error) {
		return nil, errors.New("offline")
	}}}
	results := make([]*httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = httptest.NewRecorder()
			h.quickRegisterModel(results[i], httptest.NewRequest(http.MethodPost, "/api/v1/models/quick-register", strings.NewReader(fmt.Sprintf(`{"source":"huggingface","source_id":"Org/Concurrent","name":"request-%d"}`, i))))
		}(i)
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			close(release)
			wg.Wait()
			t.Fatal("both requests did not reach insert")
		}
	}
	close(release)
	wg.Wait()
	var id uint
	var name string
	for _, rec := range results {
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var wire dto.Model
		if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
			t.Fatal(err)
		}
		if wire.ID == 0 || (id != 0 && id != wire.ID) {
			t.Fatalf("model id=%d previous=%d body=%s", wire.ID, id, rec.Body.String())
		}
		id = wire.ID
		if name != "" && wire.Name != name {
			t.Fatalf("conflicting registration overwrote name: %q then %q", name, wire.Name)
		}
		name = wire.Name
	}
	rows, err := st.ListModels()
	if err != nil || len(rows) != 1 || rows[0].Name != name {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

func TestQuickRegistrationPreservesWinnerBeforeUpsertLookup(t *testing.T) {
	st, err := store.Open(":memory:", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	winner := store.Model{Source: "huggingface", SourceID: "Org/Concurrent", Name: "winning request", WeightBytes: 123, ContextLength: 4096}
	var once sync.Once
	if err := st.DB().Callback().Query().After("gorm:query").Register("test:commit_registration_winner", func(tx *gorm.DB) {
		if tx.Statement.Table != "models" || !errors.Is(tx.Error, gorm.ErrRecordNotFound) {
			return
		}
		// Another request commits after the handler's initial lookup but
		// before UpsertModel repeats that lookup. No timing or sleep is needed.
		once.Do(func() {
			if err := st.DB().Create(&winner).Error; err != nil {
				tx.AddError(err)
			}
		})
	}); err != nil {
		t.Fatal(err)
	}
	defer st.DB().Callback().Query().Remove("test:commit_registration_winner")
	h := &apiHandlers{d: Deps{Store: st, FetchModelMeta: func(context.Context, string, string, string, string, []string) (*modelmeta.Meta, error) {
		return nil, errors.New("offline")
	}}}
	rec := httptest.NewRecorder()
	h.quickRegisterModel(rec, httptest.NewRequest(http.MethodPost, "/api/v1/models/quick-register", strings.NewReader(`{"source":"huggingface","source_id":"Org/Concurrent","name":"losing request"}`)))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var wire dto.Model
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	if wire.ID != winner.ID || wire.Name != winner.Name {
		t.Fatalf("winner overwritten: id=%d name=%q, want id=%d name=%q", wire.ID, wire.Name, winner.ID, winner.Name)
	}
	stored, err := st.GetModel(winner.ID)
	if err != nil || stored.Name != winner.Name || stored.WeightBytes != winner.WeightBytes || stored.ContextLength != winner.ContextLength {
		t.Fatalf("winning metadata changed: %+v err=%v", stored, err)
	}
}
