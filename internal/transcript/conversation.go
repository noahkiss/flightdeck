package transcript

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/noahkiss/flightdeck/api"
)

// Errors a page request can return.
var (
	// ErrUnknownMessage: the before id names no message of this conversation.
	ErrUnknownMessage = errors.New("no such message in this conversation")
	// ErrStaleCursor: the cursor belongs to other file contents, or lies past the end.
	ErrStaleCursor = errors.New("cursor does not match this transcript; read the tail again")
)

// item is one chat message and the byte offsets of the lines that made and last changed it.
// Offsets order changes: they are stable across gff-hab restarts, so a cursor stays valid.
type item struct {
	msg   api.ChatMessage
	first int64
	last  int64
}

// conversation holds the chat messages of one transcript in order. New messages only ever go at
// the end; a later record may change a message in place (a tool result completes its call).
type conversation struct {
	items []*item
	index map[string]int // message id to position
}

func newConversation() *conversation {
	return &conversation{index: map[string]int{}}
}

// put adds msg at the end, or replaces the message with the same id, as changed by the line
// that ends at end. It returns the stored message.
func (c *conversation) put(msg api.ChatMessage, end int64) *item {
	if i, ok := c.index[msg.ID]; ok {
		it := c.items[i]
		it.msg, it.last = msg, end
		return it
	}
	it := &item{msg: msg, first: end, last: end}
	c.index[msg.ID] = len(c.items)
	c.items = append(c.items, it)
	return it
}

// get returns the message with id.
func (c *conversation) get(id string) *item {
	if i, ok := c.index[id]; ok {
		return c.items[i]
	}
	return nil
}

func clampLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	return min(limit, 200)
}

// page returns up to limit messages before the message with id before (the tail when before is
// empty), oldest first, and whether older ones exist.
func (c *conversation) page(before string, limit int) ([]api.ChatMessage, bool, error) {
	limit = clampLimit(limit)
	stop := len(c.items)
	if before != "" {
		i, ok := c.index[before]
		if !ok {
			return nil, false, ErrUnknownMessage
		}
		stop = i
	}
	start := max(0, stop-limit)
	out := make([]api.ChatMessage, 0, stop-start)
	for _, it := range c.items[start:stop] {
		out = append(out, it.msg)
	}
	return out, start > 0, nil
}

// since returns the messages added or changed after offset from, oldest first, and the offset
// to resume from. When more than limit changed, it returns the oldest changes and more is true.
//
// The changes are taken in offset order, a whole line's changes at a time, up to an offset k.
// The page holds every message changed in (from, k] and every message made in (from, k],
// which may have changed again later and is sent with its current content. The client has
// every message made at or before from; the page adds the ones made up to k, in order, so a
// client that replaces messages by id and appends new ones keeps the conversation's order.
// A message sent early is sent again with its next change.
func (c *conversation) since(from, end int64, limit int) (msgs []api.ChatMessage, next int64, more bool) {
	limit = clampLimit(limit)
	var cands []*item
	for _, it := range c.items {
		if it.last > from {
			cands = append(cands, it)
		}
	}
	sort.SliceStable(cands, func(a, b int) bool { return cands[a].last < cands[b].last })
	// made walks the messages in position order; first offsets never decrease along it.
	made := sort.Search(len(c.items), func(i int) bool { return c.items[i].first > from })
	picked := map[*item]bool{}
	k := from
	i := 0
	for i < len(cands) {
		kk := cands[i].last
		var add []*item
		j := i
		for ; j < len(cands) && cands[j].last == kk; j++ {
			if !picked[cands[j]] {
				add = append(add, cands[j])
			}
		}
		m := made
		for ; m < len(c.items) && c.items[m].first <= kk; m++ {
			if it := c.items[m]; !picked[it] && it.last != kk {
				add = append(add, it)
			}
		}
		if len(picked) > 0 && len(picked)+len(add) > limit {
			more = true
			break
		}
		for _, it := range add {
			picked[it] = true
		}
		k, i, made = kk, j, m
	}
	if !more {
		k = max(k, end)
	}
	for _, it := range c.items { // position order
		if picked[it] {
			msgs = append(msgs, it.msg)
		}
	}
	return msgs, k, more
}

// formatCursor names offset off in the file whose contents hash to gen.
func formatCursor(gen string, off int64) string {
	if gen == "" {
		return ""
	}
	return gen + ":" + strconv.FormatInt(off, 10)
}

// parseCursor splits a cursor and checks it against the current contents.
func parseCursor(s, gen string, end int64) (int64, error) {
	g, o, ok := strings.Cut(s, ":")
	if !ok {
		return 0, fmt.Errorf("%w: malformed cursor", ErrStaleCursor)
	}
	off, err := strconv.ParseInt(o, 10, 64)
	if err != nil || off < 0 {
		return 0, fmt.Errorf("%w: malformed cursor", ErrStaleCursor)
	}
	if g != gen || off > end {
		return 0, ErrStaleCursor
	}
	return off, nil
}
