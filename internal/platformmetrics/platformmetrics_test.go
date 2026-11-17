package platformmetrics

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/inferencemetrics"
)

// metricLiteral is what a stored series name looks like. Literals ending in
// "_" are prefixes a producer completes, not names.
var metricLiteral = regexp.MustCompile(`^gpuworks_[a-z0-9]+(_[a-z0-9]+)*$`)

// notMetrics are gpuworks_-prefixed literals that are not series names.
var notMetrics = map[string]string{
	"gpuworks_session":  "the management session cookie",
	"gpuworks_callable": "the callable adapter's metric prefix, which the agent maps through inferencemetrics",
}

// knownSeries is every series name the two vocabularies define, with the
// histogram families' stored _sum, _count and _bucket series.
func knownSeries() map[string]bool {
	known := map[string]bool{}
	for _, d := range Definitions() {
		known[d.Series] = true
	}
	histogram := func(series string, kind inferencemetrics.Kind) {
		known[series] = true
		if kind == inferencemetrics.Histogram {
			for _, suffix := range []string{"_sum", "_count", "_bucket"} {
				known[series+suffix] = true
			}
		}
	}
	for _, d := range inferencemetrics.Definitions() {
		histogram(d.Series(), d.Kind)
	}
	for _, d := range inferencemetrics.GatewayDefinitions() {
		histogram(d.Series(), d.Kind)
	}
	return known
}

// TestEveryMetricLiteralIsRegistered reads every series name the code spells
// out - in the agent's producers, the server's writers and queries, and the
// UI's charts - and requires it in a vocabulary, so the catalog the API
// answers lists everything that can be stored or asked for.
func TestEveryMetricLiteralIsRegistered(t *testing.T) {
	root := repoRoot(t)
	known := knownSeries()
	found := map[string][]string{}
	for _, dir := range []string{"internal", "pkg", "cmd"} {
		goLiterals(t, filepath.Join(root, dir), root, found)
	}
	uiLiterals(t, filepath.Join(root, "ui", "src"), root, found)
	if len(found) < 30 {
		t.Fatalf("found only %d metric literals; the scan is not reading the producers", len(found))
	}
	var missing []string
	for name, where := range found {
		if known[name] || notMetrics[name] != "" {
			continue
		}
		missing = append(missing, name+" ("+strings.Join(where, ", ")+")")
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("series named in code but not in platformmetrics or inferencemetrics:\n%s", strings.Join(missing, "\n"))
	}
}

func TestDefinitionsAreComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range Definitions() {
		if seen[d.Series] {
			t.Fatalf("%s listed twice", d.Series)
		}
		seen[d.Series] = true
		if !metricLiteral.MatchString(d.Series) || d.Source == "" || d.Kind == "" || d.Unit == "" || d.Description == "" || len(d.Labels) == 0 {
			t.Fatalf("incomplete definition %+v", d)
		}
		if got, ok := Lookup(d.Series); !ok || got.Series != d.Series {
			t.Fatalf("Lookup(%q) = %+v, %v", d.Series, got, ok)
		}
	}
	if _, ok := Lookup("gpuworks_gpu_no_such_series"); ok {
		t.Fatal("Lookup found an unknown series")
	}
}

func goLiterals(t *testing.T, dir, root string, found map[string][]string) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || name == "node_modules" || name == "webui" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if v, err := strconv.Unquote(lit.Value); err == nil && metricLiteral.MatchString(v) {
				found[v] = appendOnce(found[v], rel)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

var tsString = regexp.MustCompile("[\"'`](gpuworks_[a-z0-9_]+)[\"'`]")

func uiLiterals(t *testing.T, dir, root string, found map[string][]string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !(strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".tsx")) {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, m := range tsString.FindAllStringSubmatch(string(body), -1) {
			if metricLiteral.MatchString(m[1]) {
				found[m[1]] = appendOnce(found[m[1]], rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func appendOnce(list []string, v string) []string {
	for _, have := range list {
		if have == v {
			return list
		}
	}
	return append(list, v)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
