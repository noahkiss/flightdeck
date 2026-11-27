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
	"go/constant"
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

	"github.com/noahkiss/flightdeck/internal/diaglog"
)

// DiaglogPath is the package every log line goes through.
const DiaglogPath = "github.com/noahkiss/flightdeck/internal/diaglog"

// The rules, one per ban of 10.2.
const (
	RuleMessage = "message"  // a diaglog message, component or key that is not a constant, a reserved key, or a diaglog function used as a value
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
		mod + "/internal/stall", mod + "/internal/diagbundle"},
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
	"Harness": true, "Event": true, "ClientKind": true, "ClientID": true, "Word": true, "Count": true, "Dur": true}

// keyed are the functions whose first argument must be a constant: a message, a component or a
// key.
var keyed = map[string]bool{"Debug": true, "Info": true, "Warn": true, "Error": true, "For": true,
	"Word": true, "Count": true, "Dur": true}

// callerKeyed are the constructors whose first argument is a key of the caller's: it may not be
// a reserved key (diaglog.Reserved).
var callerKeyed = map[string]bool{"Word": true, "Count": true, "Dur": true}

// notLogging are the diaglog functions that write no line: content may reach them, as when a
// reader parses a line, and they may be used as values.
var notLogging = map[string]bool{"Parse": true, "ParseLevel": true, "KeyClass": true, "Setup": true,
	"SetLevel": true, "SetDebug": true, "Enabled": true, "Reset": true, "Reserved": true, "Get": true}

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
	Deps           []string
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
		// A package that reaches diaglog through another package can log through it too.
		if slices.Contains(p.Deps, DiaglogPath) && p.ImportPath != DiaglogPath {
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

// typed type-checks the packages that depend on diaglog, with the export data of what they import,
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
		info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Uses: map[*ast.Ident]types.Object{},
			Instances: map[*ast.Ident]types.Instance{}}
		conf := types.Config{Importer: imp}
		if _, err := conf.Check(p.ImportPath, c.fset, files, info); err != nil {
			return fmt.Errorf("type-check %s: %w", p.ImportPath, err)
		}
		for _, f := range files {
			c.calls(p.ImportPath, f, info)
		}
		c.instances(info)
	}
	return nil
}

// diaglogFunc returns the name of the diaglog function or Logger method a call names.
func diaglogFunc(call *ast.CallExpr, info *types.Info) (string, bool) {
	return diaglogName(ast.Unparen(call.Fun), info)
}

// diaglogName returns the name of the diaglog function or method e names, as an identifier or
// a selector.
func diaglogName(e ast.Expr, info *types.Info) (string, bool) {
	var id *ast.Ident
	switch fn := e.(type) {
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

// calls applies the call bans to every declaration of f: function bodies, and the values of
// package-level declarations with the function literals in them.
func (c *checker) calls(pkg string, f *ast.File, info *types.Info) {
	for _, d := range f.Decls {
		where, hot := "", false
		if fd, ok := d.(*ast.FuncDecl); ok {
			if fd.Body == nil {
				continue
			}
			where, hot = fd.Name.Name, slices.Contains(c.cfg.HotPaths, funcKey(pkg, fd))
		}
		c.walk(d, where, hot, info)
	}
}

// walk applies the call bans inside one declaration. Each use of a diaglog function or method
// must be the function of a call: a method value or a function value would let a caller pass a
// value the constant type does not check, as through a generic helper.
func (c *checker) walk(d ast.Decl, where string, hot bool, info *types.Info) {
	var stack []ast.Node
	ast.Inspect(d, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		switch n := n.(type) {
		case *ast.Ident:
			c.value(n, stack, info)
		case *ast.CallExpr:
			c.call(n, where, hot, info)
		}
		return true
	})
}

// value reports a diaglog function or method used outside call position. stack ends with id.
func (c *checker) value(id *ast.Ident, stack []ast.Node, info *types.Info) {
	name, ok := diaglogName(id, info)
	if !ok || notLogging[name] {
		return
	}
	i := len(stack) - 1
	var e ast.Expr = id
	if i > 0 {
		if sel, ok := stack[i-1].(*ast.SelectorExpr); ok && sel.Sel == id {
			e, i = sel, i-1
		}
	}
	for i > 0 {
		p, ok := stack[i-1].(*ast.ParenExpr)
		if !ok {
			break
		}
		e, i = p, i-1
	}
	if i > 0 {
		if call, ok := stack[i-1].(*ast.CallExpr); ok && call.Fun == e {
			return
		}
	}
	c.add(id.Pos(), RuleMessage, "diaglog.%s is used as a value, not called: its message or key would go unchecked", name)
}

// constantType is diaglog's unexported type of messages, components and keys, as
// types.TypeString writes it.
const constantType = DiaglogPath + ".constant"

// instances reports each generic function or type instantiated with diaglog's constant type: a
// type parameter converts any string to it, so a variable could pass as a message or a key.
func (c *checker) instances(info *types.Info) {
	for id, in := range info.Instances {
		for t := range in.TypeArgs.Types() {
			if strings.Contains(types.TypeString(t, nil), constantType) {
				c.add(id.Pos(), RuleMessage, "%s is instantiated with diaglog's message type: a type parameter converts any string to it", id.Name)
				break
			}
		}
	}
}

// call applies the bans to one call of a diaglog function or method.
func (c *checker) call(call *ast.CallExpr, where string, hot bool, info *types.Info) {
	name, ok := diaglogFunc(call, info)
	if !ok {
		return
	}
	if hot {
		c.add(call.Pos(), RuleHotPath, "diaglog.%s in the hot-path function %s", name, where)
	}
	var consts []ast.Expr
	switch {
	case keyed[name] && len(call.Args) > 0:
		consts = call.Args[:1]
	case name == "Format" && len(call.Args) > 2: // level, component, message
		consts = call.Args[:3]
	}
	for _, a := range consts {
		tv := info.Types[a]
		if tv.Value == nil {
			c.add(a.Pos(), RuleMessage, "a message, component or key of diaglog.%s is not a constant", name)
			continue
		}
		if callerKeyed[name] && tv.Value.Kind() == constant.String {
			k := constant.StringVal(tv.Value)
			if name == "Dur" {
				k += "_ms"
			}
			if diaglog.Reserved(k) {
				c.add(a.Pos(), RuleMessage, "diaglog.%s takes the reserved key %q", name, k)
			}
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

// Unresolved returns each entry of cfg.HotPaths and cfg.ContentTypes that names no declaration
// in the tree, so a rename cannot turn a ban off without notice.
func Unresolved(cfg Config) ([]string, error) {
	pkgOf := func(entry string, parts int) string { // the package path: entry less its last parts
		i := len(entry)
		for range parts {
			i = strings.LastIndex(entry[:i], ".")
			if i < 0 {
				return ""
			}
		}
		return entry[:i]
	}
	want := map[string][]string{} // package path: entries in it
	for _, h := range cfg.HotPaths {
		// "<pkg>.<Func>" or "<pkg>.<Type>.<Method>": the package path holds no dot after its
		// last slash.
		parts := strings.Count(h[strings.LastIndex(h, "/")+1:], ".")
		want[pkgOf(h, parts)] = append(want[pkgOf(h, parts)], h)
	}
	for _, ct := range cfg.ContentTypes {
		want[pkgOf(ct, 1)] = append(want[pkgOf(ct, 1)], ct)
	}
	var paths []string
	for p := range want {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	pkgs, err := goList(cfg.Dir, paths...)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	fset := token.NewFileSet()
	for _, p := range pkgs {
		if p.Error != nil {
			continue
		}
		for _, name := range slices.Concat(p.GoFiles, p.IgnoredGoFiles) {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, parser.SkipObjectResolution)
			if err != nil {
				return nil, err
			}
			for _, d := range f.Decls {
				switch d := d.(type) {
				case *ast.FuncDecl:
					have[funcKey(p.ImportPath, d)] = true
				case *ast.GenDecl:
					for _, sp := range d.Specs {
						if ts, ok := sp.(*ast.TypeSpec); ok {
							have[p.ImportPath+"."+ts.Name.Name] = true
						}
					}
				}
			}
		}
	}
	var missing []string
	for _, e := range slices.Concat(cfg.HotPaths, cfg.ContentTypes) {
		if !have[e] {
			missing = append(missing, e)
		}
	}
	return missing, nil
}
