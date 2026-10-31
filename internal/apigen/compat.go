package apigen

import (
	"fmt"
	"slices"
	"sort"
)

// direction says who produces a value. A client sends input; the server sends output.
// An input may widen and an output may narrow; anything else breaks an old peer.
type direction int

const (
	input direction = iota
	output
)

func (d direction) String() string {
	if d == input {
		return "input"
	}
	return "output"
}

// CheckReleased checks the current schema against the released ones (spec 3.3). The current
// revision must equal the highest released revision, and then be compatible with it, or be one
// higher with a translator for every step down to MinRevision.
func CheckReleased(cur *Document, released []*Document, hasTranslator func(revision int) bool) []string {
	if len(released) == 0 {
		return []string{"no released schema to compare against"}
	}
	latest := released[0]
	for _, d := range released[1:] {
		if d.Revision > latest.Revision {
			latest = d
		}
	}
	var errs []string
	switch {
	case cur.Revision == latest.Revision:
		for _, b := range Compare(latest, cur) {
			errs = append(errs, fmt.Sprintf("breaking change against released revision %d: %s", latest.Revision, b))
		}
	case cur.Revision == latest.Revision+1:
		// A new revision may break; its translator carries old clients.
	default:
		errs = append(errs, fmt.Sprintf("revision %d must equal the released revision %d or be one higher", cur.Revision, latest.Revision))
	}
	for r := cur.MinRevision + 1; r <= cur.Revision; r++ {
		if !hasTranslator(r) {
			errs = append(errs, fmt.Sprintf("no translator from revision %d down to %d", r, r-1))
		}
	}
	return errs
}

// Compare lists every change from old to cur that an old peer cannot skip (spec 3.3).
// An empty list means cur is compatible with old.
func Compare(old, cur *Document) []string {
	c := &comparer{old: old, cur: cur, seen: map[string]bool{}}
	c.top()
	sort.Strings(c.breaks)
	return c.breaks
}

type comparer struct {
	old, cur *Document
	seen     map[string]bool
	breaks   []string
}

func (c *comparer) fail(path, format string, args ...any) {
	c.breaks = append(c.breaks, path+": "+fmt.Sprintf(format, args...))
}

func (c *comparer) top() {
	if c.old.ErrorCode != c.cur.ErrorCode {
		c.fail("x-error-code", "changed from %d to %d", c.old.ErrorCode, c.cur.ErrorCode)
	}
	c.schema("x-error-data", c.old.ErrorData, c.cur.ErrorData, output)

	for _, om := range c.old.Methods {
		i := slices.IndexFunc(c.cur.Methods, func(m MethodDoc) bool { return m.Name == om.Name })
		path := "method " + om.Name
		if i < 0 {
			c.fail(path, "removed")
			continue
		}
		nm := c.cur.Methods[i]
		c.schema(path+" params", om.ParamsType, nm.ParamsType, input)
		c.schema(path+" result", om.Result.Schema, nm.Result.Schema, output)
	}
	for _, on := range c.old.Notifications {
		i := slices.IndexFunc(c.cur.Notifications, func(n NotificationDoc) bool { return n.Name == on.Name })
		path := "notification " + on.Name
		if i < 0 {
			c.fail(path, "removed")
			continue
		}
		c.schema(path, on.Params, c.cur.Notifications[i].Params, output)
	}
	for _, oe := range c.old.Events {
		i := slices.IndexFunc(c.cur.Events, func(e EventDoc) bool { return e.Type == oe.Type })
		path := "event " + oe.Type
		if i < 0 {
			c.fail(path, "removed")
			continue
		}
		c.schema(path, oe.Data, c.cur.Events[i].Data, output)
	}
	for _, of := range c.old.Frames {
		i := slices.IndexFunc(c.cur.Frames, func(f FrameDoc) bool { return f.Name == of.Name })
		path := "frame " + of.Name
		if i < 0 {
			c.fail(path, "removed")
			continue
		}
		nf := c.cur.Frames[i]
		if nf.Kind != of.Kind {
			c.fail(path, "kind byte changed from %d to %d", of.Kind, nf.Kind)
		}
		if nf.Direction != of.Direction {
			c.fail(path, "direction changed from %s to %s", of.Direction, nf.Direction)
		}
		if nf.Layout != of.Layout {
			c.fail(path, "binary layout changed")
		}
		if (of.Payload == nil) != (nf.Payload == nil) {
			c.fail(path, "payload changed between JSON and binary")
			continue
		}
		if of.Payload != nil {
			dir := output
			if of.Direction == "client" {
				dir = input
			}
			c.schema(path, of.Payload, nf.Payload, dir)
		}
	}
}

func (c *comparer) schema(path string, o, n *Schema, dir direction) {
	if o == nil || n == nil {
		if o != n {
			c.fail(path, "schema added or removed")
		}
		return
	}
	o, oname := c.old.Resolve(o)
	n, nname := c.cur.Resolve(n)
	if o == nil || n == nil {
		c.fail(path, "dangling $ref %q / %q", oname, nname)
		return
	}
	if oname != "" && nname != "" {
		key := oname + "|" + nname + "|" + dir.String()
		if c.seen[key] {
			return
		}
		c.seen[key] = true
		path = path + " (" + nname + ")"
	}

	if kind(o) != kind(n) {
		c.fail(path, "type changed from %s to %s", kind(o), kind(n))
		return
	}
	switch {
	case o.Type == "array":
		c.schema(path+"[]", o.Items, n.Items, dir)
	case o.Type == "object":
		c.object(path, o, n, dir)
	case o.KnownValues != nil || n.KnownValues != nil:
		c.words(path, o.KnownValues, n.KnownValues, dir)
	}
}

func (c *comparer) object(path string, o, n *Schema, dir direction) {
	for _, name := range sortedKeys(o.Properties) {
		fpath := path + "." + name
		np, ok := n.Properties[name]
		if !ok {
			c.fail(fpath, "field removed")
			continue
		}
		oreq, nreq := contains(o.Required, name), contains(n.Required, name)
		if dir == input && !oreq && nreq {
			c.fail(fpath, "optional input became required")
		}
		if dir == output && oreq && !nreq {
			c.fail(fpath, "required output became optional")
		}
		c.schema(fpath, o.Properties[name], np, dir)
	}
	if dir == input {
		for _, name := range n.Required {
			if _, ok := o.Properties[name]; !ok {
				c.fail(path+"."+name, "new required input")
			}
		}
	}
}

// words compares a word set. A set the client sends is closed, so the server must keep every
// old word and must not turn a plain string into a word set. A set the server sends is open:
// clients map unknown words to a fallback, so any change is compatible.
func (c *comparer) words(path string, o, n []string, dir direction) {
	if dir == output {
		return
	}
	if o == nil && n != nil {
		c.fail(path, "input string became a closed word set")
		return
	}
	for _, w := range o {
		if !contains(n, w) {
			c.fail(path, "input word %q removed", w)
		}
	}
}

func kind(s *Schema) string {
	if s.Type == "" {
		return "any"
	}
	k := s.Type
	if s.Format != "" {
		k += "/" + s.Format
	}
	if s.ContentEncoding != "" {
		k += "/" + s.ContentEncoding
	}
	return k
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func contains(list []string, s string) bool { return slices.Contains(list, s) }
