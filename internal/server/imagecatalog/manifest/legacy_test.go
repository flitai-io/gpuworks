package manifest

import (
	"strings"
	"testing"
)

func TestLegacyCatalogSelection(t *testing.T) {
	raw := []byte(`{"schema_version":4,"catalog_version":"2026-11-26.0123abcd","series":[{"name":"runner","versions":[
 {"image_ref":"runner:manual","auto_select":"manual","registry_sources":["quay.io/team/runner:manual"]},
 {"image_ref":"runner:partial","arch":"multi","auto_select":"arm64"},
 {"image_ref":"runner:general","arch":"amd64"},
 {"image_ref":"runner:retired","auto_select":"manual","deprecated":true,"deprecated_since":"earlier"}]}]}`)
	body, err := LegacyCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if !m.IsLegacyProjection() || m.SHA256 == "" {
		t.Fatalf("unexpected legacy identity %v %s", m.SchemaVersion, m.CatalogVersion)
	}
	versions := m.Series[0].Versions
	for i, want := range []bool{true, false, false, true} {
		if versions[i].Deprecated != want || versions[i].AutoSelect != "" {
			t.Errorf("%s: deprecated=%t auto_select=%q", versions[i].ImageRef, versions[i].Deprecated, versions[i].AutoSelect)
		}
	}
	if m.CatalogVersion != "2026-11-26.0123abcd-schema3" || versions[0].DeprecatedSince != "2026-11-26.0123abcd" || versions[3].DeprecatedSince != "earlier" {
		t.Fatalf("version %q, deprecated since %q / %q", m.CatalogVersion, versions[0].DeprecatedSince, versions[3].DeprecatedSince)
	}
	if versions[1].Arch != "multi" || len(versions[0].RegistrySources) != 1 || versions[0].RegistrySources[0] != "quay.io/team/runner:manual" {
		t.Fatal("lost multi-platform support or template pull sources")
	}
}

func TestLegacyProjectionMarkers(t *testing.T) {
	old := "schema3." + strings.Repeat("a", 64)
	for _, tc := range []struct {
		schema  int
		version string
		want    bool
	}{
		{3, old, true},
		{3, "2026-11-26.0123abcd-schema3", true},
		{3, "local-release-schema3", false},
		{3, "2026-11-26.0123abcg-schema3", false},
		{3, "2026-11-26.0123abcde-schema3", false},
		{3, "schema3." + strings.Repeat("g", 64), false},
		{3, "2026-11-26.0123abcd", false},
		{4, old, false},
		{4, "release-schema3", false},
	} {
		m := Manifest{SchemaVersion: tc.schema, CatalogVersion: tc.version}
		if got := m.IsLegacyProjection(); got != tc.want {
			t.Errorf("schema %d version %q: got %t, want %t", tc.schema, tc.version, got, tc.want)
		}
	}
}

func TestLegacyCatalogWithCustomVersion(t *testing.T) {
	body, err := LegacyCatalog([]byte(`{"schema_version":4,"catalog_version":"local","series":[{"name":"runner","versions":[{"image_ref":"runner:local"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	m, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if !m.IsLegacyProjection() {
		t.Fatalf("unrecognized generated projection %q", m.CatalogVersion)
	}
}
