package experience

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
)

func seedBatch(ctx context.Context, c *apiClient) error {
	// The tasks themselves survive a restart, including while in progress.
	// Populate only an empty page; existing visitor tasks already serve as examples.
	var page struct {
		Total int `json:"total"`
	}
	if err := c.get(ctx, "/api/v1/batches?page_size=1", &page); err != nil || page.Total > 0 {
		return err
	}
	target, err := waitRunning(ctx, c, sampleDeployment.Name)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	file, err := form.CreateFormFile("file", "chat-examples.jsonl")
	if err != nil {
		return err
	}
	encode := json.NewEncoder(file)
	for i, question := range []string{"什么是模型的上下文窗口？", "什么是首个 token 延迟？"} {
		if err := encode.Encode(map[string]any{
			"custom_id": fmt.Sprintf("example-%d", i+1), "method": "POST", "url": "/v1/chat/completions",
			"body": map[string]any{"model": target.ServedName, "max_tokens": 192,
				"messages": []map[string]string{{"role": "user", "content": question}}},
		}); err != nil {
			return err
		}
	}
	if err := form.Close(); err != nil {
		return err
	}
	return c.post(ctx, "/api/v1/batches", &multipartBody{buf.Bytes(), form.FormDataContentType()}, nil)
}
