package main

import (
	"bufio"
	"fmt"
	"path"
	"strings"
)

type rule struct {
	class string
	glob  string
}

func parseRules(text string) ([]rule, error) {
	var out []rule
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		class, glob, ok := strings.Cut(line, "\t")
		if !ok {
			class, glob, _ = strings.Cut(line, " ")
		}
		class, glob = strings.TrimSpace(class), strings.TrimSpace(glob)
		if class != "contract" && class != "decision" {
			return nil, fmt.Errorf("unknown class %q", class)
		}
		if glob == "" {
			return nil, fmt.Errorf("empty glob")
		}
		out = append(out, rule{class: class, glob: glob})
	}
	return out, sc.Err()
}

func matchGlob(glob, name string) bool {
	name = path.Clean(name)
	if strings.HasSuffix(glob, "/**") {
		prefix := strings.TrimSuffix(glob, "/**")
		return name == prefix || strings.HasPrefix(name, prefix+"/")
	}
	ok, err := path.Match(glob, name)
	return err == nil && ok
}

func isTestPath(name string) bool {
	base := path.Base(name)
	return strings.HasSuffix(base, "_test.go") ||
		strings.HasSuffix(base, ".test.ts") ||
		strings.HasSuffix(base, ".test.tsx")
}

func classify(rules []rule, files []string) (contract, decision []string) {
	for _, file := range files {
		if isTestPath(file) {
			continue
		}
		for _, r := range rules {
			if !matchGlob(r.glob, file) {
				continue
			}
			switch r.class {
			case "contract":
				contract = append(contract, file)
			case "decision":
				decision = append(decision, file)
			}
			break
		}
	}
	return contract, decision
}

// contractSatisfied is true when the same diff changes a product doc or an ADR.
// docs-en/ is a translation, not the record.
func contractSatisfied(files []string) bool {
	for _, file := range files {
		if file == "docs" || strings.HasPrefix(file, "docs/") {
			return true
		}
	}
	return false
}
