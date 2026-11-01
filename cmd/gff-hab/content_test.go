package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// TestShowEndToEnd: with the real holder and gff-hab binaries, `gff-hab show` run inside a pane
// names its session by the pane's GFF_HAB alone, and from outside with -session; content.list
// lists both files, content.read returns their bytes, and an id the session never referenced
// answers not_found. A show of a directory fails with exit 3.
func TestShowEndToEnd(t *testing.T) {
	d := shortDir(t)
	hdir, state, work := filepath.Join(d, "h"), filepath.Join(d, "state"), filepath.Join(d, "w")
	for _, dir := range []string{hdir, work} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	png := filepath.Join(work, "shot.png")
	md := filepath.Join(work, "notes.md")
	os.WriteFile(png, []byte("\x89PNG invented"), 0o600)
	os.WriteFile(md, []byte("# Notes\n\nInvented.\n"), 0o600)
	sock := filepath.Join(d, "run", "hab.sock")
	startHolder(t, hdir, "one")
	startServeWith(t, sock, state, nil, "-holders", hdir)
	if w := welcome(t, sock); !slices.Contains(w.Capabilities, "content") {
		t.Fatalf("capabilities %v lack content", w.Capabilities)
	}
	script := `"$HAB" show -title "The bug" shot.png > out.tmp 2> err.txt; mv out.tmp out.json; while [ ! -e end ]; do sleep 0.05; done`
	created := createSession(t, sock, api.SessionCreateParams{Name: "shower", Argv: []string{"/bin/sh", "-c", script},
		Env: []string{"PATH=/usr/bin:/bin", "HAB=" + bin, noRaceExitSleep}, Cwd: work})
	t.Cleanup(func() { os.WriteFile(filepath.Join(work, "end"), nil, 0o600) })
	var shown api.ContentShown
	waitUntil(t, "the pane's show", func() bool {
		b, err := os.ReadFile(filepath.Join(work, "out.json"))
		if err != nil {
			return false
		}
		if err := json.Unmarshal(b, &shown); err != nil {
			e, _ := os.ReadFile(filepath.Join(work, "err.txt"))
			t.Fatalf("show in the pane: %q %q", b, e)
		}
		return true
	})
	if shown.Session != created.Session || shown.ID == "" {
		t.Fatalf("show from the pane: %+v, session %s", shown, created.Session)
	}
	var mdShown api.ContentShown
	if err := json.Unmarshal([]byte(run(t, "show", "-socket", sock, "-session", created.Session, md)), &mdShown); err != nil {
		t.Fatal(err)
	}
	var list api.ContentList
	if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "content.list", `{"session":"`+created.Session+`"}`)), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 || list.Items[0].ID != shown.ID || list.Items[0].Kind != api.ContentKindImage || list.Items[0].Title != "The bug" ||
		list.Items[1].Kind != api.ContentKindMarkdown || !list.Items[1].Shown {
		t.Fatalf("list %+v", list.Items)
	}
	var data api.ContentData
	if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "content.read", `{"session":"`+created.Session+`","id":"`+mdShown.ID+`"}`)), &data); err != nil {
		t.Fatal(err)
	}
	if string(data.Data) != "# Notes\n\nInvented.\n" || !data.EOF || data.Type != "text/markdown" {
		t.Fatalf("read %+v", data)
	}
	cmd := exec.Command(bin, "api", "call", "-socket", sock, "content.read", `{"session":"`+created.Session+`","id":"cdeadbeefdeadbeef"}`)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil || !strings.Contains(stderr.String(), "not_found") {
		t.Errorf("read of an unreferenced id: %v %q", err, stderr.String())
	}
	cmd = exec.Command(bin, "show", "-socket", sock, "-session", created.Session, work)
	if err := cmd.Run(); err == nil {
		t.Error("show of a directory worked")
	} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 {
		t.Errorf("show of a directory: %v", err)
	}
}
