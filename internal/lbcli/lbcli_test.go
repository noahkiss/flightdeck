package lbcli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/logbook"
)

func TestParse(t *testing.T) {
	a, err := parse([]string{"new", "--tag", "a", "task", "--json", "Fix", "--tag=b", "it", "--", "--not-a-flag"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(a.pos, []string{"new", "task", "Fix", "it", "--not-a-flag"}) || !slices.Equal(a.flags["tag"], []string{"a", "b"}) || !a.has("json") {
		t.Fatalf("parse: %+v", a)
	}
	if _, err := parse([]string{"ls", "--type"}); err == nil {
		t.Fatal("a flag with no value parsed")
	}
	if _, err := parse([]string{"ls", "--json=yes"}); err == nil {
		t.Fatal("a bool flag with a value parsed")
	}
}

// cli runs Main with no hab reachable: the verbs open the file in state.
func runCLI(t *testing.T, state string, stdin string, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	env := map[string]string{"GFF_HAB_SOCKET": filepath.Join(t.TempDir(), "none.sock"), "GFF_HAB_HOST_ID": "box-a"}
	code := Main(Env{Args: append([]string{"--state", state}, args...), Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb,
		Getenv: func(k string) string { return env[k] }, Name: "gff lb"})
	return out.String(), errb.String(), code
}

func TestFileFallback(t *testing.T) {
	state := t.TempDir()
	if _, errs, code := runCLI(t, state, "", "ls"); code != 1 || !strings.Contains(errs, "start gff-hab once") {
		t.Fatalf("no file: %d %s", code, errs)
	}
	st, err := logbook.Open(context.Background(), filepath.Join(state, "logbook.db"), logbook.Options{Origin: "SHA256:hab"})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	out, errs, code := runCLI(t, state, "The body, from stdin.\n", "new", "task", "Fix", "the", "build", "--body", "-", "--tag", "ci", "--priority", "2")
	id := strings.TrimSpace(out)
	if code != 0 || !strings.HasPrefix(id, "task-") || !strings.Contains(errs, "gff-hab is not running") {
		t.Fatalf("new: %d %q %q", code, out, errs)
	}
	runCLI(t, state, "", "claim", id)
	runCLI(t, state, "", "note", id, "halfway")
	out, _, code = runCLI(t, state, "", "--json", "show", id)
	var r api.LogbookRecordResult
	if err := json.Unmarshal([]byte(out), &r); err != nil || code != 0 {
		t.Fatalf("show: %d %s %v", code, out, err)
	}
	x := r.Record
	if x.Title != "Fix the build" || x.Body != "The body, from stdin.\n" || x.Priority != 2 || !slices.Equal(x.Tags, []string{"ci"}) ||
		x.Status != "doing" || x.Assignee != "cli" || len(x.Notes) != 1 || x.CreatedBy == nil || x.CreatedBy.Kind != "person" || x.CreatedBy.Host != "box-a" {
		t.Fatalf("record: %+v %+v", x, x.CreatedBy)
	}
	out, _, _ = runCLI(t, state, "", "edit", id, "--untag", "ci", "--title", "Fix the arm64 build")
	if strings.TrimSpace(out) != id {
		t.Fatalf("edit: %q", out)
	}
	out, _, _ = runCLI(t, state, "", "find", "arm64")
	if !strings.Contains(out, id) {
		t.Fatalf("find:\n%s", out)
	}
	f, _, _ := runCLI(t, state, "", "new", "finding", "The cache is stale")
	f = strings.TrimSpace(f)
	out, _, code = runCLI(t, state, "", "supersede", f, "The cache is fine")
	if code != 0 {
		t.Fatalf("supersede: %s", out)
	}
	out, _, _ = runCLI(t, state, "", "show", f)
	if !strings.Contains(out, "superseded by: finding-") {
		t.Fatalf("superseded:\n%s", out)
	}
	if _, errs, code := runCLI(t, state, "", "edit", f, "--body", "x"); code != 3 || !strings.Contains(errs, "supersede") {
		t.Fatalf("a finding's body: %d %s", code, errs)
	}
	out, _, _ = runCLI(t, state, "", "--json", "history", id)
	var h api.LogbookHistory
	if err := json.Unmarshal([]byte(out), &h); err != nil || len(h.Ops) < 5 {
		t.Fatalf("history: %s %v", out, err)
	}
	// forget asks; no answer forgets nothing.
	if _, errs, code := runCLI(t, state, "n\n", "forget", h.Ops[0].ID); code != 1 || !strings.Contains(errs, "nothing was forgotten") {
		t.Fatalf("forget without a yes: %d %s", code, errs)
	}
	noteOp := ""
	for _, o := range h.Ops {
		if o.Kind == "append" {
			noteOp = o.ID
		}
	}
	if out, errs, code := runCLI(t, state, "", "forget", "--yes", noteOp); code != 0 {
		t.Fatalf("forget --yes: %s %s", out, errs)
	}
	if out, _, _ := runCLI(t, state, "", "show", id); strings.Contains(out, "halfway") || !strings.Contains(out, "forgotten sha256:") {
		t.Fatalf("after forget:\n%s", out)
	}
	if out, _, _ := runCLI(t, state, "", "delete", id); strings.TrimSpace(out) != id {
		t.Fatalf("delete: %q", out)
	}
	if out, _, _ := runCLI(t, state, "", "ls", "--type", "task"); !strings.Contains(out, "no records") {
		t.Fatalf("ls after delete:\n%s", out)
	}
	if out, _, _ := runCLI(t, state, "", "ls", "--type", "task", "--deleted"); !strings.Contains(out, "(deleted)") {
		t.Fatalf("ls --deleted:\n%s", out)
	}
	if out, _, _ := runCLI(t, state, "", "status"); !strings.Contains(out, "origin SHA256:hab") {
		t.Fatalf("status:\n%s", out)
	}
	if _, errs, code := runCLI(t, state, "", "brief"); code != 1 || !strings.Contains(errs, "stage 6") {
		t.Fatalf("brief: %d %s", code, errs)
	}
	if _, _, code := runCLI(t, state, "", "ls", "--nope", "x"); code != 64 {
		t.Fatalf("an unknown flag: %d", code)
	}
	if _, _, code := runCLI(t, state, "", "frobnicate"); code != 64 {
		t.Fatalf("an unknown verb: %d", code)
	}
}
