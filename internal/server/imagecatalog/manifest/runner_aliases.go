package manifest

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

// RunnerAlias identifies a retired image reference and its published platform.
type RunnerAlias struct {
	ImageRef string `json:"image_ref"`
	Arch     string `json:"arch"`
}

// The descriptor comparison for each published platform established these
// exact reference pairs. Other repositories may use the same tag independently.
//
//go:embed runner-aliases.json
var runnerAliasJSON []byte

var retiredRunnerTags = sync.OnceValues(func() (map[string][]RunnerAlias, error) {
	var aliases map[string][]RunnerAlias
	if err := json.Unmarshal(runnerAliasJSON, &aliases); err != nil {
		return nil, fmt.Errorf("embedded runner aliases: %w", err)
	}
	return aliases, nil
})

// RetiredRunnerAliases matches the exact current repository and tag.
func RetiredRunnerAliases(ref string) ([]RunnerAlias, error) {
	aliases, err := retiredRunnerTags()
	return aliases[ref], err
}
