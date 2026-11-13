package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestDeleteImageAPIRejectsRunningReference(t *testing.T) {
	st, err := store.Open("file:api_delete_image?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	series := &store.ImageSeries{Name: "api-community-delete", Source: "community", Enabled: true}
	if err := st.DB().Create(series).Error; err != nil {
		t.Fatal(err)
	}
	image := &store.Image{SeriesID: series.ID, ImageRef: "example/api:v1", Source: "community", Enabled: true}
	if err := st.DB().Create(image).Error; err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "api-image-ref", DeploymentID: dep.ID, Image: image.ImageRef, State: "running"}); err != nil {
		t.Fatal(err)
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", strconv.FormatUint(uint64(image.ID), 10))
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/images/1", nil).WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).deleteImage(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := st.GetImage(image.ID); errors.Is(err, store.ErrNotFound) {
		t.Fatal("referenced image was deleted")
	}
}
