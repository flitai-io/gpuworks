package experience

import (
	"context"
	"errors"
	"net/http"
	"os"
)

// Keep a user-owned template in the initial catalog so visitors can try
// deleting a spec or a whole template without touching a starter deployment.
func seedExampleTemplate(ctx context.Context, c *apiClient, marker string) error {
	if _, err := os.Stat(marker); err == nil || !errors.Is(err, os.ErrNotExist) {
		return err
	}
	const id = "experience/qwen3-8b"
	// A previous run may have created the clone but stopped before the marker.
	// Preserve that copy, including edits made by a visitor.
	status, err := c.callStatus(ctx, http.MethodGet, "/api/v1/inference-templates/model/"+id, nil, nil)
	if status == http.StatusNotFound {
		err = c.post(ctx, "/api/v1/inference-templates/clone", map[string]string{
			"source_model_id": "qwen/qwen3-8b", "model_id": id, "display_title": "Qwen3-8B · Experience",
		}, nil)
	}
	if err != nil {
		return err
	}
	return os.WriteFile(marker, nil, 0o600)
}
