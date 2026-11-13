package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestAIConfigRoutesExposeOnlyOneDefault(t *testing.T) {
	st, err := store.Open(":memory:", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	signer, err := auth.NewTokenSigner("ai-defaults", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	router := Router(Deps{Store: st, AuthSigner: signer})
	request := func(method, body, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/v1/ai/configs", strings.NewReader(body))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	if rec := request(http.MethodPost, `{"name":"unauthorized","is_default":true}`, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated=%d %s", rec.Code, rec.Body.String())
	}
	for i, isDefault := range []bool{false, true, false} {
		rec := request(http.MethodPost, fmt.Sprintf(`{"name":"config-%d","is_default":%t}`, i, isDefault), token)
		var created map[string]uint
		if err := json.Unmarshal(rec.Body.Bytes(), &created); rec.Code != http.StatusCreated || err != nil || len(created) != 1 || created["id"] == 0 {
			t.Fatalf("create=%d %s err=%v", rec.Code, rec.Body.String(), err)
		}
	}
	rec := request(http.MethodGet, "", token)
	var rows []map[string]any
	if err := unmarshalList(rec.Body.Bytes(), &rows); rec.Code != http.StatusOK || err != nil || len(rows) != 3 {
		t.Fatalf("list=%d %s err=%v", rec.Code, rec.Body.String(), err)
	}
	for _, row := range rows {
		// Eight keys: proxy_url is present only for a custom proxy.
		if len(row) != 8 || row["is_default"] != (row["name"] == "config-1") || row["proxy_mode"] != "management" || row["image_input"] != false {
			t.Fatalf("unexpected wire row=%+v", row)
		}
		if _, ok := row["api_key"]; ok {
			t.Fatal("list exposed API key")
		}
	}
}
