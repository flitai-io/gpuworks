package proto

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// streamLanes is the authoritative lane for every declared stream kind,
// keyed by constant name. Adding a Stream* constant without classifying it
// here fails TestLaneForClassifiesEveryDeclaredStream.
var streamLanes = map[string]string{
	"StreamCtrl":     LaneCtrl,
	"StreamTerminal": LaneCtrl,

	"StreamLogtail":          LaneData,
	"StreamTunnel":           LaneData,
	"StreamPeerRelaySend":    LaneData,
	"StreamPeerRelayReceive": LaneData,
	"StreamModelRead":        LaneData,
	"StreamModelWrite":       LaneData,
	"StreamImageRead":        LaneData,
	"StreamImageWrite":       LaneData,
	"StreamLargeFileRead":    LaneData,
	"StreamAgentUpgrade":     LaneData,
	"StreamDiagnosticsRead":  LaneData,
	"StreamStorageUpload":    LaneData,
	"StreamStorageDownload":  LaneData,
}

func TestLaneForClassifiesEveryDeclaredStream(t *testing.T) {
	declared := declaredStreamKinds(t)
	for name, kind := range declared {
		want, ok := streamLanes[name]
		if !ok {
			t.Errorf("%s = %q has no lane classification in streamLanes", name, kind)
			continue
		}
		if got := LaneFor(kind); got != want {
			t.Errorf("LaneFor(%s = %q) = %q, want %q", name, kind, got, want)
		}
	}
	for name := range streamLanes {
		if _, ok := declared[name]; !ok {
			t.Errorf("streamLanes classifies undeclared stream %s", name)
		}
	}
}

func TestLaneForUnknownKindRidesCtrl(t *testing.T) {
	if got := LaneFor("kind-from-a-newer-peer"); got != LaneCtrl {
		t.Fatalf("unknown kind lane = %q, want %q", got, LaneCtrl)
	}
}

// declaredStreamKinds reads the package source so a new constant is seen
// without anyone remembering to list it.
func declaredStreamKinds(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if !strings.HasPrefix(name.Name, "Stream") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s is not a string literal constant", name.Name)
					}
					value, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					out[name.Name] = value
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no Stream constants found")
	}
	return out
}
