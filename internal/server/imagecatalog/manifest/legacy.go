package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
)

const legacyVersionSuffix = "-schema3"

// The runner catalog generator identifies releases with a date and eight hex digits.
var publishedCatalogVersion = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}\.[0-9a-f]{8}$`)

// IsLegacyProjection identifies the lossy catalog emitted for older readers.
func (m Manifest) IsLegacyProjection() bool {
	if m.SchemaVersion != 3 {
		return false
	}
	if version, ok := strings.CutSuffix(m.CatalogVersion, legacyVersionSuffix); ok {
		return publishedCatalogVersion.MatchString(version)
	}
	// Previously published projections used schema3.<SHA-256>.
	suffix, ok := strings.CutPrefix(m.CatalogVersion, "schema3.")
	if !ok || len(suffix) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

// A schema 3 reader selects a partially eligible multi-platform image as multi.
// Only manual releases need to be hidden from its automatic selector.
// Its deprecated rows still resolve explicit references and their mirrors.
func LegacyCatalog(body []byte) ([]byte, error) {
	m, err := Parse(body)
	if err != nil || m.SchemaVersion != RunnerSchema {
		return body, err
	}
	m.SchemaVersion = 3
	for _, series := range m.Series {
		for i := range series.Versions {
			v := &series.Versions[i]
			if v.AutoSelect == "manual" && !v.Deprecated {
				v.Deprecated = true
				v.DeprecatedSince = m.CatalogVersion
			}
			v.AutoSelect = ""
			v.RegistryDigest = ""
		}
	}
	m.Signature = ""
	if publishedCatalogVersion.MatchString(m.CatalogVersion) {
		m.CatalogVersion += legacyVersionSuffix
	} else {
		// A custom source version must still produce a recognizable projection.
		m.CatalogVersion = "schema3." + ComputeSHA256(m)
	}
	m.SHA256 = ComputeSHA256(m)
	body, err = json.MarshalIndent(m, "", "  ")
	return append(body, '\n'), err
}
