package lbcli

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/lbsvc"
	"github.com/noahkiss/flightdeck/internal/logbook"
)

// fileProjects files projects for remotes in state's file, as gff-hab's first sight does, and
// a task in each. It returns the project ids and the task ids, in order.
func fileProjects(t *testing.T, state string, remotes ...string) (projects, tasks []string) {
	t.Helper()
	ctx := context.Background()
	st, err := logbook.Open(ctx, filepath.Join(state, "logbook.db"), logbook.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc, err := lbsvc.New(ctx, lbsvc.Config{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range remotes {
		p, err := svc.Sight(ctx, lbsvc.Sighting{Dir: "/src/" + string(rune('a'+i)), Identity: lbsvc.Identity(r, ""), Host: "box-a"})
		if err != nil {
			t.Fatal(err)
		}
		c := api.LogbookChange{Op: api.LogbookChangeOpCreate, Type: "task", Title: "work in " + r, Project: p}
		w, err := svc.Write(ctx, api.LogbookActor{Kind: "person", Device: "cli"}, api.LogbookWriteParams{Changes: []api.LogbookChange{c}})
		if err != nil {
			t.Fatal(err)
		}
		projects, tasks = append(projects, p), append(tasks, w.IDs[0])
	}
	return projects, tasks
}

func projectList(t *testing.T, state string) api.LogbookProjects {
	t.Helper()
	out, errs, code := runCLI(t, state, "", "--json", "projects")
	var r api.LogbookProjects
	if code != 0 || json.Unmarshal([]byte(out), &r) != nil {
		t.Fatalf("projects: %d %s %s", code, out, errs)
	}
	return r
}

// TestProjectVerbs: project join, rename and private through the file, and what projects shows.
func TestProjectVerbs(t *testing.T) {
	state := newState(t)
	ps, ts := fileProjects(t, state, "git@example.com:o/app.git", "git@example.com:fork/app.git", "git@example.com:me/notes.git")
	app, fork, notes := ps[0], ps[1], ps[2]

	out, errs, code := runCLI(t, state, "", "project", "join", fork, app)
	if code != 0 || out != "joined: "+fork+" and "+app+" are one project, shown as "+app+"\n" {
		t.Fatalf("join: %d %q %q", code, out, errs)
	}
	if _, errs, code := runCLI(t, state, "", "project", "join", app, fork); code != 1 || !strings.Contains(errs, "one project already") {
		t.Fatalf("join again: %d %s", code, errs)
	}
	if _, errs, code := runCLI(t, state, "", "project", "join", app, ts[0]); code != 1 || !strings.Contains(errs, "not a project") {
		t.Fatalf("join a task: %d %s", code, errs)
	}
	out, _, _ = runCLI(t, state, "", "ls", "--project", fork)
	if !strings.Contains(out, ts[0]) || !strings.Contains(out, ts[1]) || strings.Contains(out, ts[2]) {
		t.Fatalf("ls --project fork:\n%s", out)
	}
	if out, _, code := runCLI(t, state, "", "project", "rename", notes, "My", "notes"); code != 0 || strings.TrimSpace(out) != notes {
		t.Fatalf("rename: %d %q", code, out)
	}

	if out, _, code := runCLI(t, state, "", "project", "private", fork); code != 0 || !strings.Contains(out, app+" is hidden") {
		t.Fatalf("private: %d %q", code, out)
	}
	text, _, _ := runCLI(t, state, "", "projects")
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		switch {
		case strings.HasPrefix(line, app):
			if !strings.HasSuffix(line, "  private") {
				t.Errorf("app: %q", line)
			}
		case strings.HasPrefix(line, fork):
			if !strings.HasSuffix(line, "  private  joined into "+app) {
				t.Errorf("fork: %q", line)
			}
		case strings.HasPrefix(line, notes):
			if !strings.Contains(line, "My notes") || strings.Contains(line, "private") {
				t.Errorf("notes: %q", line)
			}
		}
	}
	for _, p := range projectList(t, state).Projects {
		if p.Marked != (p.ID == fork) {
			t.Fatalf("marks: %+v", p)
		}
	}
	if out, _, code := runCLI(t, state, "", "project", "private", app, "off"); code != 0 || !strings.Contains(out, "not private") {
		t.Fatalf("private off: %d %q", code, out)
	}
	for _, p := range projectList(t, state).Projects {
		if p.Private || p.Marked {
			t.Fatalf("still private: %+v", p)
		}
	}
	if out, _, _ := runCLI(t, state, "", "project", "private", app, "off"); !strings.Contains(out, "is not private") {
		t.Fatalf("off twice: %q", out)
	}
	for _, args := range [][]string{{"project"}, {"project", "split", app}, {"project", "private", app, "maybe"}, {"project", "join", app}} {
		if _, _, code := runCLI(t, state, "", args...); code != 64 {
			t.Fatalf("%v: exit %d, want 64", args, code)
		}
	}
}

// TestPaneFileHidesPrivate: with gff-hab down, a pane reads the file, and still sees no private
// project but the one of its working folder's repository.
func TestPaneFileHidesPrivate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	state := newState(t)
	ps, ts := fileProjects(t, state, "git@example.com:o/app.git", "git@example.com:me/diary.git")
	if _, _, code := runCLI(t, state, "", "project", "private", ps[1]); code != 0 {
		t.Fatal("private")
	}
	repo := func(remote string) string {
		d := t.TempDir()
		for _, args := range [][]string{{"init", "-q", d}, {"-C", d, "remote", "add", "origin", remote}} {
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v %s", args, err, out)
			}
		}
		return d
	}
	sock := filepath.Join(shortDir(t), "hook.sock") // nothing listens
	ls := func(dir string) string {
		var out, errb bytes.Buffer
		env := map[string]string{hook.Env: hook.Addr(sock, token), "GFF_HAB_STATE": state}
		if code := Main(Env{Args: []string{"ls"}, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
			Getenv: func(k string) string { return env[k] }, Name: "gff lb", Dir: dir}); code != 0 {
			t.Fatalf("ls in %s: %d %s", dir, code, errb.String())
		}
		return out.String()
	}
	if out := ls(repo("https://example.com/o/app")); !strings.Contains(out, ts[0]) || strings.Contains(out, ts[1]) {
		t.Fatalf("a pane in app reads:\n%s", out)
	}
	if out := ls(t.TempDir()); !strings.Contains(out, ts[0]) || strings.Contains(out, ts[1]) {
		t.Fatalf("a pane outside any repository reads:\n%s", out)
	}
	if out := ls(repo("https://example.com/me/diary.git")); !strings.Contains(out, ts[1]) {
		t.Fatalf("a pane in the private project does not read it:\n%s", out)
	}
	if out, _, _ := runCLI(t, state, "", "ls"); !strings.Contains(out, ts[1]) {
		t.Fatalf("a person does not read the private project:\n%s", out)
	}
}
