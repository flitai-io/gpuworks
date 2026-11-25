package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestSeedManifestCommandUsesPublicationTime(t *testing.T) {
	source := filepath.Join("..", "..", "internal", "server", "inferencecatalog", "seed", "catalog")
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
			if err := writeSeedManifest([]string{"--catalog", source, "--published-at", tc.date}, &output); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), tc.want) {
				t.Fatalf("manifest missing publication time %s: %s", tc.want, &output)
			}
		})
	}
}
