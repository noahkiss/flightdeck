// Package aliases reads the harness alias table of a hab's config file (harness-adapters.md,
// Spawn): short words an agent or the owner may name instead of a harness, such as gpt for
// Codex with a named model. The owner edits the table; the hab reads it again when the file
// changes, so a change needs no rebuild and no restart.
//
//	[aliases]
//	gpt = { harness = "codex", model = "gpt-5.5" }
//	opus = { harness = "claude", model = "opus" }
package aliases

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

// Alias is what one alias stands for.
type Alias struct {
	Harness string // a harness word, such as codex
	Model   string // the model to start it with; "" for the harness's own default
}

// Table maps a lowercase alias to what it stands for.
type Table map[string]Alias

// Names returns the table's aliases in order.
func (t Table) Names() []string {
	out := make([]string, 0, len(t))
	for n := range t {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Lookup finds word in the table, ignoring case and surrounding space.
func (t Table) Lookup(word string) (Alias, string, bool) {
	k := strings.ToLower(strings.TrimSpace(word))
	a, ok := t[k]
	return a, k, ok
}

// validName is an alias: lowercase letters, digits, '.', '_' and '-', at most 32.
var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// Parse reads the [aliases] table from a config file's bytes. harnesses are the words an alias
// may stand for. An entry that is not valid is left out, and problems says why; the rest
// apply. err is set only when the file does not parse.
func Parse(b []byte, harnesses []string) (Table, []string, error) {
	var f struct {
		Aliases map[string]toml.Primitive `toml:"aliases"`
	}
	md, err := toml.Decode(string(b), &f)
	if err != nil {
		return nil, nil, err
	}
	out := Table{}
	var problems []string
	for _, name := range sortedKeys(f.Aliases) {
		var e struct {
			Harness string `toml:"harness"`
			Model   string `toml:"model"`
		}
		bad := func(format string, a ...any) {
			problems = append(problems, fmt.Sprintf("[aliases] %s: ", name)+fmt.Sprintf(format, a...))
		}
		if err := md.PrimitiveDecode(f.Aliases[name], &e); err != nil {
			bad("want { harness = \"...\", model = \"...\" }: %v", err)
			continue
		}
		if keys := undecodedUnder(md, name); len(keys) > 0 {
			bad("unknown key %s; an alias has harness and model", strings.Join(keys, ", "))
			continue
		}
		lower := strings.ToLower(name)
		switch {
		case !validName.MatchString(lower):
			bad("an alias is lowercase letters, digits, '.', '_' and '-', at most 32")
		case !slices.Contains(harnesses, e.Harness):
			bad("harness %q is not one of %s", e.Harness, strings.Join(harnesses, ", "))
		case strings.HasPrefix(e.Model, "-") || strings.ContainsAny(e.Model, " \t\r\n"):
			bad("model %q is not a model name", e.Model)
		case out[lower] != (Alias{}):
			bad("named twice, in another case")
		default:
			out[lower] = Alias{Harness: e.Harness, Model: e.Model}
		}
	}
	return out, problems, nil
}

func sortedKeys(m map[string]toml.Primitive) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// undecodedUnder lists the keys inside [aliases].name that Parse did not read.
func undecodedUnder(md toml.MetaData, name string) []string {
	var out []string
	for _, k := range md.Undecoded() {
		if len(k) == 3 && k[0] == "aliases" && k[1] == name {
			out = append(out, k[2])
		}
	}
	return out
}

// File is the alias table of one config file. Get reads the file again when its size or
// modification time changed, so a call costs a stat. A file that does not parse keeps the last
// good table; Logf hears each problem once per change of the file.
type File struct {
	Path      string
	Harnesses []string
	Logf      func(string, ...any)

	mu    sync.Mutex
	stamp [2]int64
	read  bool
	table Table
}

// Get returns the current table. A missing file, or no path, is an empty table.
func (f *File) Get() Table {
	if f == nil || f.Path == "" {
		return Table{}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fi, err := os.Stat(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		f.read, f.table = true, Table{}
		f.stamp = [2]int64{}
		return f.table
	}
	if err != nil {
		f.logf("aliases: %v", err)
		return f.current()
	}
	stamp := [2]int64{fi.Size(), fi.ModTime().UnixNano()}
	if f.read && stamp == f.stamp {
		return f.current()
	}
	b, err := os.ReadFile(f.Path)
	if err != nil {
		f.logf("aliases: %v", err)
		return f.current()
	}
	f.stamp, f.read = stamp, true
	t, problems, err := Parse(b, f.Harnesses)
	if err != nil {
		f.logf("aliases: %s: %v; the last good table stays", f.Path, err)
		return f.current()
	}
	for _, p := range problems {
		f.logf("aliases: %s: %s; left out", f.Path, p)
	}
	f.table = t
	return t
}

func (f *File) current() Table {
	if f.table == nil {
		return Table{}
	}
	return f.table
}

func (f *File) logf(format string, a ...any) {
	if f.Logf != nil {
		f.Logf(format, a...)
	}
}
