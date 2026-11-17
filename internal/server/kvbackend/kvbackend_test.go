package kvbackend

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

func externalRow() Backend {
	return Backend{
		ID: "testext", Engine: engines.VLLM, Label: "Test External", External: true,
		Vendors: []string{accelerator.VendorNVIDIA}, Replaces: []string{"kv_offloading_size"},
		PDRoles: []string{pdmode.RolePrefill}, Topology: Topology{MaxNNodes: 1},
	}
}

func TestBuiltinIsValidAndHoldsOnlyTheNativeRow(t *testing.T) {
	registry := Builtin()
	if _, err := New(registry.rows...); err != nil {
		t.Fatalf("Builtin rows fail New: %v", err)
	}
	rows := registry.ForEngine(engines.VLLM)
	if len(rows) != 1 || rows[0].ID != Native || !rows[0].Default || rows[0].External {
		t.Fatalf("vLLM rows = %+v, want the native default row only", rows)
	}
	if !slices.Equal(rows[0].PDRoles, []string{pdmode.RolePrefill}) {
		t.Fatalf("native PDRoles = %v, want [prefill]", rows[0].PDRoles)
	}
	if !rows[0].TopologyAllows(4, true, true) {
		t.Fatal("native row must allow every topology")
	}
	if got := registry.ForEngine(engines.SGLang); len(got) != 0 {
		t.Fatalf("SGLang rows = %+v, want none", got)
	}
	for _, engine := range engines.DialectNames() {
		if got := registry.Selectable(engine); len(got) != 0 {
			t.Fatalf("%s selectable rows = %+v, want none while only native ships", engine, got)
		}
	}
}

func TestResolve(t *testing.T) {
	registry, err := New(Builtin().rows[0], externalRow())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		engine, id, want string
		ok               bool
	}{
		{engines.VLLM, "", Native, true},
		{engines.VLLM, Native, Native, true},
		{engines.VLLM, "testext", "testext", true},
		{engines.VLLM, "missing", "", false},
		{engines.SGLang, "", "", false},
		{engines.SGLang, "testext", "", false},
	} {
		got, ok := registry.Resolve(tc.engine, tc.id)
		if ok != tc.ok || got.ID != tc.want {
			t.Errorf("Resolve(%q, %q) = %q, %v; want %q, %v", tc.engine, tc.id, got.ID, ok, tc.want, tc.ok)
		}
	}
	selectable := registry.Selectable(engines.VLLM)
	if len(selectable) != 1 || selectable[0].ID != "testext" {
		t.Fatalf("Selectable = %+v, want the external row only", selectable)
	}
	// ForEngine keeps declaration order and hands out copies.
	rows := registry.ForEngine(engines.VLLM)
	rows[1].Replaces[0] = "mutated"
	if again, _ := registry.Resolve(engines.VLLM, "testext"); again.Replaces[0] != "kv_offloading_size" {
		t.Fatal("ForEngine exposes the registry's own slices")
	}
}

func TestBackendPredicates(t *testing.T) {
	row := externalRow()
	if !row.KeptOn(pdmode.RolePrefill) || row.KeptOn(pdmode.RoleDecode) {
		t.Fatal("KeptOn must follow PDRoles")
	}
	if !row.Supports("NVIDIA") || !row.Supports("nvidia") || row.Supports(accelerator.VendorAscend) || row.Supports("") {
		t.Fatal("Supports must normalize vendors and refuse those not listed")
	}
	if !(Backend{}).Supports(accelerator.VendorKunlunxin) {
		t.Fatal("an empty vendor list allows every vendor")
	}
	for _, tc := range []struct {
		nodes  int
		pp, dp bool
		want   bool
	}{{1, false, false, true}, {0, false, false, true}, {2, false, false, false}, {1, true, false, false}, {1, false, true, false}} {
		if got := row.TopologyAllows(tc.nodes, tc.pp, tc.dp); got != tc.want {
			t.Errorf("TopologyAllows(%d, %v, %v) = %v, want %v", tc.nodes, tc.pp, tc.dp, got, tc.want)
		}
	}
}

func TestNewRejectsBrokenTables(t *testing.T) {
	native := Builtin().rows[0]
	mutate := func(edit func(*Backend)) Backend {
		row := externalRow()
		edit(&row)
		return row
	}
	for name, rows := range map[string][]Backend{
		"empty id":            {mutate(func(b *Backend) { b.ID = "" })},
		"empty engine":        {mutate(func(b *Backend) { b.Engine = "" })},
		"not a dialect":       {mutate(func(b *Backend) { b.Engine = engines.Callable })},
		"duplicate":           {externalRow(), externalRow()},
		"two defaults":        {native, mutate(func(b *Backend) { b.ID, b.External, b.Default, b.Replaces = "other", false, true, nil })},
		"external default":    {mutate(func(b *Backend) { b.Default = true })},
		"unknown vendor":      {mutate(func(b *Backend) { b.Vendors = []string{"voodoo"} })},
		"alias vendor":        {mutate(func(b *Backend) { b.Vendors = []string{"NVIDIA"} })},
		"unknown role":        {mutate(func(b *Backend) { b.PDRoles = []string{"router"} })},
		"not kept on prefill": {mutate(func(b *Backend) { b.PDRoles = []string{pdmode.RoleDecode} })},
		"kept on no role":     {mutate(func(b *Backend) { b.PDRoles = nil })},
		"empty label":         {mutate(func(b *Backend) { b.Label = "" })},
		"negative max nodes":  {mutate(func(b *Backend) { b.Topology.MaxNNodes = -1 })},
		"default replaces":    {mutate(func(b *Backend) { b.External, b.Default = false, true; b.Replaces = []string{"x"} })},
	} {
		if _, err := New(rows...); err == nil {
			t.Errorf("%s: New accepted %+v", name, rows)
		}
	}
}

// The leaf keeps no mutable package state and registers nothing at init:
// every table is built inside a function, so a test can only swap a whole
// Registry value its caller owns.
func TestPackageHasNoInitAndNoPackageVars(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, source, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Name.Name == "init" && d.Recv == nil {
					t.Errorf("%s declares init()", name)
				}
			case *ast.GenDecl:
				if d.Tok == token.VAR {
					t.Errorf("%s declares a package-level var", name)
				}
			}
		}
	}
}
