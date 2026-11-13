package deploy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// Keep the local-template dialog's literal response tied to the complete HTTP
// journey rather than a permissive hand-built UI stub.
func checkLocalTemplateUIFixture(t *testing.T, response []byte, fixtureName ...string) {
	t.Helper()
	name := "exported-local"
	if len(fixtureName) > 0 {
		name = fixtureName[0]
	}
	path := fmt.Sprintf("../../../ui/tests/fixtures/catalog-template/%s.json", name)
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, response, "", "  "); err != nil {
		t.Fatal(err)
	}
	got := append(bytes.TrimRight(pretty.Bytes(), "\n"), '\n')
	if os.Getenv("GPUWORKS_UPDATE_UI_FIXTURES") != "" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			t.Error("regenerated the local template UI fixture; rerun without GPUWORKS_UPDATE_UI_FIXTURES")
		})
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("local template HTTP response drifted from the UI fixture; review and regenerate with GPUWORKS_UPDATE_UI_FIXTURES=1")
	}
}
