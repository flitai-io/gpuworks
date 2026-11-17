package apiclient

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"testing"
)

func TestCloudActionPreservesUnicodeWithServletDefaultEncoding(t *testing.T) {
	const description = "GPU 备注\n被gpuworks-server使用中"
	client := clientForTest(t)
	client.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
		if !validTestAccessKey(r) {
			t.Error("request lost AccessKey authentication")
		}
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			t.Fatalf("invalid JSON content type: %q", r.Header.Get("Content-Type"))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		// Cloud 4.8 RestServer reads the Servlet reader, whose default is ISO-8859-1.
		if params["charset"] != "utf-8" {
			decoded := make([]rune, len(body))
			for i, b := range body {
				decoded[i] = rune(b)
			}
			body = []byte(string(decoded))
		}
		var payload struct {
			Update struct {
				Description string `json:"description"`
			} `json:"updatePciDevice"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Update.Description != description {
			t.Errorf("Cloud received description %q, want %q", payload.Update.Description, description)
		}
		return cloudResponse(r, http.StatusOK, `{}`), nil
	})
	if err := client.PutAction(t.Context(), "/pci-device/pci-devices/gpu/actions", map[string]any{
		"updatePciDevice": map[string]string{"description": description},
	}); err != nil {
		t.Fatal(err)
	}
}
