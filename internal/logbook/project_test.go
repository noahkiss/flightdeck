package logbook

import (
	"context"
	"slices"
	"testing"
)

// TestProjectReads: ProjectByIdentity answers the canonical project, PrivateProjects counts a
// project joined with a private one under the canonical id, Search's Hide leaves out a hidden
// project's records and the project records, and the folders table outlives a rebuild and is
// never synced.
func TestProjectReads(t *testing.T) {
	ctx := context.Background()
	a, b := newReplica(t, "A"), newReplica(t, "B")
	a.clock.Set(at("09:00"))
	p1, p2, p3 := NewID(TypeProject), NewID(TypeProject), NewID(TypeProject)
	t1, t3 := NewID(TypeTask), NewID(TypeTask)
	a.write(t, nil, create(p1, TypeProject, map[string]any{"title": "one"}), add(p1, "identities", "example.com/o/one"),
		create(t1, TypeTask, map[string]any{"title": "in one", "project": p1}))
	a.clock.Set(at("09:01"))
	a.write(t, nil, create(p2, TypeProject, map[string]any{"title": "two"}), add(p2, "identities", "example.com/o/two"))
	a.clock.Set(at("09:02"))
	a.write(t, nil, create(p3, TypeProject, map[string]any{"title": "three"}), add(p3, "identities", "root:abc"),
		create(t3, TypeTask, map[string]any{"title": "in three", "project": p3}))

	if id, ok, err := a.ProjectByIdentity(ctx, "example.com/o/two"); err != nil || !ok || id != p2 {
		t.Fatalf("by identity: %q %v %v", id, ok, err)
	}
	if _, ok, _ := a.ProjectByIdentity(ctx, "example.com/o/none"); ok {
		t.Fatal("an unknown identity found a project")
	}
	if priv, _ := a.PrivateProjects(ctx); len(priv) != 0 {
		t.Fatalf("private before any mark: %v", priv)
	}
	// p2 is marked private, then joined with p1 by a shared identity: p1, the older, is private.
	a.write(t, nil, set(p2, "private", true), add(p2, "identities", "example.com/o/one"))
	if id, _, _ := a.ProjectByIdentity(ctx, "example.com/o/two"); id != p1 {
		t.Fatalf("two's identity finds %s, want the canonical %s", id, p1)
	}
	if priv, _ := a.PrivateProjects(ctx); !slices.Equal(priv, []string{p1}) {
		t.Fatalf("private %v, want [%s]", priv, p1)
	}
	if got, _ := a.JoinedWith(ctx, p2); !slices.Equal(got, sorted(p1, p2)) {
		t.Fatalf("joined with %v", got)
	}
	items, _, err := a.Search(ctx, Query{Hide: []string{p1}})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	if !slices.Equal(sorted(ids...), sorted(p3, t3)) {
		t.Fatalf("hidden search %v, want only three's", ids)
	}

	if err := a.NoteFolder(ctx, "/src/one", p2, "example.com/o/two"); err != nil {
		t.Fatal(err)
	}
	if err := a.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	f, ok, err := a.FolderProject(ctx, "/src/one")
	if err != nil || !ok || f.Project != p2 || f.Identity != "example.com/o/two" {
		t.Fatalf("folder after rebuild: %+v %v %v", f, ok, err)
	}
	if priv, _ := a.PrivateProjects(ctx); !slices.Equal(priv, []string{p1}) {
		t.Fatalf("private after rebuild %v", priv)
	}
	link(t, a, b)
	if _, ok, _ := b.FolderProject(ctx, "/src/one"); ok {
		t.Fatal("the folders table synced")
	}
	if priv, _ := b.PrivateProjects(ctx); !slices.Equal(priv, []string{p1}) {
		t.Fatalf("b's private %v", priv)
	}
	// Unmarking the one marked project makes the group public again.
	a.write(t, nil, set(p2, "private", false))
	if priv, _ := a.PrivateProjects(ctx); len(priv) != 0 {
		t.Fatalf("private after unmark %v", priv)
	}
}

func sorted(s ...string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}
