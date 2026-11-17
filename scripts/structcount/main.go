// Command structcount measures how much state and behaviour one struct type
// carries, for the architecture ratchet (scripts/check_architecture.sh,
// ADR 0066). It parses the package's non-test files and prints one number:
//
//	-measure fields   field names, counting inside anonymous structs (wrapping
//	                  fields in one does not make them leave the type)
//	-measure mutexes  sync.Mutex / sync.RWMutex fields, by value or pointer,
//	                  inside anonymous structs too
//	-measure maps     map fields, sync.Map fields and fields of a map type
//	                  declared in the package, inside anonymous structs too
//	-measure methods  methods with the type as receiver (value or pointer)
//	-measure rpcs     distinct proto.RPC* constants used as switch case values
//	                  anywhere in the package (a helper switch counts too)
//
// A field of a named struct type counts once: moving state into a type of
// its own is how it leaves the composition root.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	dir := flag.String("dir", "", "package directory")
	typ := flag.String("type", "", "struct type name")
	measure := flag.String("measure", "", "fields | mutexes | maps | methods | rpcs")
	flag.Parse()
	n, err := count(*dir, *typ, *measure)
	if err != nil {
		fmt.Fprintln(os.Stderr, "structcount:", err)
		os.Exit(1)
	}
	fmt.Println(n)
}

func count(dir, typ, measure string) (int, error) {
	files, err := parsePackage(dir)
	if err != nil {
		return 0, err
	}
	switch measure {
	case "methods":
		return countMethods(files, typ), nil
	case "rpcs":
		return countRPCCases(files), nil
	case "fields", "mutexes", "maps":
	default:
		return 0, fmt.Errorf("unknown measure %q", measure)
	}
	types := localTypes(files)
	spec, ok := types[typ]
	if !ok {
		return 0, fmt.Errorf("type %s not found in %s", typ, dir)
	}
	st, ok := spec.Type.(*ast.StructType)
	if !ok {
		return 0, fmt.Errorf("%s is not a struct", typ)
	}
	return countFields(st, measure, types), nil
}

func parsePackage(dir string) ([]*ast.File, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no Go files in %s", dir)
	}
	return files, nil
}

func localTypes(files []*ast.File) map[string]*ast.TypeSpec {
	out := map[string]*ast.TypeSpec{}
	for _, f := range files {
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.TYPE {
				continue
			}
			for _, s := range g.Specs {
				ts := s.(*ast.TypeSpec)
				out[ts.Name.Name] = ts
			}
		}
	}
	return out
}

func countFields(st *ast.StructType, measure string, types map[string]*ast.TypeSpec) int {
	n := 0
	for _, f := range st.Fields.List {
		names := len(f.Names)
		if names == 0 {
			names = 1 // embedded
		}
		t := f.Type
		if star, ok := t.(*ast.StarExpr); ok {
			t = star.X
		}
		if inner, ok := t.(*ast.StructType); ok {
			n += names * countFields(inner, measure, types)
			if measure == "fields" {
				n += names
			}
			continue
		}
		switch measure {
		case "fields":
			n += names
		case "mutexes":
			if isSync(t, "Mutex") || isSync(t, "RWMutex") {
				n += names
			}
		case "maps":
			if isMap(t, types) {
				n += names
			}
		}
	}
	return n
}

func isSync(t ast.Expr, name string) bool {
	sel, ok := t.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "sync" && sel.Sel.Name == name
}

func isMap(t ast.Expr, types map[string]*ast.TypeSpec) bool {
	switch t := t.(type) {
	case *ast.MapType:
		return true
	case *ast.SelectorExpr:
		return isSync(t, "Map")
	case *ast.Ident:
		if spec, ok := types[t.Name]; ok {
			_, isMapType := spec.Type.(*ast.MapType)
			return isMapType
		}
	}
	return false
}

func countMethods(files []*ast.File, typ string) int {
	n := 0
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
				continue
			}
			t := fn.Recv.List[0].Type
			if star, ok := t.(*ast.StarExpr); ok {
				t = star.X
			}
			if id, ok := t.(*ast.Ident); ok && id.Name == typ {
				n++
			}
		}
	}
	return n
}

func countRPCCases(files []*ast.File) int {
	seen := map[string]bool{}
	for _, f := range files {
		ast.Inspect(f, func(node ast.Node) bool {
			cc, ok := node.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, e := range cc.List {
				sel, ok := e.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "proto" && strings.HasPrefix(sel.Sel.Name, "RPC") {
					seen[sel.Sel.Name] = true
				}
			}
			return true
		})
	}
	return len(seen)
}
