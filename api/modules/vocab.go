// Package modules holds the module tier's data contracts: the block vocabulary a module's render
// returns, and the module manifest. The vocabulary is a table (Blocks); internal/apigen turns it
// into one JSON Schema per vocabulary version, the Go types in blocks_gen.go and the Swift types
// in swift/HabAPI. Spec: docs/specs/modules.md 5, 6 and 9.
package modules

// Version is the newest block vocabulary version. It is a whole number that only rises: a new
// block type raises it, a new optional field or a new word in a field does not (modules.md 9).
const Version = 1

// Tree limits (modules.md 8.1). JSON Schema cannot state them; ValidateTree checks them.
const (
	MaxBlocks    = 500
	MaxDepth     = 12
	MaxTreeBytes = 64 << 10
)

// Kind is the type of a block field.
type Kind int

const (
	KindString   Kind = iota // text of at most Max runes
	KindBool                 // true or false
	KindInt                  // a whole number from Min to Max
	KindFraction             // a number from 0 to 1
	KindWords                // one of Words; clients treat an unknown word as the first one
	KindTint                 // a meaning from Tints, or #RRGGBB
	KindSymbol               // an SF Symbol name
	KindTime                 // an RFC 3339 time
	KindAction               // an Action
	KindSteps                // {total, current}
	KindChildren             // an array of blocks, Min to Max of them; Only limits their type
	KindLines                // an array of at most Max strings of at most LineMax runes each
)

// Field is one field of a block.
type Field struct {
	Name     string
	Kind     Kind
	Required bool
	Min, Max int      // KindInt: the range. KindString: Max runes. KindChildren, KindLines: item count
	Words    []string // KindWords
	Only     string   // KindChildren: the one block type allowed
	LineMax  int      // KindLines: runes per line
	Doc      string
}

// BlockDef is one block type.
type BlockDef struct {
	Type  string
	Since int // the vocabulary version that added it
	Doc   string
	// ExactlyOne names fields of which a block sets exactly one.
	ExactlyOne []string
	Fields     []Field
}

// Text limits shared by several blocks (modules.md 6.1). The hab cuts longer text with an
// ellipsis before it sends a tree; a tree on the wire is always within them.
const (
	TitleMax    = 120
	BodyMax     = 300
	ShortMax    = 40
	TextMax     = 2000
	SymbolMax   = 64
	IDMax       = 64
	FallbackMax = 120
)

// Common fields every block may carry.
var Common = []Field{
	{Name: "id", Kind: KindString, Max: IDMax, Doc: "Stable across renders when the block has view state or an action. Unique in a tree."},
	{Name: "fallback", Kind: KindString, Max: FallbackMax, Doc: "Text a client shows when it does not know the block's type."},
	{Name: "stale_after", Kind: KindInt, Min: 1, Max: 86400, Doc: "Seconds after the tree arrives when the client greys the block."},
}

// IDPattern is the form of a block id.
const IDPattern = `^[A-Za-z0-9_.:-]+$`

// Tints are the colour meanings a tint may name (modules.md 6.3).
var Tints = []string{"accent", "muted", "working", "blocked", "done", "idle", "error", "warning"}

// TintPattern is the raw colour form of a tint.
const TintPattern = `^#[0-9A-Fa-f]{6}$`

// SymbolPattern is the form of an SF Symbol name.
const SymbolPattern = `^[a-z0-9]+(\.[a-z0-9]+)*$`

// Action names: words of lowercase letters, digits and underscores, joined by dots.
const (
	ActionPattern = `^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`
	ActionMax     = 64
	// ClientPrefix marks an action the client handles itself (modules.md 6.2).
	ClientPrefix = "client."
)

// ClientActions are the actions a client handles, with the string arguments each requires.
// A client.* name outside this list is refused.
var ClientActions = []ClientAction{
	{Name: "client.open_session", Args: []string{"id"}, Doc: "Open a session's view."},
	{Name: "client.open_pane", Args: []string{"id"}, Doc: "Open a pane's terminal view."},
	{Name: "client.copy", Args: []string{"text"}, Doc: "Copy text to the clipboard."},
	{Name: "client.open_url", Args: []string{"url"}, Doc: "Open an https URL, after asking."},
}

// ClientAction is one action a client handles.
type ClientAction struct {
	Name string
	Args []string
	Doc  string
}

// URLPattern is the form of client.open_url's url: https only.
const URLPattern = `^https://[^\s]+$`

// Blocks is vocabulary 1 (modules.md 6.1). Append new types with the next Since; never remove or
// rename one.
var Blocks = []BlockDef{
	{
		Type: "stack", Since: 1, Doc: "Children in a column or a row.",
		Fields: []Field{
			{Name: "axis", Kind: KindWords, Words: []string{"vertical", "horizontal"}, Doc: "vertical (the default) or horizontal."},
			{Name: "children", Kind: KindChildren, Required: true, Max: 100},
			{Name: "gap", Kind: KindInt, Min: 0, Max: 4, Doc: "Space between children in steps: 4 points each in the app; a blank line or column per step in the terminal."},
		},
	},
	{
		Type: "section", Since: 1, Doc: "Children under a header.",
		Fields: []Field{
			{Name: "title", Kind: KindString, Required: true, Max: TitleMax},
			{Name: "children", Kind: KindChildren, Required: true, Max: 100},
			{Name: "collapsed", Kind: KindBool, Doc: "Starts collapsed. The client keeps the person's choice by block id."},
		},
	},
	{
		Type: "row", Since: 1, Doc: "One list row: symbol, title, body and a trailing value.",
		Fields: []Field{
			{Name: "symbol", Kind: KindSymbol},
			{Name: "title", Kind: KindString, Required: true, Max: TitleMax},
			{Name: "body", Kind: KindString, Max: BodyMax},
			{Name: "trailing", Kind: KindString, Max: ShortMax, Doc: "Right-aligned."},
			{Name: "tint", Kind: KindTint},
			{Name: "action", Kind: KindAction, Doc: "Sent when the row is tapped."},
		},
	},
	{
		Type: "text", Since: 1, Doc: "A run of text.",
		Fields: []Field{
			{Name: "text", Kind: KindString, Required: true, Max: TextMax},
			{Name: "style", Kind: KindWords, Words: []string{"body", "caption", "mono", "title"}},
		},
	},
	{
		Type: "badge", Since: 1, Doc: "A short label in a capsule.",
		Fields: []Field{
			{Name: "text", Kind: KindString, Required: true, Max: ShortMax},
			{Name: "tint", Kind: KindTint},
		},
	},
	{
		Type: "metric", Since: 1, Doc: "A labelled value.",
		Fields: []Field{
			{Name: "label", Kind: KindString, Required: true, Max: ShortMax},
			{Name: "value", Kind: KindString, Required: true, Max: ShortMax, Doc: "Formatted by the module."},
			{Name: "unit", Kind: KindString, Max: 16},
			{Name: "tint", Kind: KindTint},
		},
	},
	{
		Type: "metrics", Since: 1, Doc: "A grid of up to six metrics.",
		Fields: []Field{
			{Name: "children", Kind: KindChildren, Required: true, Min: 1, Max: 6, Only: "metric"},
		},
	},
	{
		Type: "progress", Since: 1, Doc: "A bar from a fraction, or a step count.",
		ExactlyOne: []string{"value", "steps"},
		Fields: []Field{
			{Name: "value", Kind: KindFraction, Doc: "0 to 1."},
			{Name: "steps", Kind: KindSteps, Doc: "current of total; current is at most total."},
			{Name: "ends_at", Kind: KindTime, Doc: "When it ends; the client counts down without a new tree."},
		},
	},
	{
		Type: "button", Since: 1, Doc: "A button; a key hint in the terminal.",
		Fields: []Field{
			{Name: "title", Kind: KindString, Required: true, Max: ShortMax},
			{Name: "symbol", Kind: KindSymbol},
			{Name: "action", Kind: KindAction, Required: true},
			{Name: "role", Kind: KindWords, Words: []string{"normal", "destructive"}},
			{Name: "confirm", Kind: KindString, Max: TitleMax, Doc: "A question the client asks before it sends the action."},
		},
	},
	{
		Type: "toggle", Since: 1, Doc: "An on or off switch.",
		Fields: []Field{
			{Name: "title", Kind: KindString, Required: true, Max: TitleMax},
			{Name: "value", Kind: KindBool, Required: true},
			{Name: "action", Kind: KindAction, Required: true, Doc: "The client adds value, the new state, to args."},
		},
	},
	{
		Type: "field", Since: 1, Doc: "A text field; Return sends it.",
		Fields: []Field{
			{Name: "placeholder", Kind: KindString, Max: TitleMax},
			{Name: "action", Kind: KindAction, Required: true, Doc: "The client adds text, what was typed, to args."},
		},
	},
	{
		Type: "list", Since: 1, Doc: "Children as a list.",
		Fields: []Field{
			{Name: "children", Kind: KindChildren, Required: true, Max: 200},
			{Name: "empty", Kind: KindString, Max: TitleMax, Doc: "Shown when there are no children."},
		},
	},
	{
		Type: "log", Since: 1, Doc: "Lines of monospaced text.",
		Fields: []Field{
			{Name: "lines", Kind: KindLines, Required: true, Max: 500, LineMax: 1000},
			{Name: "follow", Kind: KindBool, Doc: "Keep the newest line in view."},
		},
	},
}

// BlocksAt returns the block types of vocabulary version v.
func BlocksAt(v int) []BlockDef {
	var out []BlockDef
	for _, b := range Blocks {
		if b.Since <= v {
			out = append(out, b)
		}
	}
	return out
}

// Lookup returns the block type named t in vocabulary version v.
func Lookup(t string, v int) (BlockDef, bool) {
	for _, b := range Blocks {
		if b.Type == t && b.Since <= v {
			return b, true
		}
	}
	return BlockDef{}, false
}
