package main

import (
	"go/ast"
	goparser "go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// notSentYet are the capability words the API table declares that no hab sends yet, with the
// reason. A word here that a hab starts to send fails the test, so the list only shrinks.
var notSentYet = map[string]string{
	"devices":          "the embedded SSH server and devices.* are not built (hab-logic-api.md 3.4)",
	"snapshot-ghostty": "data streams offer only snapshot-vt so far (hab-logic-api.md 5.5, Q6)",
}

// habCapSources are the code that puts words in the welcome, by folder and function: serve's
// caps list, and the control server, which adds idempotency-keys and push ("" is every
// function).
var habCapSources = []struct{ dir, fn string }{{"cmd/gff-hab", "serve"}, {"internal/control", ""}}

// TestCapabilitiesDeclared: every capability word the hab can send is in api.Capabilities (and
// so in schema.json and the generated clients), and every word there is one the hab can send,
// or is listed in notSentYet (hab-logic-api.md 3.4). The words are read from the source: each
// assignment to caps or to a Capabilities field, as a list or an append.
func TestCapabilitiesDeclared(t *testing.T) {
	root := filepath.Join("..", "..")
	sent := map[string]string{}
	for _, src := range habCapSources {
		for w, at := range capWords(t, root, src.dir, src.fn) {
			sent[w] = at
		}
	}
	if len(sent) < 10 {
		t.Fatalf("found only %d capability words: the scan no longer matches the code", len(sent))
	}
	for w, at := range sent {
		if !slices.Contains(api.Capabilities, w) {
			t.Errorf("the hab sends %q (%s), which api.Capabilities does not declare", w, at)
		}
		if why, ok := notSentYet[w]; ok {
			t.Errorf("the hab sends %q (%s); drop it from notSentYet (%s)", w, at, why)
		}
	}
	for _, w := range api.Capabilities {
		if _, ok := sent[w]; !ok && notSentYet[w] == "" {
			t.Errorf("api.Capabilities declares %q, which no hab sends", w)
		}
	}
}

// capWords returns the capability words assigned in the package in dir, in the function fn or
// in any, each with where.
func capWords(t *testing.T, root, dir, fn string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	files := parseDir(t, fset, filepath.Join(root, dir))
	consts := constStrings(files)
	out := map[string]string{}
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil || fn != "" && (fd.Name.Name != fn || fd.Recv != nil) {
				continue
			}
			scanCaps(t, root, fset, f, consts, fd.Body, out)
		}
	}
	return out
}

// scanCaps adds the words of each assignment to caps or a Capabilities field under body.
func scanCaps(t *testing.T, root string, fset *token.FileSet, f *ast.File, consts map[string]string, body ast.Node, out map[string]string) {
	t.Helper()
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 || !capTarget(as.Lhs[0]) {
			return true
		}
		var exprs []ast.Expr
		switch r := as.Rhs[0].(type) {
		case *ast.CompositeLit:
			exprs = r.Elts
		case *ast.CallExpr:
			if id, ok := r.Fun.(*ast.Ident); ok && id.Name == "append" {
				exprs = r.Args[1:]
			}
		}
		for _, e := range exprs {
			at := fset.Position(e.Pos()).String()
			w, ok := resolve(t, root, f, consts, e)
			if !ok {
				t.Errorf("%s: cannot read the capability word of this expression; extend the scan", at)
				continue
			}
			out[w] = at
		}
		return true
	})
}

// capTarget reports whether an assignment target holds the welcome's words: caps, or a
// Capabilities field.
func capTarget(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == "caps"
	case *ast.SelectorExpr:
		return x.Sel.Name == "Capabilities"
	}
	return false
}

// resolve reads a string literal, a constant of the package, or a constant of an imported
// package of this module.
func resolve(t *testing.T, root string, f *ast.File, consts map[string]string, e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		s, err := strconv.Unquote(x.Value)
		return s, err == nil
	case *ast.Ident:
		s, ok := consts[x.Name]
		return s, ok
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		const module = "github.com/noahkiss/flightdeck/"
		for _, im := range f.Imports {
			path, _ := strconv.Unquote(im.Path.Value)
			name := filepath.Base(path)
			if im.Name != nil {
				name = im.Name.Name
			}
			if name != pkg.Name || !strings.HasPrefix(path, module) {
				continue
			}
			s, ok := constStrings(parseDir(t, token.NewFileSet(), filepath.Join(root, strings.TrimPrefix(path, module))))[x.Sel.Name]
			return s, ok
		}
	}
	return "", false
}

// parseDir parses the non-test Go files of dir, whatever their build tags.
func parseDir(t *testing.T, fset *token.FileSet, dir string) []*ast.File {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		if n := e.Name(); strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var files []*ast.File
	for _, n := range names {
		f, err := goparser.ParseFile(fset, filepath.Join(dir, n), nil, goparser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

// constStrings returns the package-level string constants of files by name.
func constStrings(files []*ast.File) map[string]string {
	out := map[string]string{}
	for _, f := range files {
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, sp := range g.Specs {
				vs := sp.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if s, err := strconv.Unquote(lit.Value); err == nil {
							out[n.Name] = s
						}
					}
				}
			}
		}
	}
	return out
}
