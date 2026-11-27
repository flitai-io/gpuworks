package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
)

func TestSeedManifestCommandUsesPublicationTime(t *testing.T) {
	// Even an incomplete checkout in the working directory cannot add files
	// to the source tree embedded in this generator.
	t.Chdir(t.TempDir())
	models := filepath.Join("internal", "server", "inferencecatalog", "seed", "catalog", "models")
	if err := os.MkdirAll(filepath.Join(models, "_local"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{".DS_Store", "_local/model.yaml"} {
		if err := os.WriteFile(filepath.Join(models, file), []byte("invalid YAML: ["), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	catalog, err := seed.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	specs := 0
	for _, model := range catalog.Models {
		specs += len(model.Specs)
	}
	for _, tc := range []struct {
		name string
		date string
		want string
	}{
		{"published", "2026-11-25T01:00:00+08:00", "2026-11-24T17:00:00Z"},
		{"unpublished", "", "1970-01-01T00:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := writeSeedManifest([]string{"--published-at", tc.date}, &output); err != nil {
				t.Fatal(err)
			}
			var got inferencecatalog.PackageManifest
			if err := yaml.Unmarshal(output.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.PublishedAt != tc.want || got.Models != len(catalog.Models) || got.Specs != specs {
				t.Fatalf("manifest for %d embedded models / %d specs at %s: %+v", len(catalog.Models), specs, tc.want, got)
			}
		})
	}
}

func TestSeedManifestCommandRejectsInvalidArguments(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"invalid date", []string{"--published-at", "yesterday"}, "seed publication time"},
		{"unexpected argument", []string{"extra"}, "usage:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			err := writeSeedManifest(tc.args, &output)
			if err == nil || !strings.Contains(err.Error(), tc.want) || output.Len() != 0 {
				t.Fatalf("invalid arguments produced %q, error %v", output.String(), err)
			}
		})
	}
}
