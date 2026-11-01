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
// tab stay until Esc or Enter. Sidebar and board are the keys while that part has focus.
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

// sticky reports whether a mode stays after a command, until Esc or Enter.
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

// SetPrefix moves the prefix to key k (ctrl-<key>): the normal-mode keys that enter prefix
// mode, and the prefix-mode key that sends the prefix to the pane.
func (km *Keymap) SetPrefix(k string) {
	for m, bs := range km.modes {
		for i, b := range bs {
			if (m == ModeNormal && b.Action == actPrefix) || (m == ModePrefix && b.Action == actSend) {
				bs[i].Key = k
			}
		}
	}
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

// DefaultKeymap is the shipped keymap: one leader, Ctrl+\, that takes one command key or opens
// the pane and tab modes (gff-client.md 15.5). It takes none of the keys 8.1 leaves to programs.
func DefaultKeymap() *Keymap {
	km := &Keymap{modes: map[Mode][]Binding{}}
	add := func(m Mode, pairs ...string) {
		for i := 0; i < len(pairs); i += 2 {
			km.modes[m] = append(km.modes[m], Binding{Key: pairs[i], Action: pairs[i+1]})
		}
	}
	add(ModeNormal, `ctrl-\`, actPrefix)
	add(ModeLocked, "ctrl-g", actUnlock)
	add(ModePrefix,
		"w", actSidebar, "g", actBoard, "p", actPaneMode, "t", actTabMode, "c", actNewTab,
		"]", actNextTab, "[", actPrevTab, "x", actCloseView, "z", actZoom, "o", actFocusNext,
		"tab", actFocusNext, "s", actSplit, "l", actLock, "b", actBars, "!", actWarnings,
		"r", actRedraw, "n", actNudge, "f", actFit, "d", actDetach, `ctrl-\`, actSend, "esc", actBack)
	for i := 1; i <= 9; i++ {
		add(ModePrefix, strconv.Itoa(i), actTabN+strconv.Itoa(i))
	}
	add(ModePane, "o", actFocusNext, "tab", actFocusNext, "z", actZoom, "s", actSplit, "x", actCloseView,
		"r", actRedraw, "n", actNudge, "f", actFit, "esc", actBack, "enter", actBack)
	add(ModeTab, "c", actNewTab, "]", actNextTab, "[", actPrevTab)
	for i := 1; i <= 9; i++ {
		add(ModeTab, strconv.Itoa(i), actTabN+strconv.Itoa(i))
	}
	add(ModeTab, "x", actCloseView, "esc", actBack, "enter", actBack)
	add(ModeSidebar, "up", actUp, "k", actUp, "down", actDown, "j", actDown, "enter", actOpen, "t", actOpenTab,
		"n", actNew, "r", actRename, "x", actClose, "/", actFilter, "p", actGroup, "esc", actBack, "q", actBack)
	add(ModeBoard, "up", actUp, "k", actUp, "down", actDown, "j", actDown, "enter", actOpen, "t", actOpenTab,
		"esc", actBack, "q", actBack)
	return km
}

// ParseKey reads a key name: a single character, ctrl-<key> (a key from @ a-z [ \ ] ^ _, not
// one that is Backspace, Tab, Enter or Esc), or tab, enter, esc, up, down, backspace, space.
// It returns the canonical name.
func ParseKey(s string) (string, error) {
	switch l := strings.ToLower(s); l {
	case "tab", "enter", "esc", "up", "down", "backspace":
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
	return "", fmt.Errorf("key %q: want one character, ctrl-<key>, tab, enter, esc, up, down, backspace or space", s)
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
// internal/hosts.
type fileSettings struct {
	Keys map[string]toml.Primitive `toml:"keys"`
	Bar  struct {
		MaxHints *int      `toml:"max_hints"`
		Clock    *string   `toml:"clock"`
		Usage    *bool     `toml:"usage"`
		Info     *[]string `toml:"info"`
	} `toml:"bar"`
}

// SettingsTables are the config file's tables LoadSettings reads, for a reader of the rest of
// the file that warns about keys it does not know.
var SettingsTables = []string{"keys", "bar"}

// LoadSettings reads the keymap and the bars from gff's config file, over the defaults.
//
//	[keys]
//	defaults = false          # start from no bindings instead of the shipped keymap
//	[keys.prefix]
//	z = "zoom"                # bind z to zoom
//	q = "detach"
//	d = "none"                # unbind d
//	[bar]
//	max_hints = 8
//	clock = "15:04"           # a Go time layout; "" hides the clock
//	usage = true
//	info = ["cpu", "mem", "disk", "battery", "uptime"]
//
// An unknown mode, key or action, or one a mode cannot take, is a problem naming the file and
// line, and the rest still loads (8.3). A missing file is the defaults and no error.
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
	if p, ok := fs.Keys["defaults"]; ok {
		var on bool
		if err := md.PrimitiveDecode(p, &on); err != nil {
			problems = append(problems, where("keys", "defaults")+": want true or false")
		} else if !on {
			s.Keymap = &Keymap{modes: map[Mode][]Binding{}}
		}
	}
	names := make([]string, 0, len(fs.Keys))
	for n := range fs.Keys {
		if n != "defaults" {
			names = append(names, n)
		}
	}
	// The modes in a fixed order, so problems come out the same way each time.
	slices.SortFunc(names, func(a, b string) int {
		return slices.Index(Modes, Mode(a)) - slices.Index(Modes, Mode(b))
	})
	for _, n := range names {
		m := Mode(n)
		table := "keys." + n
		if !slices.Contains(Modes, m) {
			problems = append(problems, fmt.Sprintf("%s: [%s]: unknown mode; modes are %s", path, table, modeList()))
			continue
		}
		var binds map[string]string
		if err := md.PrimitiveDecode(fs.Keys[n], &binds); err != nil {
			problems = append(problems, fmt.Sprintf("%s: [%s]: want key = \"action\" lines", path, table))
			continue
		}
		ks := make([]string, 0, len(binds))
		for k := range binds {
			ks = append(ks, k)
		}
		// In file order, so a later line wins over an earlier one that names the same key.
		slices.SortFunc(ks, func(x, y string) int { return keyLine(b, table, x) - keyLine(b, table, y) })
		seen := map[string]bool{}
		for _, k := range ks {
			act := binds[k]
			key, err := ParseKey(k)
			switch {
			case err != nil:
				problems = append(problems, where(table, k)+": "+err.Error())
				continue
			case seen[key]:
				problems = append(problems, where(table, k)+": the key is bound twice; this later line wins")
			}
			seen[key] = true
			switch {
			case m.chord() && !strings.HasPrefix(key, "ctrl-"):
				problems = append(problems, where(table, k)+": "+string(m)+" mode takes only ctrl-<key>; other keys go to the program")
				continue
			case act == "none":
				s.Keymap.bind(m, key, "")
				continue
			case !modeAllows(m, act):
				if _, ok := actionLabels[act]; ok {
					problems = append(problems, fmt.Sprintf("%s: action %q does not work in %s mode", where(table, k), act, m))
				} else {
					problems = append(problems, fmt.Sprintf("%s: unknown action %q", where(table, k), act))
				}
				continue
			}
			s.Keymap.bind(m, key, act)
		}
	}
	if fs.Bar.MaxHints != nil {
		s.Bar.MaxHints = max(*fs.Bar.MaxHints, 0)
	}
	if fs.Bar.Clock != nil {
		s.Bar.Clock = *fs.Bar.Clock
	}
	if fs.Bar.Usage != nil {
		s.Bar.Usage = *fs.Bar.Usage
	}
	if fs.Bar.Info != nil {
		s.Bar.Info = nil
		for _, it := range *fs.Bar.Info {
			if !slices.Contains(infoItems, it) {
				problems = append(problems, fmt.Sprintf("%s: unknown info item %q; items are %s", where("bar", "info"), it, strings.Join(infoItems, ", ")))
				continue
			}
			s.Bar.Info = append(s.Bar.Info, it)
		}
	}
	for _, k := range md.Undecoded() {
		if k[0] == "bar" {
			problems = append(problems, fmt.Sprintf("%s: unknown key %s", path, k))
		}
	}
	return s, problems, nil
}

// infoItems are the system info items (gff-client.md 9.1).
var infoItems = []string{"cpu", "mem", "disk", "battery", "uptime"}

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
			in = strings.TrimSpace(strings.Trim(l, "[]")) == table
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
