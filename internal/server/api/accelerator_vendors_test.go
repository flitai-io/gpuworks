package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

// The vendor dictionary's wire JSON, byte for byte, is the fixture the
// console's tests load (ui/tests/fixtures/accelerator-vendors.json), so the
// console is tested against what the server sends. A definition that changes
// fails here until the fixture is rewritten with
// GPUWORKS_UPDATE_VENDOR_FIXTURE=1.
func TestAcceleratorVendorsWireJSONIsTheConsoleFixture(t *testing.T) {
	signer, err := auth.NewTokenSigner("vendors-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st := managementSessionTestStore(t)
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "viewer", Role: "viewer"})
	if err != nil {
		t.Fatal(err)
	}
	r := Router(Deps{Store: st, Log: slog.Default(), AuthSigner: signer})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/accelerator-vendors", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, bytes.TrimSpace(rec.Body.Bytes()), "", "  "); err != nil {
		t.Fatal(err)
	}
	pretty.WriteByte('\n')

	const fixture = "../../../ui/tests/fixtures/accelerator-vendors.json"
	want, _ := os.ReadFile(fixture)
	if !bytes.Equal(want, pretty.Bytes()) {
		if os.Getenv("GPUWORKS_UPDATE_VENDOR_FIXTURE") == "" {
			t.Fatalf("the response differs from %s; rerun with GPUWORKS_UPDATE_VENDOR_FIXTURE=1\n got:\n%s", fixture, pretty.String())
		}
		if err := os.WriteFile(fixture, pretty.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The keys the console reads, spelled as on the wire, and an optional
	// one left out rather than sent empty.
	var body struct {
		Items []map[string]json.RawMessage `json:"items"`
		Total int                          `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Total != len(body.Items) || body.Total == 0 {
		t.Fatalf("total = %d for %d items", body.Total, len(body.Items))
	}
	byID := map[string]map[string]json.RawMessage{}
	for _, item := range body.Items {
		var id string
		_ = json.Unmarshal(item["id"], &id)
		byID[id] = item
		for _, key := range []string{"id", "brand", "frameworks", "runtimes", "features"} {
			if _, ok := item[key]; !ok {
				t.Errorf("%s has no %q", id, key)
			}
		}
	}
	if got := string(byID["apple"]["features"]); got != "[]" {
		t.Errorf("a vendor that offers no feature sends features = %s, want []", got)
	}
	if _, sent := byID["apple"]["partition_backend"]; sent {
		t.Error("a vendor without a dGPU backend sends partition_backend")
	}
	if got := string(byID["nvidia"]["partition_backend"]); got != `"nvidia-dgpu"` {
		t.Errorf("NVIDIA partition_backend = %s", got)
	}

	// Reading only: the dictionary answers no other method.
	post := httptest.NewRequest(http.MethodPost, "/api/v1/accelerator-vendors", nil)
	post.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, post)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}
