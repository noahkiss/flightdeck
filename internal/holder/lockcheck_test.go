package holder

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocksUnlockByDefer is a static check on the package source (not the tests). Every Lock
// call must be followed at once by a deferred Unlock of the same mutex, and no Unlock may be
// called any other way. That keeps every critical section panic safe: a panic unwinds through
// the deferred Unlock, so goSafe can recover without leaving the holder lock held.
func TestLocksUnlockByDefer(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		deferred := map[*ast.CallExpr]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if d, ok := n.(*ast.DeferStmt); ok {
				deferred[d.Call] = true
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BlockStmt:
				for i, st := range n.List {
					recv, ok := mutexCall(st, "Lock")
					if !ok {
						continue
					}
					checked++
					if i+1 >= len(n.List) {
						t.Errorf("%s: %s.Lock() is not followed by defer %s.Unlock()", fset.Position(st.Pos()), recv, recv)
						continue
					}
					d, ok := n.List[i+1].(*ast.DeferStmt)
					if !ok || exprString(src, fset, d.Call) != recv+".Unlock()" {
						t.Errorf("%s: %s.Lock() is not followed by defer %s.Unlock()", fset.Position(st.Pos()), recv, recv)
					}
				}
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Unlock" && len(n.Args) == 0 && !deferred[n] {
					t.Errorf("%s: Unlock called without defer", fset.Position(n.Pos()))
				}
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("found no Lock calls; the check is not looking at the package source")
	}
}

// mutexCall reports whether st is a bare call statement x.<method>(), and returns x as source.
func mutexCall(st ast.Stmt, method string) (string, bool) {
	es, ok := st.(*ast.ExprStmt)
	if !ok {
		return "", false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != method {
		return "", false
	}
	return chain(sel.X), true
}

// chain renders a selector chain such as c.h.mu.
func chain(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return chain(e.X) + "." + e.Sel.Name
	}
	return "?"
}

func exprString(src []byte, fset *token.FileSet, e ast.Expr) string {
	return string(src[fset.Position(e.Pos()).Offset:fset.Position(e.End()).Offset])
}
