package main

import (
	"strings"

	"gopkg.in/yaml.v3"
)

type decisionFrontmatter struct {
	Decision string `yaml:"decision"`
	Rejected string `yaml:"rejected"`
}

func splitFrontmatter(content string) (string, bool) {
	if !strings.HasPrefix(content, "---\n") {
		return "", false
	}
	rest := content[len("---\n"):]
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return "", false
	}
	return rest[:idx], true
}

func decisionRecorded(bodies []string) bool {
	for _, body := range bodies {
		yamlText, ok := splitFrontmatter(body)
		if !ok {
			continue
		}
		var fm decisionFrontmatter
		if err := yaml.Unmarshal([]byte(yamlText), &fm); err != nil {
			continue
		}
		if strings.TrimSpace(fm.Decision) != "" && strings.TrimSpace(fm.Rejected) != "" {
			return true
		}
	}
	return false
}
