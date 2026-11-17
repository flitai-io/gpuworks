package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

func TestDeleteImageAPIRefusesAnOfficialImageWithItsCode(t *testing.T) {
	st, err := store.Open("file:api_delete_official_image?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	series := &store.ImageSeries{Name: "api-official-delete", Source: "official", Enabled: true}
	if err := st.DB().Create(series).Error; err != nil {
		t.Fatal(err)
	}
	image := &store.Image{SeriesID: series.ID, ImageRef: "example/official:v1", Source: "official", Enabled: true}
	if err := st.DB().Create(image).Error; err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]struct {
		id     uint
		handle func(*apiHandlers, http.ResponseWriter, *http.Request)
	}{
		"version": {image.ID, (*apiHandlers).deleteImage},
		"series":  {series.ID, (*apiHandlers).deleteImageSeries},
	} {
		t.Run(name, func(t *testing.T) {
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", strconv.FormatUint(uint64(call.id), 10))
			req := httptest.NewRequest(http.MethodDelete, "/api/v1/images/x", nil).WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))
			rec := httptest.NewRecorder()
			call.handle(&apiHandlers{d: Deps{Store: st}}, rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"`+ErrorOfficialImageDeleteForbidden+`"`) {
				t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
			}
		})
	}
	if err := st.DeleteImage(image.ID); !errors.Is(fmt.Errorf("delete: %w", err), store.ErrCatalogManaged) {
		t.Fatalf("store error = %v, want ErrCatalogManaged", err)
	}
}
