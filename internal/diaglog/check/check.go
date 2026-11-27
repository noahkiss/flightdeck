// Package check is the source check of docs/specs/diagnostics.md 10.2: it walks Go packages and
// reports each use of logging that would let a value skip its class, or log on a hot path. It
// reads the code with go/parser and go/types only, and the export data that go list builds.
package check

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// DiaglogPath is the package every log line goes through.
const DiaglogPath = "github.com/noahkiss/flightdeck/internal/diaglog"

// The rules, one per ban of 10.2.
const (
	RuleMessage = "message"  // a diaglog message, component or key that is not a constant
	RuleSlog    = "slog"     // log/slog used outside internal/diaglog
	RuleBuilt   = "built"    // fmt or strings in the argument of a safe constructor
	RuleContent = "content"  // a []byte or a content type reaching a diaglog call
	RuleHotPath = "hot-path" // a diaglog call inside a listed hot-path function
	RuleNetwork = "network"  // net, net/http or crypto/tls imported by a diagnostics package
)

// Rules lists every rule, for the fixture test.
var Rules = []string{RuleMessage, RuleSlog, RuleBuilt, RuleContent, RuleHotPath, RuleNetwork}

// Config is what the check knows of the tree.
type Config struct {
	// Dir is where go list runs: the module root.
	Dir string
	// Patterns are the packages to check, as go list takes them. Default ./...
	Patterns []string
	// HotPaths are functions no diaglog call may sit in, as "<package path>.<Func>" or
	// "<package path>.<Type>.<Method>".
	HotPaths []string
	// ContentTypes are types that hold content, as "<package path>.<Type>": none may reach a
	// diaglog call.
	ContentTypes []string
	// DiagPackages may import no network package (10.1).
	DiagPackages []string
}

const (
	mod = "github.com/noahkiss/flightdeck"
	api = mod + "/api"
)

// Default is the check of this repository.
var Default = Config{
	HotPaths: []string{
		mod + "/internal/holder.Holder.pump",
		mod + "/internal/holder.Holder.input",
		mod + "/internal/holder.Holder.inputWriter",
		mod + "/internal/holder.ring.write",
		mod + "/internal/holder.ring.read",
		mod + "/internal/holder.conn.write",
		mod + "/internal/datastream.stream.writeLoop",
		mod + "/internal/datastream.Client.write",
		mod + "/internal/link.Link.Input",
		mod + "/internal/link.pane.inputWriter",
		mod + "/internal/link.Subscription.send",
		mod + "/internal/link.seqScanner.write",
	},
	ContentTypes: []string{api + ".ChatMessage", api + ".ChatMessages", api + ".ChatTool", api + ".PaneText"},
	DiagPackages: []string{DiaglogPath, mod + "/internal/diaglog/check", mod + "/internal/logfile",
		mod + "/internal/health", mod + "/internal/tallies", mod + "/internal/trace", mod + "/internal/friction",
		mod + "/internal/stall"},
}

// Finding is one ban broken.
type Finding struct {
	Pos  token.Position
	Rule string
	Msg  string
}

func (f Finding) String() string { return fmt.Sprintf("%s: %s: %s", f.Pos, f.Rule, f.Msg) }

// safeCtors are the constructors of class safe: their arguments may not be built with fmt or
// strings.
var safeCtors = map[string]bool{"Session": true, "Pane": true, "Method": true, "Kind": true, "Trace": true,
	"Harness": true, "Event": true, "ClientKind": true, "Word": true, "Count": true, "Dur": true}

// keyed are the functions whose first argument must be a constant: a message, a component or a
// key.
var keyed = map[string]bool{"Debug": true, "Info": true, "Warn": true, "Error": true, "For": true,
	"Word": true, "Count": true, "Dur": true}

// notLogging are the diaglog functions that write no line: content may reach them, as when a
// reader parses a line.
var notLogging = map[string]bool{"Parse": true, "ParseLevel": true, "KeyClass": true, "Setup": true,
	"SetLevel": true, "SetDebug": true, "Enabled": true, "Reset": true, "Get": true}

var netImports = []string{"net", "net/http", "crypto/tls"}

type listed struct {
	ImportPath   string
	Dir          string
	GoFiles      []string
	TestGoFiles  []string
	XTestGoFiles []string
	// IgnoredGoFiles are the files this build leaves out: other systems, test seams.
	IgnoredGoFiles []string
	Imports        []string
	Export         string
	Error          *struct{ Err string }
}

// goList runs go list -json with CGO_ENABLED=0, as make check-pure builds.
func goList(dir string, args ...string) ([]listed, error) {
	cmd := exec.Command("go", append([]string{"list", "-e", "-json"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %v: %s", err, stderr.String())
	}
	var ps []listed
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listed
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		ps = append(ps, p)
	}
	return ps, nil
}

// Run checks the packages of cfg.Patterns and returns what it found, by position.
func Run(cfg Config) ([]Finding, error) {
	pats := cfg.Patterns
	if len(pats) == 0 {
		pats = []string{"./..."}
	}
	pkgs, err := goList(cfg.Dir, pats...)
	if err != nil {
		return nil, err
	}
	c := &checker{cfg: cfg, fset: token.NewFileSet()}
	var typed []listed
	for _, p := range pkgs {
		if p.Error != nil {
			return nil, fmt.Errorf("%s: %s", p.ImportPath, p.Error.Err)
		}
		if err := c.imports(p); err != nil {
			return nil, err
		}
		if slices.Contains(p.Imports, DiaglogPath) && p.ImportPath != DiaglogPath {
			typed = append(typed, p)
		}
	}
	if len(typed) > 0 {
		if err := c.typed(typed); err != nil {
			return nil, err
		}
	}
	sort.Slice(c.found, func(i, j int) bool {
		a, b := c.found[i].Pos, c.found[j].Pos
		if a.Filename != b.Filename {
			return a.Filename < b.Filename
		}
		return a.Line < b.Line
	})
	return c.found, nil
}

type checker struct {
	cfg   Config
	fset  *token.FileSet
	found []Finding
}

func (c *checker) add(pos token.Pos, rule, format string, args ...any) {
	c.found = append(c.found, Finding{Pos: c.fset.Position(pos), Rule: rule, Msg: fmt.Sprintf(format, args...)})
}

// imports applies the import bans to every file of p, test files too: log/slog outside
// diaglog, and a network package in a diagnostics package.
func (c *checker) imports(p listed) error {
	diag := slices.Contains(c.cfg.DiagPackages, p.ImportPath)
	for _, name := range slices.Concat(p.GoFiles, p.TestGoFiles, p.XTestGoFiles, p.IgnoredGoFiles) {
		f, err := parser.ParseFile(c.fset, filepath.Join(p.Dir, name), nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, im := range f.Imports {
			path, _ := strconv.Unquote(im.Path.Value)
			if path == "log/slog" && p.ImportPath != DiaglogPath {
				c.add(im.Pos(), RuleSlog, "log/slog is for internal/diaglog only: a slog value skips its class")
			}
			if diag && slices.Contains(netImports, path) && !strings.HasSuffix(name, "_test.go") {
				c.add(im.Pos(), RuleNetwork, "a diagnostics package imports %s; diagnostics open no connection", path)
			}
		}
	}
	return nil
}

// typed type-checks the packages that call diaglog, with the export data of what they import,
// and applies the call bans.
func (c *checker) typed(pkgs []listed) error {
	var paths []string
	for _, p := range pkgs {
		paths = append(paths, p.ImportPath)
	}
	deps, err := goList(c.cfg.Dir, append([]string{"-export", "-deps"}, paths...)...)
	if err != nil {
		return err
	}
	exports := map[string]string{}
	for _, d := range deps {
		if d.Export != "" {
			exports[d.ImportPath] = d.Export
		}
	}
	imp := importer.ForCompiler(c.fset, "gc", func(path string) (io.ReadCloser, error) {
		e, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(e)
	})
	for _, p := range pkgs {
		var files []*ast.File
		for _, name := range p.GoFiles {
			f, err := parser.ParseFile(c.fset, filepath.Join(p.Dir, name), nil, parser.ParseComments)
			if err != nil {
				return err
			}
			files = append(files, f)
		}
		info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Uses: map[*ast.Ident]types.Object{}}
		conf := types.Config{Importer: imp}
		if _, err := conf.Check(p.ImportPath, c.fset, files, info); err != nil {
			return fmt.Errorf("type-check %s: %w", p.ImportPath, err)
		}
		for _, f := range files {
			c.calls(p.ImportPath, f, info)
		}
	}
	return nil
}

// diaglogFunc returns the name of the diaglog function or Logger method a call names.
func diaglogFunc(call *ast.CallExpr, info *types.Info) (string, bool) {
	var id *ast.Ident
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		id = fn.Sel
	case *ast.Ident:
		id = fn
	default:
		return "", false
	}
	obj, ok := info.Uses[id].(*types.Func)
	if !ok || obj.Pkg() == nil || obj.Pkg().Path() != DiaglogPath {
		return "", false
	}
	return obj.Name(), true
}

// funcKey names a function declaration as HotPaths lists it.
func funcKey(pkg string, fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return pkg + "." + fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
			continue
		case *ast.IndexExpr:
			t = x.X
			continue
		case *ast.IndexListExpr:
			t = x.X
			continue
		}
		break
	}
	name := "?"
	if id, ok := t.(*ast.Ident); ok {
		name = id.Name
	}
	return pkg + "." + name + "." + fd.Name.Name
}

func (c *checker) calls(pkg string, f *ast.File, info *types.Info) {
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		hot := slices.Contains(c.cfg.HotPaths, funcKey(pkg, fd))
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name, ok := diaglogFunc(call, info)
			if !ok {
				return true
			}
			if hot {
				c.add(call.Pos(), RuleHotPath, "diaglog.%s in the hot-path function %s", name, fd.Name.Name)
			}
			var consts []ast.Expr
			switch {
			case keyed[name] && len(call.Args) > 0:
				consts = call.Args[:1]
			case name == "Format" && len(call.Args) > 2: // level, component, message
				consts = call.Args[:3]
			}
			for _, a := range consts {
				if tv := info.Types[a]; tv.Value == nil {
					c.add(a.Pos(), RuleMessage, "a message, component or key of diaglog.%s is not a constant", name)
				}
			}
			if safeCtors[name] {
				for _, a := range call.Args {
					c.built(a, name, info)
				}
			}
			if !notLogging[name] {
				for _, a := range call.Args {
					c.content(a, name, info)
				}
			}
			return true
		})
	}
}

// built reports a call into fmt or strings inside the argument of a safe constructor.
func (c *checker) built(arg ast.Expr, ctor string, info *types.Info) {
	ast.Inspect(arg, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if fn, ok := info.Uses[sel.Sel].(*types.Func); ok && fn.Pkg() != nil {
			if p := fn.Pkg().Path(); p == "fmt" || p == "strings" {
				c.add(call.Pos(), RuleBuilt, "%s.%s builds the value of the safe diaglog.%s", p, fn.Name(), ctor)
			}
		}
		return true
	})
}

// content reports a []byte or a content type anywhere in the argument of a diaglog call.
func (c *checker) content(arg ast.Expr, fn string, info *types.Info) {
	ast.Inspect(arg, func(n ast.Node) bool {
		e, ok := n.(ast.Expr)
		if !ok {
			return true
		}
		if call, ok := e.(*ast.CallExpr); ok {
			if _, nested := diaglogFunc(call, info); nested {
				return false // checked as a call of its own
			}
		}
		tv, ok := info.Types[e]
		if !ok || tv.Type == nil || !tv.IsValue() {
			return true
		}
		if why := c.contentType(tv.Type); why != "" {
			c.add(e.Pos(), RuleContent, "%s reaches diaglog.%s", why, fn)
			return false
		}
		return true
	})
}

func (c *checker) contentType(t types.Type) string {
	for range 4 { // through pointers, slices and arrays of a content type
		if named, ok := t.(*types.Named); ok && named.Obj().Pkg() != nil {
			full := named.Obj().Pkg().Path() + "." + named.Obj().Name()
			if slices.Contains(c.cfg.ContentTypes, full) {
				return "the content type " + full
			}
		}
		switch u := t.Underlying().(type) {
		case *types.Slice:
			if b, ok := u.Elem().Underlying().(*types.Basic); ok && b.Kind() == types.Byte {
				return "a []byte"
			}
			t = u.Elem()
		case *types.Pointer:
			t = u.Elem()
		case *types.Array:
			t = u.Elem()
		default:
			return ""
		}
	}
	return ""
}
