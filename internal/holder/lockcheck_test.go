package holder

import (
	"fmt"
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
// called or taken as a value any other way. A Lock inside a loop is refused too, because its
// deferred Unlock runs only when the function returns. That keeps every critical section panic
// safe: a panic unwinds through the deferred Unlock, so goSafe can recover without leaving the
// holder lock held.
func TestLocksUnlockByDefer(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		problems, n, err := lockProblems(name, src)
		if err != nil {
			t.Fatal(err)
		}
		checked += n
		for _, p := range problems {
			t.Error(p)
		}
	}
	if checked == 0 {
		t.Fatal("found no Lock calls; the check is not looking at the package source")
	}
}

// TestLockCheckCatches feeds the check code it must refuse, one case per rule, and code it must
// accept.
func TestLockCheckCatches(t *testing.T) {
	bad := map[string]string{
		"no defer":                 "func f() { mu.Lock(); g() }",
		"lock at the end":          "func f() { mu.Lock() }",
		"other mutex":              "func f() { mu.Lock(); defer other.Unlock() }",
		"plain unlock":             "func f() { defer mu.Lock(); mu.Unlock() }",
		"switch case":              "func f() { switch x { case 1: mu.Lock(); g() } }",
		"select case":              "func f() { select { case <-ch: mu.Lock(); g() } }",
		"unlock value":             "func f() { u := mu.Unlock; u() }",
		"unlock argument":          "func f() { run(mu.Unlock) }",
		"lock in for":              "func f() { for { mu.Lock(); defer mu.Unlock() } }",
		"lock in range":            "func f() { for range xs { if ok { mu.Lock(); defer mu.Unlock() } } }",
		"lock in a case in a loop": "func f() { for { select { case <-ch: mu.Lock(); defer mu.Unlock() } } }",
	}
	for name, body := range bad {
		problems, _, err := lockProblems(name+".go", []byte("package p\n"+body+"\n"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(problems) == 0 {
			t.Errorf("%s: not refused", name)
		}
	}
	good := []string{
		"func f() { mu.Lock(); defer mu.Unlock(); g() }",
		"func f() { switch x { case 1: mu.Lock(); defer mu.Unlock() } }",
		"func f() { select { case <-ch: mu.Lock(); defer mu.Unlock() } }",
		"func f() { for { func() { mu.Lock(); defer mu.Unlock() }() } }",
	}
	for _, body := range good {
		problems, n, err := lockProblems("good.go", []byte("package p\n"+body+"\n"))
		if err != nil || n != 1 || len(problems) != 0 {
			t.Errorf("%q: %d locks, problems %v, err %v", body, n, problems, err)
		}
	}
}

// lockProblems checks one source file and returns what is wrong, and how many Lock calls it saw.
func lockProblems(name string, src []byte) (problems []string, checked int, err error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		return nil, 0, err
	}
	report := func(pos token.Pos, format string, args ...any) {
		problems = append(problems, fset.Position(pos).String()+": "+fmt.Sprintf(format, args...))
	}
	called := map[*ast.SelectorExpr]bool{} // selectors that are the function of a call
	deferred := map[*ast.CallExpr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.DeferStmt:
			deferred[n.Call] = true
		case *ast.CallExpr:
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok {
				called[sel] = true
			}
		}
		return true
	})
	// checkList checks one statement list: a block, or the body of a case or select clause.
	checkList := func(list []ast.Stmt, inLoop bool) {
		for i, st := range list {
			recv, ok := mutexCall(st, "Lock")
			if !ok {
				continue
			}
			checked++
			if inLoop {
				report(st.Pos(), "%s.Lock() inside a loop: its deferred Unlock runs only when the function returns", recv)
			}
			if i+1 >= len(list) {
				report(st.Pos(), "%s.Lock() is not followed by defer %s.Unlock()", recv, recv)
				continue
			}
			d, ok := list[i+1].(*ast.DeferStmt)
			if !ok || exprString(src, fset, d.Call) != recv+".Unlock()" {
				report(st.Pos(), "%s.Lock() is not followed by defer %s.Unlock()", recv, recv)
			}
		}
	}
	// walk visits n, tracking whether it sits inside a loop of the same function.
	var walk func(n ast.Node, inLoop bool)
	walk = func(n ast.Node, inLoop bool) {
		ast.Inspect(n, func(m ast.Node) bool {
			if m == n {
				switch m := m.(type) {
				case *ast.BlockStmt:
					checkList(m.List, inLoop)
				case *ast.CaseClause:
					checkList(m.Body, inLoop)
				case *ast.CommClause:
					checkList(m.Body, inLoop)
				}
				return true
			}
			switch m := m.(type) {
			case *ast.FuncLit:
				walk(m.Body, false) // a new function: its defers run when it returns
				return false
			case *ast.ForStmt:
				walk(m.Body, true)
				return false
			case *ast.RangeStmt:
				walk(m.Body, true)
				return false
			case *ast.BlockStmt, *ast.CaseClause, *ast.CommClause:
				walk(m, inLoop)
				return false
			case *ast.CallExpr:
				if sel, ok := m.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Unlock" && len(m.Args) == 0 && !deferred[m] {
					report(m.Pos(), "Unlock called without defer")
				}
			case *ast.SelectorExpr:
				if m.Sel.Name == "Unlock" && !called[m] {
					report(m.Pos(), "Unlock taken as a value; call it only by defer")
				}
			}
			return true
		})
	}
	walk(f, false)
	return problems, checked, nil
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
