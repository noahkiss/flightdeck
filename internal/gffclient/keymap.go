package gffclient

// The keymap: modes, their bindings, and the hints the shortcut bar shows (gff-client.md 8).
// The bar reads its hints from the keymap gff loaded, so an unbound key leaves the bar.

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Mode is a key mode.
type Mode string

// The modes (gff-client.md 8.1). Normal passes keys to the pane and watches for its Ctrl keys.
// Locked passes every key but its own. Prefix takes one key, then returns to normal. Pane and
// tab stay through focus and tab moves, until another command, Esc or Enter. Sidebar and board
// are the keys while that part has focus.
const (
	ModeNormal  Mode = "normal"
	ModeLocked  Mode = "locked"
	ModePrefix  Mode = "prefix"
	ModePane    Mode = "pane"
	ModeTab     Mode = "tab"
	ModeSidebar Mode = "sidebar"
	ModeBoard   Mode = "board"
)

// Modes lists the modes in the order the config documents them.
var Modes = []Mode{ModeNormal, ModeLocked, ModePrefix, ModePane, ModeTab, ModeSidebar, ModeBoard}

// sticky reports whether a mode stays after a key it does not bind, and after a focus or tab
// move (stays), until Esc, Enter or another command.
func (m Mode) sticky() bool { return m == ModePane || m == ModeTab }

// command reports whether every key in the mode is a command key.
func (m Mode) command() bool { return m == ModePrefix || m.sticky() }

// chord reports whether the mode only takes Ctrl keys, passing the rest to the pane.
func (m Mode) chord() bool { return m == ModeNormal || m == ModeLocked }

// Actions. The label is the bar's word for the action.
const (
	actPrefix    = "prefix"     // enter prefix mode
	actPaneMode  = "pane-mode"  // enter pane mode
	actTabMode   = "tab-mode"   // enter tab mode
	actLock      = "lock"       // enter locked mode
	actUnlock    = "unlock"     // leave locked mode
	actBack      = "back"       // leave the mode, or the sidebar or board
	actSend      = "send"       // send the bound key itself to the pane
	actFocusNext = "focus-next" // focus the next pane
	actZoom      = "zoom"       // the focused pane alone, and back
	actSplit     = "split"      // side by side or stacked
	actRedraw    = "redraw"     // pane.redraw snapshot
	actNudge     = "nudge"      // pane.redraw nudge
	actFit       = "fit"        // pane.take
	actDetach    = "detach"     // quit gff; programs keep running
	actSidebar   = "sidebar"    // focus the sidebar, showing it
	actBoard     = "board"      // the status board
	actBars      = "bars"       // hide or show the bars
	actNewTab    = "new-tab"
	actNextTab   = "next-tab"
	actPrevTab   = "prev-tab"
	actCloseView = "close-view" // close the focused view; the program keeps running
	actWarnings  = "warnings"   // list the warnings on the bottom row
	actTabN      = "tab-"       // tab-1 to tab-9: go to that tab
	actUp        = "up"
	actDown      = "down"
	actOpen      = "open"
	actOpenTab   = "open-tab"
	actNew       = "new-session"
	actRename    = "rename"
	actClose     = "close-session"
	actFilter    = "filter"
	actGroup     = "group"
)

// actionLabels names every action on the bar. The bar groups keys by label, so tab-1 to tab-9
// show as one hint.
var actionLabels = map[string]string{
	actPrefix: "commands", actPaneMode: "pane", actTabMode: "tab", actLock: "lock", actUnlock: "unlock",
	actBack: "back", actSend: "send key", actFocusNext: "focus", actZoom: "zoom", actSplit: "split",
	actRedraw: "redraw", actNudge: "nudge", actFit: "fit", actDetach: "detach", actSidebar: "sidebar",
	actBoard: "board", actBars: "bars", actNewTab: "new tab", actNextTab: "next tab", actPrevTab: "previous tab",
	actCloseView: "close view", actWarnings: "warnings", actUp: "move", actDown: "move", actOpen: "open",
	actOpenTab: "open in new tab", actNew: "new", actRename: "rename", actClose: "close", actFilter: "filter",
	actGroup: "group",
}

func init() {
	for i := 1; i <= 9; i++ {
		actionLabels[actTabN+strconv.Itoa(i)] = "go to tab"
	}
}

// modeActions lists the actions a mode can bind. Normal binds the mode keys and any command,
// so one chord can do one thing; locked binds only unlock.
func modeAllows(m Mode, act string) bool {
	switch act {
	case actUp, actDown, actOpen, actOpenTab, actNew, actRename, actClose, actFilter, actGroup:
		return m == ModeSidebar || m == ModeBoard && (act == actUp || act == actDown || act == actOpen || act == actOpenTab)
	case actUnlock:
		return m == ModeLocked
	case actBack:
		return m != ModeNormal && m != ModeLocked
	case actSend:
		return m.command()
	}
	if _, ok := actionLabels[act]; !ok {
		return false
	}
	return m != ModeLocked && m != ModeSidebar && m != ModeBoard
}

// Binding is one key of a mode.
type Binding struct {
	Key    string // canonical key name (ParseKey)
	Action string
}

// Keymap is every mode's bindings, in the order the bar shows them: earlier is higher
// priority when the bar is short.
type Keymap struct {
	modes map[Mode][]Binding
}

// lookup returns the action key k has in mode m, or "".
func (km *Keymap) lookup(m Mode, k string) string {
	for _, b := range km.modes[m] {
		if b.Key == k {
			return b.Action
		}
	}
	return ""
}

// Bindings returns a copy of mode m's bindings.
func (km *Keymap) Bindings(m Mode) []Binding { return slices.Clone(km.modes[m]) }

// bind sets key k in mode m to act, replacing what k had; act "" unbinds it.
func (km *Keymap) bind(m Mode, k, act string) {
	bs := km.modes[m]
	i := slices.IndexFunc(bs, func(b Binding) bool { return b.Key == k })
	switch {
	case act == "" && i >= 0:
		bs = slices.Delete(bs, i, i+1)
	case act == "":
	case i >= 0:
		bs[i].Action = act
	default:
		bs = append(bs, Binding{Key: k, Action: act})
	}
	km.modes[m] = bs
}

// SetPrefix moves the prefix to key k (ctrl-<key>): the keys that enter prefix mode from
// normal, pane and tab mode, and a prefix-mode key bound to send that was one of them. The
// prefix pressed again sends itself unless prefix mode binds it (keyReader.act). Another
// binding of k in normal, pane or tab mode gives way, and SetPrefix returns a problem for each
// one in normal mode. In prefix mode, k keeps only a send binding.
func (km *Keymap) SetPrefix(k string) []string {
	var problems []string
	old := map[string]bool{}
	for _, b := range km.modes[ModeNormal] {
		if b.Action == actPrefix {
			old[b.Key] = true
		}
	}
	for m, bs := range km.modes {
		for i, b := range bs {
			if (m != ModePrefix && b.Action == actPrefix) || (m == ModePrefix && b.Action == actSend && old[b.Key]) {
				bs[i].Key = k
			}
		}
	}
	for _, m := range sharedModes {
		var kept []Binding
		seen := false
		for _, b := range km.modes[m] {
			switch {
			case b.Key != k:
			case m != ModePrefix && b.Action != actPrefix:
				if m == ModeNormal {
					problems = append(problems, fmt.Sprintf("-prefix %s replaces [keys.normal] %s = %q", k, k, b.Action))
				}
				continue
			case m == ModePrefix && b.Action != actSend:
				continue // the prefix pressed twice sends itself
			case seen:
				continue // two old prefix keys moved to one
			default:
				seen = true
			}
			kept = append(kept, b)
		}
		km.modes[m] = kept
	}
	return problems
}

// chords returns the Ctrl bytes bound in mode m, for the key reader.
func (km *Keymap) chords(m Mode) map[byte]bool {
	out := map[byte]bool{}
	for _, b := range km.modes[m] {
		if c, ok := ctrlByte(b.Key); ok {
			out[c] = true
		}
	}
	return out
}

// sharedModes are the modes [keys.shared] binds into: every mode but locked, the sidebar and
// the board, as zellij's shared_except "locked".
var sharedModes = []Mode{ModeNormal, ModePrefix, ModePane, ModeTab}

// entersMode is the mode an action enters, or "".
func entersMode(act string) Mode {
	switch act {
	case actPrefix:
		return ModePrefix
	case actPaneMode:
		return ModePane
	case actTabMode:
		return ModeTab
	case actLock:
		return ModeLocked
	}
	return ""
}

// bindShared binds key k to act in every shared mode, but not a mode-entry action in the mode
// it enters: there the key that entered the mode, pressed again, sends itself (keyReader.act).
// act "" unbinds k in every shared mode.
func (km *Keymap) bindShared(k, act string) {
	for _, m := range sharedModes {
		if act != "" && entersMode(act) == m {
			continue
		}
		km.bind(m, k, act)
	}
}

// stays reports whether an action keeps pane or tab mode on, as zellij's focus and tab moves
// do. Every other action in those modes goes back to normal after it runs.
func stays(act string) bool {
	return act == actFocusNext || act == actNextTab || act == actPrevTab
}

// DefaultKeymap is the shipped keymap: modes with Ctrl leaders, as zellij's (gff-client.md
// 8.4, 15.5). Ctrl+G locks, Ctrl+P enters pane mode, Ctrl+T tab mode and Ctrl+O the command
// mode (prefix mode: one key, then back). Each leader also works from the other command modes,
// so one leader switches modes. The leader of the current mode, pressed again, sends itself to
// the pane (keyReader.act).
func DefaultKeymap() *Keymap {
	km := &Keymap{modes: map[Mode][]Binding{}}
	add := func(m Mode, pairs ...string) {
		for i := 0; i < len(pairs); i += 2 {
			km.modes[m] = append(km.modes[m], Binding{Key: pairs[i], Action: pairs[i+1]})
		}
	}
	add(ModeLocked, "ctrl-g", actUnlock)
	add(ModePrefix, "w", actSidebar, "g", actBoard, "b", actBars, "!", actWarnings,
		"r", actRedraw, "n", actNudge, "f", actFit, "d", actDetach, "esc", actBack)
	add(ModePane, "p", actFocusNext, "tab", actFocusNext, "f", actZoom, "s", actSplit, "x", actCloseView,
		"esc", actBack, "enter", actBack)
	add(ModeTab, "n", actNewTab, "h", actPrevTab, "left", actPrevTab, "k", actPrevTab, "up", actPrevTab,
		"l", actNextTab, "right", actNextTab, "j", actNextTab, "down", actNextTab)
	for i := 1; i <= 9; i++ {
		add(ModeTab, strconv.Itoa(i), actTabN+strconv.Itoa(i))
	}
	add(ModeTab, "esc", actBack, "enter", actBack)
	// The leaders last, so the bar of a command mode lists the mode's own keys first.
	km.bindShared("ctrl-g", actLock)
	km.bindShared("ctrl-p", actPaneMode)
	km.bindShared("ctrl-t", actTabMode)
	km.bindShared("ctrl-o", actPrefix)
	add(ModeSidebar, "up", actUp, "k", actUp, "down", actDown, "j", actDown, "enter", actOpen, "t", actOpenTab,
		"n", actNew, "r", actRename, "x", actClose, "/", actFilter, "p", actGroup, "esc", actBack, "q", actBack)
	add(ModeBoard, "up", actUp, "k", actUp, "down", actDown, "j", actDown, "enter", actOpen, "t", actOpenTab,
		"esc", actBack, "q", actBack)
	return km
}

// ParseKey reads a key name: a single character, ctrl-<key> (a key from @ a-z [ \ ] ^ _, not
// one that is Backspace, Tab, Enter or Esc), or tab, enter, esc, up, down, left, right,
// backspace, space.
// It returns the canonical name.
func ParseKey(s string) (string, error) {
	switch l := strings.ToLower(s); l {
	case "tab", "enter", "esc", "up", "down", "left", "right", "backspace":
		return l, nil
	case "space":
		return " ", nil
	case "escape":
		return "esc", nil
	case "return":
		return "enter", nil
	}
	if k, ok := strings.CutPrefix(strings.ToLower(s), "ctrl-"); ok && k != "" {
		b, err := ParsePrefix(s)
		if err != nil {
			return "", err
		}
		return ctrlName(b), nil
	}
	if r := []rune(s); len(r) == 1 && r[0] > 0x20 && r[0] != 0x7f {
		return s, nil
	}
	return "", fmt.Errorf("key %q: want one character, ctrl-<key>, tab, enter, esc, up, down, left, right, backspace or space", s)
}

// ctrlName is the canonical name of a Ctrl byte: ctrl-a, ctrl-\.
func ctrlName(b byte) string {
	if b >= 1 && b <= 26 {
		return "ctrl-" + string(rune('a'+b-1))
	}
	return "ctrl-" + string(rune(b|0x40))
}

// ctrlByte is the byte of a canonical ctrl-<key> name.
func ctrlByte(k string) (byte, bool) {
	if !strings.HasPrefix(k, "ctrl-") {
		return 0, false
	}
	b, err := ParsePrefix(k)
	return b, err == nil
}

// keyLabel is how the bar writes a key.
func keyLabel(k string) string {
	switch k {
	case "up":
		return "↑"
	case "down":
		return "↓"
	case "left":
		return "←"
	case "right":
		return "→"
	case "enter":
		return "Enter"
	case "esc":
		return "Esc"
	case "tab":
		return "Tab"
	case "backspace":
		return "Bksp"
	case " ":
		return "Space"
	}
	if b, ok := ctrlByte(k); ok {
		return prefixName(b)
	}
	return k
}

// prefixName writes a Ctrl byte as Ctrl+<key>.
func prefixName(p byte) string {
	switch {
	case p == 0x1c:
		return "Ctrl+\\"
	case p >= 1 && p <= 26:
		return "Ctrl+" + string(rune('A'+p-1))
	}
	return "Ctrl+" + string(rune(p|0x40))
}

// hint is one item of the shortcut bar: the keys of one action label.
type hint struct {
	keys  string
	label string
}

func (h hint) full() string { return h.keys + " " + h.label }

// hints lists mode m's hints in binding order, keys of one label joined: "o/Tab focus",
// "1-9 go to tab". Send and back show only where they are the only way out.
func (km *Keymap) hints(m Mode) []hint {
	var out []hint
	idx := map[string]int{}
	keys := map[string][]string{}
	for _, b := range km.modes[m] {
		if b.Action == actSend {
			continue
		}
		l := actionLabels[b.Action]
		if _, ok := idx[l]; !ok {
			idx[l] = len(out)
			out = append(out, hint{label: l})
		}
		keys[l] = append(keys[l], b.Key)
	}
	for i := range out {
		out[i].keys = joinKeys(keys[out[i].label])
	}
	return out
}

// joinKeys writes keys for one hint: a run of digits as a range, the rest joined by slashes.
func joinKeys(ks []string) string {
	digits := true
	for i, k := range ks {
		if len(k) != 1 || k[0] < '0' || k[0] > '9' || (i > 0 && k[0] != ks[i-1][0]+1) {
			digits = false
		}
	}
	if digits && len(ks) > 2 {
		return ks[0] + "-" + ks[len(ks)-1]
	}
	var ls []string
	for _, k := range ks {
		ls = append(ls, keyLabel(k))
	}
	return strings.Join(ls, "/")
}

// fitHints lays hints into w columns, at most max of them (0: no cap). It keeps hints in
// priority order and drops from the end until the rest fit with their labels. When fewer than
// three would remain, it shows keys alone instead, as many as fit (gff-client.md 8.1).
func fitHints(hs []hint, w, max int) string {
	if max > 0 && len(hs) > max {
		hs = hs[:max]
	}
	join := func(hs []hint, full bool) string {
		var parts []string
		for _, h := range hs {
			if full {
				parts = append(parts, h.full())
			} else {
				parts = append(parts, h.keys)
			}
		}
		return " " + strings.Join(parts, "  ")
	}
	fits := func(s string) bool { return len([]rune(s)) <= w }
	for n := len(hs); n >= 0; n-- {
		if s := join(hs[:n], true); fits(s) && (n >= 3 || n == len(hs)) {
			return s
		}
	}
	for n := len(hs); n > 0; n-- {
		if s := join(hs[:n], false); fits(s) {
			return s
		}
	}
	return ""
}

// Settings is what gff's config file says about the keymap and the bars.
type Settings struct {
	Keymap *Keymap
	Bar    BarConfig
}

// BarConfig shapes the bars (gff-client.md 8.1, 9.1).
type BarConfig struct {
	MaxHints int    // the most hints the shortcut bar shows; 0: no cap
	Clock    string // the tab bar clock's Go time layout; "" hides it
	Usage    bool   // the usage row under the tab bar
	// Info lists the system info items in order: cpu, mem, disk, battery, uptime.
	Info []string
}

// DefaultBar is the bars with no config.
func DefaultBar() BarConfig {
	return BarConfig{Clock: "15:04", Usage: true, Info: []string{"cpu", "mem", "disk", "battery", "uptime"}}
}

// DefaultSettings is the default keymap and bars.
func DefaultSettings() Settings { return Settings{Keymap: DefaultKeymap(), Bar: DefaultBar()} }

// fileSettings is the part of gff's config file this package reads. The hosts belong to
// internal/hosts. Each value is read on its own, so one of the wrong type is a problem on its
// line and the rest still loads.
type fileSettings struct {
	Keys toml.Primitive `toml:"keys"`
	Bar  toml.Primitive `toml:"bar"`
}

// SettingsTables are the config file's tables LoadSettings reads, for a reader of the rest of
// the file that warns about keys it does not know.
var SettingsTables = []string{"keys", "bar"}

// The [keys] names that are no mode: the switch that drops the shipped bindings, and the table
// that binds one key in every shared mode.
const (
	clearDefaults = "clear-defaults"
	sharedTable   = "shared"
)

// LoadSettings reads the keymap and the bars from gff's config file, over the defaults.
//
//	[keys]
//	clear-defaults = true     # start from no bindings instead of the shipped keymap
//	[keys.shared]             # normal, prefix, pane and tab mode at once
//	"ctrl-a" = "pane-mode"    # not bound in pane mode itself: there it sends itself
//	"ctrl-p" = "none"         # unbind Ctrl+P in all four
//	[keys.pane]
//	clear-defaults = true     # start pane mode from no bindings; [keys.shared] still applies
//	z = "zoom"                # bind z to zoom
//	[keys.prefix]
//	d = "none"                # unbind d
//	[bar]
//	max_hints = 8
//	clock = "15:04"           # a Go time layout; "" hides the clock
//	usage = true
//	info = ["cpu", "mem", "disk", "battery", "uptime"]
//
// An unknown mode, key or action, one a mode cannot take, or a value of the wrong type, is a
// problem naming the file and line, and the rest still loads (8.3). A keymap that leaves a
// mode with no way back gets the default way back, and a problem (exits). A missing file is
// the defaults and no error.
func LoadSettings(path string) (Settings, []string, error) {
	s := DefaultSettings()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil, nil
	}
	if err != nil {
		return s, nil, err
	}
	var fs fileSettings
	md, err := toml.Decode(string(b), &fs)
	if err != nil {
		return s, nil, fmt.Errorf("%s: %w", path, err)
	}
	var problems []string
	where := func(table, key string) string {
		if n := keyLine(b, table, key); n > 0 {
			return fmt.Sprintf("%s:%d: [%s] %s", path, n, table, key)
		}
		return fmt.Sprintf("%s: [%s] %s", path, table, key)
	}
	// table reads one table as its values, each still to decode. An absent table is empty; a
	// value that is no table is a problem.
	table := func(p toml.Primitive, name string) map[string]toml.Primitive {
		if !md.IsDefined(strings.Split(name, ".")...) {
			return nil
		}
		var t map[string]toml.Primitive
		if err := md.PrimitiveDecode(p, &t); err != nil {
			problems = append(problems, fmt.Sprintf("%s: [%s]: want a table", path, name))
			return nil
		}
		return t
	}
	keys := table(fs.Keys, "keys")
	// cleared reads a clear-defaults value: true starts that part of the keymap from no bindings.
	cleared := func(tname string, p toml.Primitive) bool {
		var on bool
		if err := md.PrimitiveDecode(p, &on); err != nil {
			problems = append(problems, where(tname, clearDefaults)+": want true or false")
			return false
		}
		return on
	}
	if p, ok := keys[clearDefaults]; ok && cleared("keys", p) {
		s.Keymap = &Keymap{modes: map[Mode][]Binding{}}
	}
	names := make([]string, 0, len(keys))
	for n := range keys {
		if n != clearDefaults {
			names = append(names, n)
		}
	}
	// shared first, so a mode's own table wins over it; then the modes in a fixed order, so
	// problems come out the same way each time.
	rank := func(n string) int {
		if n == sharedTable {
			return -1
		}
		return slices.Index(Modes, Mode(n))
	}
	slices.SortFunc(names, func(a, b string) int {
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	// Read each table once, and clear the modes that ask before any binding applies, so
	// [keys.shared] lands in a cleared mode too.
	tables := map[string]map[string]toml.Primitive{}
	for _, n := range names {
		tname := "keys." + n
		switch {
		case md.Type("keys", n) != "Hash":
			if n == "defaults" {
				problems = append(problems, where("keys", n)+": unknown key; write clear-defaults = true to start from no bindings")
			} else {
				problems = append(problems, where("keys", n)+": unknown key; [keys] takes clear-defaults, a shared table and a table per mode")
			}
			continue
		case n != sharedTable && !slices.Contains(Modes, Mode(n)):
			problems = append(problems, fmt.Sprintf("%s: [%s]: unknown mode; modes are %s; [keys.shared] binds into %s", path, tname, modeList(), sharedList()))
			continue
		}
		t := table(keys[n], tname)
		tables[n] = t
		if p, ok := t[clearDefaults]; ok && n != sharedTable && cleared(tname, p) {
			s.Keymap.modes[Mode(n)] = nil
		}
	}
	for _, n := range names {
		binds, ok := tables[n]
		if !ok {
			continue
		}
		shared := n == sharedTable
		m, what := Mode(n), n+" mode"
		if shared {
			m, what = ModeNormal, "[keys.shared]" // the rules of the strictest shared mode
		}
		tname := "keys." + n
		ks := make([]string, 0, len(binds))
		for k := range binds {
			ks = append(ks, k)
		}
		// In file order, so a later line wins over an earlier one that names the same key.
		slices.SortFunc(ks, func(x, y string) int { return keyLine(b, tname, x) - keyLine(b, tname, y) })
		seen := map[string]bool{}
		for _, k := range ks {
			if k == clearDefaults {
				if shared {
					problems = append(problems, where(tname, k)+": unknown key; clear-defaults goes in [keys] or a mode's table")
				}
				continue
			}
			var act string
			if err := md.PrimitiveDecode(binds[k], &act); err != nil {
				problems = append(problems, where(tname, k)+`: want an action in quotes, such as "zoom"`)
				continue
			}
			key, err := ParseKey(k)
			switch {
			case err != nil:
				problems = append(problems, where(tname, k)+": "+err.Error())
				continue
			case seen[key]:
				problems = append(problems, where(tname, k)+": the key is bound twice; this later line wins")
			}
			seen[key] = true
			bind := func(act string) { s.Keymap.bind(m, key, act) }
			if shared {
				bind = func(act string) { s.Keymap.bindShared(key, act) }
			}
			switch {
			case m.chord() && !strings.HasPrefix(key, "ctrl-"):
				problems = append(problems, where(tname, k)+": "+what+" takes only ctrl-<key>; other keys go to the program")
				continue
			case act == "none":
				bind("")
				continue
			case !modeAllows(m, act):
				if _, ok := actionLabels[act]; ok {
					problems = append(problems, fmt.Sprintf("%s: action %q does not work in %s", where(tname, k), act, what))
				} else {
					problems = append(problems, fmt.Sprintf("%s: unknown action %q", where(tname, k), act))
				}
				continue
			}
			bind(act)
		}
	}
	bar := table(fs.Bar, "bar")
	barKeys := make([]string, 0, len(bar))
	for k := range bar {
		barKeys = append(barKeys, k)
	}
	slices.Sort(barKeys)
	for _, k := range barKeys {
		var err error
		want := ""
		switch k {
		case "max_hints":
			var n int
			if err = md.PrimitiveDecode(bar[k], &n); err == nil {
				s.Bar.MaxHints = max(n, 0)
			}
			want = "a number"
		case "clock":
			var c string
			if err = md.PrimitiveDecode(bar[k], &c); err == nil {
				s.Bar.Clock = c
			}
			want = `a Go time layout in quotes, or ""`
		case "usage":
			var on bool
			if err = md.PrimitiveDecode(bar[k], &on); err == nil {
				s.Bar.Usage = on
			}
			want = "true or false"
		case "info":
			var items []string
			if err = md.PrimitiveDecode(bar[k], &items); err == nil {
				s.Bar.Info = nil
				for _, it := range items {
					if !slices.Contains(infoItems, it) {
						problems = append(problems, fmt.Sprintf("%s: unknown info item %q; items are %s", where("bar", "info"), it, strings.Join(infoItems, ", ")))
						continue
					}
					s.Bar.Info = append(s.Bar.Info, it)
				}
			}
			want = "a list of items in quotes"
		default:
			problems = append(problems, where("bar", k)+": unknown key")
			continue
		}
		if err != nil {
			problems = append(problems, where("bar", k)+": want "+want)
		}
	}
	problems = append(problems, s.Keymap.exits(path)...)
	return s, problems, nil
}

// exits gives each mode the keymap reaches a way back, so no config traps the user in a mode
// (gff-client.md 8.4). Locked mode needs an unlock key; pane and tab mode need a back key, or
// Esc or Enter unbound (keyReader.act); the sidebar, which the workspace may start in, and the
// board need a back key. A mode without one gets the default (Ctrl+G unlock, Esc back) over
// what that key had, and a problem says so. With no key to detach in any reachable mode, a
// problem says gff cannot be left from the keyboard.
func (km *Keymap) exits(path string) []string {
	to := map[string]Mode{actPrefix: ModePrefix, actPaneMode: ModePane, actTabMode: ModeTab, actLock: ModeLocked,
		actSidebar: ModeSidebar, actBoard: ModeBoard}
	seen := map[Mode]bool{ModeNormal: true, ModeSidebar: true}
	queue := []Mode{ModeNormal, ModeSidebar}
	detach := false
	for len(queue) > 0 {
		m := queue[0]
		queue = queue[1:]
		for _, b := range km.modes[m] {
			if b.Action == actDetach {
				detach = true
			}
			if n, ok := to[b.Action]; ok && !seen[n] {
				seen[n] = true
				queue = append(queue, n)
			}
		}
	}
	var problems []string
	for _, m := range Modes {
		if !seen[m] || m == ModeNormal || m == ModePrefix {
			continue // prefix mode takes one key and returns
		}
		out := m.sticky() && (km.lookup(m, "esc") == "" || km.lookup(m, "enter") == "")
		for _, b := range km.modes[m] {
			if b.Action == actBack || b.Action == actUnlock {
				out = true
			}
		}
		if out {
			continue
		}
		key, act := "esc", actBack
		if m == ModeLocked {
			key, act = "ctrl-g", actUnlock
		}
		km.bind(m, key, act)
		problems = append(problems, fmt.Sprintf("%s: [keys.%s]: no key leaves %s mode; %s now does (%s)", path, m, m, keyLabel(key), act))
	}
	if !detach {
		problems = append(problems, path+": no mode binds detach; gff cannot be left from the keyboard")
	}
	return problems
}

// infoItems are the system info items (gff-client.md 9.1).
var infoItems = []string{"cpu", "mem", "disk", "battery", "uptime"}

// sharedList names the modes [keys.shared] binds into.
func sharedList() string {
	var ms []string
	for _, m := range sharedModes {
		ms = append(ms, string(m))
	}
	return strings.Join(ms, ", ")
}

func modeList() string {
	var ms []string
	for _, m := range Modes {
		ms = append(ms, string(m))
	}
	return strings.Join(ms, ", ")
}

// keyLine finds the 1-based line of key in [table] of a TOML file, or 0. It reads tables and
// keys written on lines of their own, bare or quoted, as people write them.
func keyLine(b []byte, table, key string) int {
	in := false
	for i, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "[") {
			// [table] or [[table]], perhaps with a comment or spaces after it.
			if j := strings.IndexByte(l, ']'); j >= 0 {
				l = l[:j]
			}
			in = strings.TrimSpace(strings.TrimLeft(l, "[")) == table
			continue
		}
		if !in {
			continue
		}
		k, _, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if uq, err := strconv.Unquote(k); err == nil {
			k = uq
		} else if len(k) >= 2 && k[0] == '\'' && k[len(k)-1] == '\'' {
			k = k[1 : len(k)-1]
		}
		if k == key {
			return i + 1
		}
	}
	return 0
}
