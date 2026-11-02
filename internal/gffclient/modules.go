package gffclient

// Modules on the status board (modules.md 4, 6; gff-client.md 7.6): each module placed on the
// board draws as a card under the Rounds list, through internal/modview. Its buttons take keys,
// its targets take the board's selection, and an action goes back to the module's hab.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/noahkiss/flightdeck/api/modules"
	"github.com/noahkiss/flightdeck/internal/modview"
	"github.com/noahkiss/flightdeck/internal/vt"
)

// ModulesCapability is the welcome capability of a hab that serves module trees.
const ModulesCapability = "modules"

// ModuleCard is the latest tree of one module, drawn for the board, on one host.
type ModuleCard struct {
	Host   string // the host's label
	Module string // the module's id
	Name   string // the manifest's name
	// Tree is the last good tree, as JSON; nil before the first.
	Tree []byte
	// Err is the last failure, shown under the last good tree (modules.md 8.3), or the
	// reason the module shows no tree ("Needs a newer Flightdeck").
	Err string
	At  time.Time // when Tree arrived, for stale_after
}

// ModuleSource gives the board its module cards and takes their actions. The hab will serve
// module trees over its API (modules.md 4); until it does, Config.Modules is nil and the board
// shows no module section. Tests drive a module host in-process through it.
type ModuleSource interface {
	// Cards returns the cards for a place ("board"), in the order they show.
	Cards(place string) []ModuleCard
	// Act sends a module action (never a client.* one) to the module on host. It returns when
	// the module has handled it; the new tree arrives through Changed.
	Act(ctx context.Context, host, module string, a modules.Action) error
	// Changed signals that Cards has something new.
	Changed() <-chan struct{}
}

// modStop is one module target the board's selection can rest on.
type modStop struct {
	host, module, ref string
}

func (s modStop) card() string { return s.host + "/" + s.module }

// boardCards returns the board's cards whose host's hab serves modules: online now, or offline
// with its last tree, dimmed.
func (u *workspaceUI) boardCards() []ModuleCard {
	src := u.c.cfg.Modules
	if src == nil || u.c.cfg.Hosts == nil {
		return nil
	}
	caps := map[string]bool{}
	for _, h := range u.c.cfg.Hosts.Hosts() {
		if hasCap(h.Welcome, ModulesCapability) {
			caps[h.Label] = true
		}
	}
	var out []ModuleCard
	for _, c := range src.Cards("board") {
		if caps[c.Host] {
			out = append(out, c)
		}
	}
	return out
}

// hostOnline reports whether the host with label is online.
func (u *workspaceUI) hostOnline(label string) bool {
	for _, h := range u.c.cfg.Hosts.Hosts() {
		if h.Label == label {
			return h.Online()
		}
	}
	return false
}

// modReserved are the keys a card's key hints must not take: every single key the board binds.
func (u *workspaceUI) modReserved() string {
	var s []rune
	for _, b := range u.c.cfg.Keymap.Bindings(ModeBoard) {
		if r := []rune(b.Key); len(r) == 1 {
			s = append(s, r[0])
		}
	}
	return string(s)
}

// modView draws a card's tree in w columns, with the board's selection and the folds. Only
// the card that holds the selection draws its key hints, as only its keys act; the others show
// "›" for every button.
func (u *workspaceUI) modView(c ModuleCard, w int) *modview.View {
	focus := ""
	selected := u.mode == modeBoard && u.onMod && u.selMod.card() == c.Host+"/"+c.Module
	if selected {
		focus = u.selMod.ref
	}
	folded := map[string]bool{}
	for k, v := range u.modFold {
		if k.card() == c.Host+"/"+c.Module {
			folded[k.ref] = v
		}
	}
	now := u.c.cfg.Now()
	return modview.Render(c.Tree, modview.Options{Width: w, Now: now, Age: now.Sub(c.At), Focus: focus,
		Folded: folded, Reserved: u.modReserved(), NoHints: !selected})
}

// modStops lists every target of every card, in board order.
func (u *workspaceUI) modStops() []modStop {
	var out []modStop
	for _, c := range u.boardCards() {
		if c.Tree == nil {
			continue
		}
		for _, t := range u.modView(c, 80).Targets {
			out = append(out, modStop{c.Host, c.Module, t.Ref})
		}
	}
	return out
}

// selectedTarget is the target under the board's selection, with its card.
func (u *workspaceUI) selectedTarget() (ModuleCard, modview.Target, bool) {
	if u.mode != modeBoard || !u.onMod {
		return ModuleCard{}, modview.Target{}, false
	}
	for _, c := range u.boardCards() {
		if c.Host+"/"+c.Module != u.selMod.card() || c.Tree == nil {
			continue
		}
		v := u.modView(c, 80)
		if i := v.TargetByRef(u.selMod.ref); i >= 0 {
			return c, v.Targets[i], true
		}
	}
	return ModuleCard{}, modview.Target{}, false
}

// modChangeGrace is how long after the selected target changed under the owner one Enter is
// ignored: the owner may have meant the target that was there before.
const modChangeGrace = time.Second

// modSeen is the selected target as last drawn, and when it last changed without the owner
// moving the selection.
type modSeen struct {
	stop    modStop
	sig     string
	changed time.Time
}

// targetSig is what the owner sees of a target and what it would send: when it changes under
// the selection, Enter would press something else. A toggle's value and a section's fold are
// left out, as pressing them changes them.
func targetSig(t modview.Target) string {
	a, _ := json.Marshal(t.Action)
	return strconv.Itoa(int(t.Kind)) + "\x00" + t.Title + "\x00" + t.Confirm + "\x00" + string(t.Key) + "\x00" + string(a)
}

// seeTarget records the selected target as drawn. A change the owner did not make by moving
// the selection (the tree changed, or the target went and the selection moved on) starts the
// grace in which one Enter is ignored.
func (u *workspaceUI) seeTarget(t modview.Target) {
	sig := targetSig(t)
	if u.modSeen.stop == u.selMod && u.modSeen.sig == sig {
		return
	}
	var changed time.Time
	if !u.modMoved && u.modSeen.sig != "" {
		changed = u.c.cfg.Now()
	}
	u.modSeen, u.modMoved = modSeen{stop: u.selMod, sig: sig, changed: changed}, false
}

// modKey handles a key on the board while the selection is in a card: a key hint presses its
// button, Enter or Space the selected target. It reports whether it used the key.
func (u *workspaceUI) modKey(k key, act string) bool {
	c, t, ok := u.selectedTarget()
	if !ok {
		return false
	}
	switch {
	case act == actOpenTab, act == actRename, act == actClose:
		// A target opens nothing in a tab and has no name or pane; the key must not fall to
		// the session row the board hides under the selection.
		return true
	case act == actOpen || k.name == "enter" || k.r == ' ' && act == "":
		u.seeTarget(t)
		if !u.modSeen.changed.IsZero() && u.c.cfg.Now().Sub(u.modSeen.changed) < modChangeGrace {
			u.modSeen.changed = time.Time{}
			u.msg = "the selection changed; press again"
			return true
		}
		u.press(c, t)
		return true
	case act == "" && k.r != 0:
		v := u.modView(c, 80)
		if i := v.TargetByKey(k.r); i >= 0 {
			u.selMod.ref, u.modMoved = v.Targets[i].Ref, true
			u.press(c, v.Targets[i])
			return true
		}
	}
	return false
}

// press acts on a target: folds a section, asks for a field's text or a button's confirm, then
// sends the action.
func (u *workspaceUI) press(c ModuleCard, t modview.Target) {
	if t.Kind == modview.KindSection {
		if u.modFold == nil {
			u.modFold = map[modStop]bool{}
		}
		u.modFold[modStop{c.Host, c.Module, t.Ref}] = !t.Collapsed
		return
	}
	send := func(text string) {
		a, ok := t.Send(text)
		if ok {
			u.sendAction(c, a)
		}
	}
	switch {
	case t.Kind == modview.KindField:
		label := t.Title
		if label == "" {
			label = "text"
		}
		u.ask(&prompt{label: label + ": ", done: func(text, _ string) { send(text) }})
	case t.Confirm != "":
		u.ask(&prompt{label: t.Confirm + " (y/n) ", yesNo: true, done: func(string, string) { send("") }})
	default:
		send("")
	}
}

// sendAction runs a client action here, or sends a module action to its hab off the loop.
func (u *workspaceUI) sendAction(c ModuleCard, a modules.Action) {
	if modview.IsClient(a) {
		u.clientAction(c, a)
		return
	}
	if !u.hostOnline(c.Host) {
		u.msg = cleanMsg(c.Host) + " is offline"
		return
	}
	src, cl := u.c.cfg.Modules, u.c
	go func() {
		ctx, cancel := context.WithTimeout(cl.ctx, callTimeout)
		defer cancel()
		err := src.Act(ctx, c.Host, c.Module, a)
		if err != nil {
			cl.run(func() { u.msg = cleanMsg(c.Name) + ": " + cleanMsg(err.Error()) })
		}
	}()
}

// clientAction handles the four client actions (modules.md 6.2).
func (u *workspaceUI) clientAction(c ModuleCard, a modules.Action) {
	arg := func(k string) string { s, _ := a.Args[k].(string); return s }
	switch a.Name {
	case "client.open_session", "client.open_pane":
		id := arg("id")
		for _, r := range u.rows {
			if r.Host == c.Host && (a.Name == "client.open_session" && r.Session == id || a.Name == "client.open_pane" && r.Pane == id) {
				u.open(r, false)
				return
			}
		}
		u.msg = "no " + map[bool]string{true: "session", false: "pane"}[a.Name == "client.open_session"] + " " + cleanMsg(id) + " on " + cleanMsg(c.Host)
	case "client.copy":
		// OSC 52: the host terminal copies, also over ssh. The text goes as base64, so no byte of
		// it reaches the terminal as a control. The note shows how long it is and how it starts,
		// so the owner sees what a button put on the clipboard.
		text := arg("text")
		switch {
		case text == "":
			// An empty OSC 52 payload clears the clipboard on some terminals.
			u.msg = "nothing to copy"
		case len(text) > copyMax:
			u.msg = "not copied: " + strconv.Itoa(len(text)) + " bytes; the limit is " + strconv.Itoa(copyMax)
		default:
			u.c.out.WriteString("\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07")
			u.msg = "copied " + strconv.Itoa(len([]rune(text))) + " chars: " + cut(cleanMsg(text), copyPreview)
		}
	case "client.open_url":
		// gff cannot know where a browser runs; it shows the link, which the host terminal can
		// open. Nothing opens without the owner, so no question is asked. Only an https link
		// shows, its host first, so a long path cannot push the host off the bar.
		l, err := url.Parse(arg("url"))
		if err != nil || l.Scheme != "https" || l.Host == "" || l.User != nil {
			u.msg = "not an https link"
			return
		}
		u.msg = cut(cleanMsg(l.Host)+" · "+cleanMsg(l.String()), urlShown)
	default:
		u.msg = "unknown action " + cleanMsg(a.Name)
	}
}

// cleanMsg keeps module and hab text off the terminal's control path.
func cleanMsg(s string) string { return modview.Clean(s) }

const (
	// copyMax is the most bytes client.copy sends: base64 makes it about 100 KB of OSC 52,
	// which common terminals take.
	copyMax = 74 * 1024
	// copyPreview and urlShown are the most columns of the copied text and of the link the
	// note shows.
	copyPreview = 24
	urlShown    = 120
)

// cut shortens s to n runes, ending with "…" when it was longer.
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// fixModSel keeps a selection in the cards on a target that still shows: the first target of
// the same card, else the first of any card, else back to the rows.
func (u *workspaceUI) fixModSel(stops []modStop) {
	if !u.onMod {
		return
	}
	if slices.Contains(stops, u.selMod) {
		return
	}
	for _, s := range stops {
		if s.card() == u.selMod.card() {
			u.selMod = s
			return
		}
	}
	if len(stops) > 0 {
		u.selMod = stops[0]
		return
	}
	u.onMod = false
}

// drawCards adds the Modules section's lines to the board.
func (u *workspaceUI) drawCards(w int, line func(string, vt.Style), spans func([]modview.Span, bool)) {
	cards := u.boardCards()
	if len(cards) == 0 {
		return
	}
	line(" Modules", boldStyle)
	for _, c := range cards {
		online := u.hostOnline(c.Host)
		head := "  " + cleanMsg(c.Name) + " · " + cleanMsg(c.Host)
		st := boldStyle
		if !online {
			head += " · offline"
			st = dimStyle
		}
		line(head, st)
		inner := max(1, w-4)
		if c.Tree != nil {
			v := u.modView(c, inner)
			if i := v.TargetByRef(u.selMod.ref); i >= 0 && u.onMod && u.mode == modeBoard && u.selMod.card() == c.Host+"/"+c.Module {
				u.seeTarget(v.Targets[i])
			}
			for _, l := range v.Lines {
				ss := append([]modview.Span{{Text: "    "}}, l.Spans...)
				if !online {
					for i := range ss {
						ss[i].Style.Dim = true
					}
				}
				sel := false
				for _, s := range l.Spans {
					sel = sel || s.Style.Focus
				}
				spans(ss, sel)
			}
		}
		if c.Err != "" {
			l := modview.ErrorLine(c.Err, inner)
			if c.Err == modview.NeedsNewer {
				l = modview.Line{Spans: []modview.Span{{Text: c.Err, Style: modview.Style{Dim: true}}}}
			}
			spans(append([]modview.Span{{Text: "    "}}, l.Spans...), false)
		}
	}
}

// modStyle maps a span's style to the host terminal's palette (modules.md 6.3): tints are ANSI
// colours, so the host's theme decides how they look.
func modStyle(s modview.Style) vt.Style {
	st := tintStyle(s.Tint)
	st.Bold = st.Bold || s.Bold
	st.Faint = st.Faint || s.Dim
	st.Inverse = s.Focus
	return st
}

func tintStyle(t string) vt.Style {
	switch t {
	case "":
		return vt.Style{}
	case "accent":
		return vt.Style{Fg: palette(4)}
	case "muted":
		return vt.Style{Faint: true}
	case "working", "warning":
		return vt.Style{Fg: palette(3)}
	case "blocked":
		return vt.Style{Bold: true, Fg: palette(1)}
	case "error":
		return vt.Style{Fg: palette(1)}
	case "done":
		return vt.Style{Fg: palette(2)}
	case "idle":
		return vt.Style{}
	}
	if len(t) == 7 && t[0] == '#' {
		return vt.Style{Fg: palette(nearestANSI(t))}
	}
	return vt.Style{}
}

// nearestANSI is the nearest of the eight basic ANSI colours to a #RRGGBB colour: each channel
// is on or off at half strength.
func nearestANSI(hex string) uint8 {
	var i uint8
	for c, bit := range []uint8{1, 2, 4} {
		v := 0
		for _, ch := range hex[1+2*c : 3+2*c] {
			v *= 16
			switch {
			case ch >= '0' && ch <= '9':
				v += int(ch - '0')
			case ch >= 'a' && ch <= 'f':
				v += int(ch-'a') + 10
			case ch >= 'A' && ch <= 'F':
				v += int(ch-'A') + 10
			}
		}
		if v >= 0x80 {
			i |= bit
		}
	}
	return i
}
