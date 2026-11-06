package lbextra

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/noahkiss/flightdeck/internal/logbook"
)

// Marker is the file at the root of a Markdown tree that says Logbook wrote it. A tree is only
// written into a folder that is empty (a .git folder aside) or holds the marker, so an export
// never removes files it did not write.
const Marker = ".logbook-export"

// General is the folder of the records with no project (3.2).
const General = "general"

// TreePath is a record's file in the tree: by project, the general folder for none. A project's
// own file is in its folder.
func TreePath(r *logbook.Record) string {
	dir := r.Project
	if r.Type == logbook.TypeProject {
		dir = r.ID
	}
	if !safeName(dir) {
		dir = General
	}
	return filepath.Join(dir, r.ID+".md")
}

// safeName reports whether s is a plain id: lower-case letters, digits and dashes. Ids and
// project ids are; anything else never becomes a path.
func safeName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// Markdown renders one record: a front matter of JSON values (each one is YAML too), the title,
// the body, the links and the notes.
func Markdown(r *logbook.Record) []byte {
	e := logbook.ExportOf(r)
	var b bytes.Buffer
	q := func(v any) string {
		out, _ := json.Marshal(v)
		return string(out)
	}
	b.WriteString("---\n")
	fmt.Fprintf(&b, "id: %s\n", q(e.ID))
	fmt.Fprintf(&b, "type: %s\n", q(e.Type))
	if e.Status != "" {
		fmt.Fprintf(&b, "status: %s\n", q(e.Status))
	}
	if e.Project != "" {
		fmt.Fprintf(&b, "project: %s\n", q(e.Project))
	}
	fmt.Fprintf(&b, "created: %s\n", q(e.Created))
	if e.Imported {
		b.WriteString("imported: true\n")
	}
	if len(e.Tags) > 0 {
		fmt.Fprintf(&b, "tags: %s\n", q(e.Tags))
	}
	if len(e.Identities) > 0 {
		fmt.Fprintf(&b, "identities: %s\n", q(e.Identities))
	}
	names := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		fmt.Fprintf(&b, "%s: %s\n", k, e.Fields[k])
	}
	b.WriteString("---\n\n")
	fmt.Fprintf(&b, "# %s\n", e.Title)
	if e.Body != "" {
		fmt.Fprintf(&b, "\n%s\n", strings.TrimRight(e.Body, "\n"))
	}
	if len(e.Links) > 0 {
		b.WriteString("\n## Links\n\n")
		for _, l := range e.Links {
			fmt.Fprintf(&b, "- %s %s\n", l.Kind, l.To)
		}
	}
	if len(e.Notes) > 0 {
		b.WriteString("\n## Notes\n")
		for _, n := range e.Notes {
			fmt.Fprintf(&b, "\n%s\n\n%s\n", n.Time, strings.TrimRight(n.Text, "\n"))
		}
	}
	return b.Bytes()
}

// TreeResult counts what WriteTree changed.
type TreeResult struct {
	Records int `json:"records"`
	Written int `json:"written"`
	Removed int `json:"removed"`
}

// ErrNotTree means the folder holds files and no Marker: WriteTree will not write there.
var ErrNotTree = errors.New("the folder holds files Logbook did not write")

// WriteTree makes dir hold one Markdown file per record that is not deleted, and nothing else
// of Logbook's: a file of a record now deleted, moved or gone is removed. A file whose content
// is the same is left alone, so git sees only real changes.
func WriteTree(ctx context.Context, st *logbook.Store, dir string) (TreeResult, error) {
	var res TreeResult
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return res, err
	}
	if err := checkTree(dir); err != nil {
		return res, err
	}
	ids, err := st.IDs(ctx)
	if err != nil {
		return res, err
	}
	want := map[string][]byte{}
	for _, id := range ids {
		r, err := st.Get(ctx, id)
		if errors.Is(err, logbook.ErrNotFound) {
			continue
		}
		if err != nil {
			return res, err
		}
		if r.Deleted || !safeName(r.ID) {
			continue
		}
		want[TreePath(r)] = Markdown(r)
	}
	res.Records = len(want)
	if err := os.WriteFile(filepath.Join(dir, Marker), []byte("Written by Flightdeck Logbook. Edits here are not read back.\n"), 0o600); err != nil {
		return res, err
	}
	var stale []string
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			if rel != "." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Dir(rel) != "." && strings.HasSuffix(rel, ".md") {
			if _, ok := want[rel]; !ok {
				stale = append(stale, p)
			}
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	for _, p := range stale {
		if err := os.Remove(p); err != nil {
			return res, err
		}
		res.Removed++
		_ = os.Remove(filepath.Dir(p)) // only when empty
	}
	paths := make([]string, 0, len(want))
	for p := range want {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, rel := range paths {
		p := filepath.Join(dir, rel)
		if old, err := os.ReadFile(p); err == nil && bytes.Equal(old, want[rel]) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return res, err
		}
		if err := writeAtomic(p, want[rel]); err != nil {
			return res, err
		}
		res.Written++
	}
	return res, nil
}

// checkTree refuses a folder with files in it and no Marker.
func checkTree(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, Marker)); err == nil {
		return nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.Name() != ".git" {
			return fmt.Errorf("%s: %w (no %s); name an empty folder", dir, ErrNotTree, Marker)
		}
	}
	return nil
}

func writeAtomic(p string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}
