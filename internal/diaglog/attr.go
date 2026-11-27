// Package diaglog writes the structured log lines of docs/specs/diagnostics.md 4.2: one JSON
// object per line through log/slog. A line's message is a constant; every other value comes
// from a constructor of this package, and each constructor has a class (8.3) that says how a
// bundle treats the value. The legacy shim turns each line of an old log.Logger into a line
// with msg "legacy" and its text under the key text.
//
// The source check in internal/diaglog/check keeps every other Go file to these rules (10.2).
package diaglog

import (
	"time"

	"github.com/noahkiss/flightdeck/internal/trace"
)

// Class says how a bundle treats a value (diagnostics.md 8.3).
type Class uint8

const (
	// Text is free text a program or a user produced: a bundle scrubs it (pass 2 only).
	ClassText Class = iota
	// Safe is an id Flightdeck minted, a known word or a number: a bundle keeps it.
	ClassSafe
	// Name is a host, label, device, project, branch or title: a bundle replaces it with a
	// salted token.
	ClassName
	// Path is a file path: a bundle maps the home folder to ~ and each component as a name.
	ClassPath
)

func (c Class) String() string {
	switch c {
	case ClassSafe:
		return "safe"
	case ClassName:
		return "name"
	case ClassPath:
		return "path"
	}
	return "text"
}

// Attr is one key and value of a line. Only this package's constructors make one, so every
// value has a class.
type Attr struct {
	key   string
	str   string
	num   int64
	isNum bool
	class Class
}

// The fixed keys of the constructors. KeyClass maps each to its class.
const (
	KeySession = "session"
	KeyPane    = "pane"
	KeyMethod  = "method"
	KeyKind    = "kind"
	KeyTrace   = "trace"
	KeyHarness = "harness"
	KeyEvent   = "event"
	KeyClient  = "client"

	KeyHost    = "host"
	KeyLabel   = "label"
	KeyDevice  = "device"
	KeyWas     = "device_was"
	KeyProject = "project"
	KeyBranch  = "branch"
	KeyTitle   = "title"
	KeyGroup   = "group"

	KeyPath = "path"

	KeyErr  = "err"
	KeyText = "text"
)

// textSuffix ends the key of a value whose form a safe constructor refused: the value is kept,
// as text (10.2).
const textSuffix = "_text"

var keyClass = map[string]Class{
	KeySession: ClassSafe, KeyPane: ClassSafe, KeyMethod: ClassSafe, KeyKind: ClassSafe, KeyTrace: ClassSafe, KeyHarness: ClassSafe,
	KeyEvent: ClassSafe, KeyClient: ClassSafe,
	KeyHost: ClassName, KeyLabel: ClassName, KeyDevice: ClassName, KeyWas: ClassName, KeyProject: ClassName, KeyBranch: ClassName, KeyTitle: ClassName,
	KeyGroup: ClassName,
	KeyPath:  ClassPath,
	KeyErr:   ClassText, KeyText: ClassText,
}

// The keys a line always has, or that the sink adds (4.2).
const (
	KeyT          = "t"
	KeyLvl        = "lvl"
	KeyProc       = "proc"
	KeyEpoch      = "epoch"
	KeyComp       = "comp"
	KeyMsg        = "msg"
	KeySuppressed = "suppressed"
)

// KeyClass is the class of a key in a written line, for a bundle's first pass. The line's own
// keys (t, lvl, proc, epoch, comp, msg, suppressed) are safe. A key no constructor makes with a
// fixed class, such as one of Word's, is text: a string under it gets pass 2. A number or a
// Boolean is safe under any key; KeyClass does not see values.
func KeyClass(key string) Class {
	switch key {
	case KeyT, KeyLvl, KeyProc, KeyEpoch, KeyComp, KeyMsg, KeySuppressed:
		return ClassSafe
	}
	if c, ok := keyClass[key]; ok {
		return c
	}
	return ClassText
}

// maxID bounds an id a safe constructor keeps (diagnostics.md 9.2 refuses longer ones too).
const maxID = 128

// idForm reports whether s has the form of an id Flightdeck mints or a known word: printable
// ASCII letters, digits and . _ - / : @ only, at most maxID bytes. No space, so no prose.
func idForm(s string) bool {
	if s == "" || len(s) > maxID {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-', c == '/', c == ':', c == '@':
		default:
			return false
		}
	}
	return true
}

// wordForm reports whether s is a lower-case word: a method, a kind or an event name.
func wordForm(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		case c >= 'A' && c <= 'Z': // harness event names such as PostToolUse
		default:
			return false
		}
	}
	return true
}

// checked is a safe value when ok, else the value as text under key_text.
func checked(key, v string, ok bool) Attr {
	if ok {
		return Attr{key: key, str: v, class: ClassSafe}
	}
	return Attr{key: key + textSuffix, str: v, class: ClassText}
}

// Session is a session id.
func Session(id string) Attr { return checked(KeySession, id, idForm(id)) }

// Pane is a pane id.
func Pane(id string) Attr { return checked(KeyPane, id, idForm(id)) }

// Method is an API method name.
func Method(m string) Attr { return checked(KeyMethod, m, wordForm(m)) }

// Kind is an error kind or another kind word of the API.
func Kind(k string) Attr { return checked(KeyKind, k, wordForm(k)) }

// Trace is a trace id (diagnostics.md 5.1).
func Trace(t string) Attr { return checked(KeyTrace, t, trace.Valid(t)) }

// Harness is a harness name: claude, codex, opencode.
func Harness(h string) Attr { return checked(KeyHarness, h, wordForm(h)) }

// Event is an event or hook event name.
func Event(e string) Attr { return checked(KeyEvent, e, wordForm(e)) }

// ClientKind is a client kind from a hello: app, terminal, cli, script, hab.
func ClientKind(k string) Attr { return checked(KeyClient, k, wordForm(k)) }

// Word is a known word under a key of the caller's: a state, a choice, a mode. Its key has no
// fixed class, so a bundle scrubs its value; a value that is not a word is kept as text.
func Word(key, w string) Attr {
	if wordForm(w) {
		return Attr{key: key, str: w, class: ClassSafe}
	}
	return Attr{key: key + textSuffix, str: w, class: ClassText}
}

// Count is a number under a key of the caller's: a count, a pid, a size.
func Count(key string, n int64) Attr { return Attr{key: key, num: n, isNum: true, class: ClassSafe} }

// Dur is a duration, written in milliseconds under key.
func Dur(key string, d time.Duration) Attr {
	return Attr{key: key + "_ms", num: d.Milliseconds(), isNum: true, class: ClassSafe}
}

// Host is a host name or host id.
func Host(h string) Attr { return Attr{key: KeyHost, str: h, class: ClassName} }

// Label is a label a user gave: a peer's, a device's.
func Label(l string) Attr { return Attr{key: KeyLabel, str: l, class: ClassName} }

// Device is a device id.
func Device(id string) Attr { return Attr{key: KeyDevice, str: id, class: ClassName} }

// WasDevice is the device id a peer had before: the pinned one a new id replaced.
func WasDevice(id string) Attr { return Attr{key: KeyWas, str: id, class: ClassName} }

// Project is a project name.
func Project(p string) Attr { return Attr{key: KeyProject, str: p, class: ClassName} }

// Branch is a git branch name.
func Branch(b string) Attr { return Attr{key: KeyBranch, str: b, class: ClassName} }

// Title is a session or window title.
func Title(t string) Attr { return Attr{key: KeyTitle, str: t, class: ClassName} }

// Group is a group's name or id.
func Group(g string) Attr { return Attr{key: KeyGroup, str: g, class: ClassName} }

// Path is a file or folder path.
func Path(p string) Attr { return Attr{key: KeyPath, str: p, class: ClassPath} }

// Err is an error's text. A nil error writes nothing.
func Err(err error) Attr {
	if err == nil {
		return Attr{}
	}
	return Attr{key: KeyErr, str: err.Error(), class: ClassText}
}

// Text is free text a program produced: a child's stderr line, a notice's text.
func Text(s string) Attr { return Attr{key: KeyText, str: s, class: ClassText} }
