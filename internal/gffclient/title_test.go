package gffclient

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/datastream"
)

func TestWindowTitleFocus(t *testing.T) {
	h := startHab(t)
	other := h.other()
	first := spawn(t, other, `read -r x; printf '\033]0;first title\007'; exec sleep 600`, 80, 24)
	second := spawn(t, other, `read -r x; printf '\033]2;second title\033\\'; exec sleep 600`, 80, 24)
	hst := newHost(t, 160, 40, false)
	startWorkspaceWith(t, hst, func(c *Config) { c.Workspace = "work" }, specsOf(h.sock)...)
	hst.waitScreen("sessions", func(s string) bool { return line(s, "sh-2") > 0 })
	hst.typ("\r")
	hst.waitScreen("first view", func(s string) bool { return strings.Contains(s, " sh · one") })
	var empty api.Empty
	call(t, other, "pane.input", api.InputParams{Pane: first, Data: []byte("\n")}, &empty)
	hst.waitScreen("first pane title", func(s string) bool { return strings.Contains(s, "first title") })
	waitUntil(t, "first window title", func() bool { return bytes.Contains(hst.output(), []byte("\x1b]2;one · work | first title\x1b\\")) })
	hst.typ(cmdKey + "wj\r")
	hst.waitScreen("second view", func(s string) bool { return strings.Contains(s, " sh-2 · one") })
	call(t, other, "pane.input", api.InputParams{Pane: second, Data: []byte("\n")}, &empty)
	hst.waitScreen("second pane title", func(s string) bool { return strings.Contains(s, "second title") })
	waitUntil(t, "second window title", func() bool { return bytes.Contains(hst.output(), []byte("\x1b]2;one · work | second title\x1b\\")) })
	hst.typ("\x1bh")
	waitUntil(t, "focus changes title", func() bool { return bytes.HasSuffix(hst.output(), []byte("\x1b]2;one · work | first title\x1b\\")) })
}

// TestTitleBeforeAttach: a title the program set before gff opened the pane's stream comes in the
// snapshot (hab-logic-api.md 5.5), so the frame and the window title show it. A slow machine
// opens the stream late, after the program set its title.
func TestTitleBeforeAttach(t *testing.T) {
	h := startHab(t)
	other := h.other()
	pane := spawn(t, other, `printf '\033]2;early title\033\\ready'; exec sleep 600`, 80, 24)
	// Another device's stream shows "ready" once the hab's parser holds the title.
	var tk api.Ticket
	call(t, other, "data.ticket", api.TicketParams{Pane: pane, Observe: true}, &tk)
	nc, err := net.Dial("unix", h.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	dc, _, err := datastream.Open(nc, api.DataHello{Ticket: tk.Ticket})
	if err != nil {
		t.Fatal(err)
	}
	var seen []byte
	for !bytes.Contains(seen, []byte("ready")) {
		nc.SetReadDeadline(time.Now().Add(waitLong()))
		m, err := dc.Next()
		if err != nil {
			t.Fatalf("the pane's stream: %v (read %q)", err, seen)
		}
		seen = append(seen, m.Data...)
	}
	dc.Close()
	hst := newHost(t, 160, 40, false)
	startWorkspaceWith(t, hst, func(c *Config) { c.Workspace = "work" }, specsOf(h.sock)...)
	hst.waitScreen("sessions", func(s string) bool { return line(s, "sh") > 0 })
	hst.typ("\r")
	hst.waitScreen("the title in the frame", func(s string) bool { return strings.Contains(s, " sh · early title · one") })
	waitUntil(t, "the window title", func() bool { return bytes.Contains(hst.output(), []byte("\x1b]2;one · work | early title\x1b\\")) })
}

func TestTitleTemplateAndSanitizing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[workspace]\ntitle = '{title} / {host}'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	settings, problems, err := LoadSettings(path)
	if err != nil || len(problems) > 0 || settings.Workspace.Title != "{title} / {host}" {
		t.Fatalf("settings: %+v %v %v", settings, problems, err)
	}
	var f passFilter
	var out bytes.Buffer
	for _, p := range []string{"\x1b]2;title", "\u009d\u202eevil\x1b\\"} {
		f.write(&out, []byte(p))
	}
	if f.title != "titleevil" || out.Len() != 0 {
		t.Fatalf("title %q, output %q", f.title, out.String())
	}
	c := &client{cfg: Config{TitleTemplate: &settings.Workspace.Title}, tabs: []*tab{{views: []*view{{spec: PaneSpec{Label: "{host}"}, h: &hostState{label: "box"}, filter: f}}}}}
	c.updateTitle()
	if !strings.Contains(c.out.String(), "\x1b]2;titleevil / box\x1b\\") {
		t.Fatal(c.out.String())
	}
	c.restoreHost()
	if !strings.HasSuffix(c.out.String(), "\x1b[23;2t") {
		t.Fatal("host title not restored")
	}
}

func TestLongPaneTitle(t *testing.T) {
	for _, title := range []string{strings.Repeat("界", 512), strings.Repeat("😀", 800), strings.Repeat("x", 5000)} {
		var f passFilter
		var out bytes.Buffer
		input := []byte("\x1b]2;" + title + "\x1b\\")
		for _, b := range input {
			f.write(&out, []byte{b})
		}
		if out.Len() != 0 || f.title != cleanTitle(title) {
			t.Fatalf("long title: output %d, title runes %d, want %d", out.Len(), len([]rune(f.title)), len([]rune(cleanTitle(title))))
		}
	}
}
