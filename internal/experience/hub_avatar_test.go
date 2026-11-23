package experience

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

func TestPreloadedModelUsesPackagedPublisherAvatar(t *testing.T) {
	shapes, err := catalogShapes()
	if err != nil {
		t.Fatal(err)
	}
	assets := os.DirFS("../../ui/public")
	srv := httptest.NewServer(&hub{shapes: shapes, assets: assets})
	defer srv.Close()
	env := testenv.NewWithModelEndpoint(t, srv.URL)
	client, err := login(t.Context(), env.URL(), "admin", testenv.AdminPassword, "")
	if err != nil {
		t.Fatal(err)
	}
	var fresh dto.Model
	if err := client.post(t.Context(), "/api/v1/models", map[string]string{"source": "huggingface", "source_id": "Qwen/Qwen3-8B"}, &fresh); err != nil || fresh.LogoPath == "" {
		t.Fatalf("registered model has logo %q: %v", fresh.LogoPath, err)
	}
	// A previous version already registered this artifact and wrote its checkpoints.
	old, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3-32B", Name: "Custom display name"})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, n := range scenario {
		for _, repo := range n.Models {
			dir := filepath.Join(root, "nodes", n.Hostname, "models", "local", strings.ReplaceAll(repo, "/", "--"))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := seedModels(t.Context(), client, root, nil, shapes); err != nil {
		t.Fatal(err)
	}
	repaired, err := env.Store().GetModel(old.ID)
	if err != nil || repaired.LogoPath != fresh.LogoPath || repaired.Name != old.Name {
		t.Fatalf("repaired model = %v, error %v", repaired, err)
	}
	want, err := os.ReadFile("../../ui/public/model-org-icons/qwen.webp")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s/api/v1/models/%d/logo", env.URL(), old.ID), nil)
	req.Header.Set("Authorization", "Bearer "+client.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/webp" || !bytes.Equal(got, want) {
		t.Fatalf("logo status %d, type %q, size %d, error %v", resp.StatusCode, resp.Header.Get("Content-Type"), len(got), err)
	}
	for _, path := range []string{"/api/organizations/nonexistent/avatar", "/model-org-icons/../index.html"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: got %d", path, resp.StatusCode)
		}
	}
}
