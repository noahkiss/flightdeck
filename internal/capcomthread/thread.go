// Package capcomthread is the Capcom thread the hab keeps (docs/specs/capcom-service.md 7.1,
// 7.2): one conversation the user sees, made of items, kept on the home host as append-only
// month files of JSON lines in <state>/capcom/thread/. A change to an item (read, a card's
// answer, a tool call added to a turn's line) is a new line that names the item; reading folds
// them. Pure Go.
package capcomthread

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Item kinds (7.2).
const (
	KindMessage = "message"
	KindTools   = "tools"
	KindAgent   = "agent"
	KindRound   = "round"
	KindCard    = "card"
	KindNote    = "note"
	KindDivider = "divider"
)

// Author kinds (7.1).
const (
	AuthorUser       = "user"
	AuthorCapcom     = "capcom"
	AuthorAgent      = "agent"
	AuthorFlightdeck = "flightdeck"
)

// PageMax is the most items one page holds (19.3: a fixed ceiling on one API reply).
const PageMax = 200

// MaxLine bounds one stored line, so a reader never takes an unbounded line.
const MaxLine = 4 << 20

// Author says who wrote an item.
type Author struct {
	Kind    string `json:"kind"`
	Device  string `json:"device,omitempty"`  // a user's device id
	Address string `json:"address,omitempty"` // an agent's channel address
}

// Context is one attached-context block that went with a user message (capcom.md 8.4).
type Context struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

// ToolCall is one call in a turn's folded tools line.
type ToolCall struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
}

// Item is one entry of the thread, folded.
type Item struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	Time    time.Time `json:"time"`
	Author  Author    `json:"author"`
	Text    string    `json:"text,omitempty"`
	Images  []string  `json:"images,omitempty"`
	Context []Context `json:"context,omitempty"`
	TZ      string    `json:"tz,omitempty"` // the sending device's time zone (6.5)
	// Key is a user message's idempotency key: a repeat with the same key is the same item.
	Key string `json:"key,omitempty"`
	// Turn and Part place a Capcom reply or a tools line in its turn (4.3: a repeated reply is
	// dropped by turn and part).
	Turn   string     `json:"turn,omitempty"`
	Part   int        `json:"part,omitempty"`
	Event  bool       `json:"event,omitempty"` // written in an event turn ("wrote first", 7.2)
	Tools  []ToolCall `json:"tools,omitempty"`
	Card   *Card      `json:"card,omitempty"`
	Answer *Answer    `json:"answer,omitempty"`
	// Summary is a divider's summary: the text the fresh SDK session opened with (6.2).
	Summary string `json:"summary,omitempty"`
	// Model marks a note the model gets as input (a card answered, a confirmation outcome; 4.2).
	// User messages, agent reports and round items always go to the model.
	Model bool `json:"model,omitempty"`
	// Read is when the engine took the message (input.taken, 7.3). Zero: not read yet.
	Read time.Time `json:"read,omitzero"`
}

// Card is a card item's content: what kind of card, and the choices it offers.
type Card struct {
	Type    string   `json:"type"` // approval, confirmation, handoff, calendar, reminder, proposal, memory
	Ref     string   `json:"ref,omitempty"`
	Choices []string `json:"choices,omitempty"`
	// A memory card (6.4) names a capcom guide: Rev is the record's revision when it was noted,
	// so an Undo after an edit elsewhere says so; Proposal marks a guide that waits for review,
	// whose Undo rejects the proposal Ref names.
	Rev      int  `json:"rev,omitempty"`
	Proposal bool `json:"proposal,omitempty"`
}

// CardMemory is the card type of a "Noted" line (6.4), and ChoiceUndo its one choice.
const (
	CardMemory = "memory"
	ChoiceUndo = "undo"
)

// Answer is a card's answer: the first wins.
type Answer struct {
	Choice string    `json:"choice"`
	By     string    `json:"by,omitempty"`
	Time   time.Time `json:"time"`
}

// ForModel reports whether the item goes to the model as an input (4.2).
func (it Item) ForModel() bool {
	switch it.Kind {
	case KindMessage:
		return it.Author.Kind == AuthorUser
	case KindAgent, KindRound:
		return true
	case KindNote:
		return it.Model
	}
	return false
}

// line is one stored line: an item added, or a change to one.
type line struct {
	Op     string    `json:"op"` // add, read, tool, answer
	Item   *Item     `json:"item,omitempty"`
	ID     string    `json:"id,omitempty"`
	Time   time.Time `json:"time,omitzero"`
	Tool   *ToolCall `json:"tool,omitempty"`
	Answer *Answer   `json:"answer,omitempty"`
}

// ErrConflict means a card already has an answer.
var ErrConflict = errors.New("capcomthread: the card has an answer already")

// ErrNotFound means no item has the id.
var ErrNotFound = errors.New("capcomthread: no such item")

// Store is the thread of one home host.
type Store struct {
	dir string
	now func() time.Time

	mu     sync.Mutex
	items  []*Item // in id order
	byID   map[string]*Item
	byKey  map[string]*Item
	reply  map[string]*Item // turn + "/" + part
	tools  map[string]*Item // turn -> its tools line
	lastID uint64
	watch  []func(Item)
}

// Problem is a month file that could not be read and was set aside (14: "The thread file is
// damaged").
type Problem struct {
	File, SetAside string
	Err            error
}

func (p Problem) String() string {
	return fmt.Sprintf("capcom thread: %s is damaged (%v); set aside as %s, a new file starts", filepath.Base(p.File), p.Err, filepath.Base(p.SetAside))
}

// Open loads the thread in dir, making dir (0700) when it is missing. A month file with a line
// that does not parse is set aside as <file>.bad-<unix seconds> and reported; the other months
// still load. A last line cut short by a crash (no newline) is dropped from the file.
func Open(dir string, now func() time.Time) (*Store, []Problem, error) {
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	s := &Store{dir: dir, now: now, byID: map[string]*Item{}, byKey: map[string]*Item{}, reply: map[string]*Item{}, tools: map[string]*Item{}}
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(files)
	var problems []Problem
	var lines []line
	for _, f := range files {
		ls, err := readFile(f)
		if err != nil {
			bad := fmt.Sprintf("%s.bad-%d", f, now().Unix())
			for i := 1; ; i++ {
				if _, serr := os.Lstat(bad); errors.Is(serr, fs.ErrNotExist) {
					break
				}
				bad = fmt.Sprintf("%s.bad-%d-%d", f, now().Unix(), i)
			}
			if rerr := os.Rename(f, bad); rerr != nil {
				return nil, nil, fmt.Errorf("capcom thread: set aside %s: %w", f, rerr)
			}
			problems = append(problems, Problem{File: f, SetAside: bad, Err: err})
			continue
		}
		lines = append(lines, ls...)
	}
	// Adds first, in id order; then the changes, in file order. A change may sit in a later
	// month than its item.
	for _, l := range lines {
		if l.Op == "add" && l.Item != nil {
			s.insert(l.Item)
		}
	}
	sort.Slice(s.items, func(i, j int) bool { return s.items[i].ID < s.items[j].ID })
	for _, l := range lines {
		if l.Op != "add" {
			s.apply(l)
		}
	}
	return s, problems, nil
}

// readFile reads one month file. A last line with no newline is cut off the file.
func readFile(path string) ([]line, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if i := bytes.LastIndexByte(b, '\n'); i != len(b)-1 {
		b = b[:i+1]
		if err := os.Truncate(path, int64(len(b))); err != nil {
			return nil, err
		}
	}
	var out []line
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), MaxLine+1)
	n := 0
	for sc.Scan() {
		n++
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var l line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return nil, fmt.Errorf("line %d: %v", n, err)
		}
		if l.Op == "add" && (l.Item == nil || !validID(l.Item.ID)) {
			return nil, fmt.Errorf("line %d: an item with no valid id", n)
		}
		out = append(out, l)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// An id is "c" and 16 hex digits of a nanosecond count that rises with each item.
func validID(id string) bool {
	if len(id) != 17 || id[0] != 'c' {
		return false
	}
	_, err := strconv.ParseUint(id[1:], 16, 64)
	return err == nil
}

func (s *Store) insert(it *Item) {
	if _, dup := s.byID[it.ID]; dup {
		return
	}
	s.items = append(s.items, it)
	s.byID[it.ID] = it
	if it.Key != "" {
		s.byKey[it.Key] = it
	}
	if it.Turn != "" && it.Kind == KindMessage && it.Author.Kind == AuthorCapcom {
		s.reply[it.Turn+"/"+strconv.Itoa(it.Part)] = it
	}
	if it.Turn != "" && it.Kind == KindTools {
		s.tools[it.Turn] = it
	}
	n, _ := strconv.ParseUint(it.ID[1:], 16, 64)
	s.lastID = max(s.lastID, n)
}

func (s *Store) apply(l line) *Item {
	it := s.byID[l.ID]
	if it == nil {
		return nil
	}
	switch l.Op {
	case "read":
		if it.Read.IsZero() {
			it.Read = l.Time
		}
	case "tool":
		if l.Tool != nil {
			it.Tools = append(it.Tools, *l.Tool)
		}
	case "answer":
		if it.Answer == nil && l.Answer != nil {
			a := *l.Answer
			it.Answer = &a
		}
	}
	return it
}

// Watch calls fn with each new or changed item, after it is stored. fn runs with no lock held,
// on the writer's goroutine.
func (s *Store) Watch(fn func(Item)) {
	s.mu.Lock()
	s.watch = append(s.watch, fn)
	s.mu.Unlock()
}

// write appends one line to the month file of t, synced before it returns. The caller holds
// s.mu.
func (s *Store) write(t time.Time, l line) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	if len(b) > MaxLine {
		return fmt.Errorf("capcom thread: an item of %d bytes is over the %d byte limit", len(b), MaxLine)
	}
	path := filepath.Join(s.dir, t.UTC().Format("2006-01")+".jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (s *Store) notify(it Item) {
	s.mu.Lock()
	ws := append([]func(Item){}, s.watch...)
	s.mu.Unlock()
	for _, w := range ws {
		w(it)
	}
}

// Add stores a new item and returns it with its id and time set. An item whose Key is held, or
// a Capcom reply whose turn and part are held, is not stored again: Add returns the stored one
// and added false.
func (s *Store) Add(it Item) (Item, bool, error) {
	s.mu.Lock()
	if it.Key != "" {
		if old := s.byKey[it.Key]; old != nil {
			s.mu.Unlock()
			return clone(old), false, nil
		}
	}
	if it.Turn != "" && it.Kind == KindMessage && it.Author.Kind == AuthorCapcom {
		if old := s.reply[it.Turn+"/"+strconv.Itoa(it.Part)]; old != nil {
			s.mu.Unlock()
			return clone(old), false, nil
		}
	}
	now := s.now()
	if it.Time.IsZero() {
		it.Time = now.UTC()
	}
	n := max(uint64(now.UnixNano()), s.lastID+1)
	it.ID = fmt.Sprintf("c%016x", n)
	cp := clone(&it)
	if err := s.write(now, line{Op: "add", Item: &cp}); err != nil {
		s.mu.Unlock()
		return Item{}, false, err
	}
	stored := cp
	s.insert(&stored)
	out := clone(&stored)
	s.mu.Unlock()
	s.notify(out)
	return out, true, nil
}

// MarkRead marks a message read at t. A message already read keeps its first time; changed is
// then false.
func (s *Store) MarkRead(id string, t time.Time) (Item, bool, error) {
	return s.change(line{Op: "read", ID: id, Time: t.UTC()}, func(it *Item) bool { return it.Read.IsZero() }, nil)
}

// AddTool adds a call to the tools line of turn, making the line when the turn has none.
func (s *Store) AddTool(turn string, call ToolCall, event bool) (Item, error) {
	s.mu.Lock()
	it := s.tools[turn]
	s.mu.Unlock()
	if it == nil {
		created, added, err := s.Add(Item{Kind: KindTools, Author: Author{Kind: AuthorCapcom}, Turn: turn, Event: event, Tools: []ToolCall{call}})
		if err != nil || added {
			return created, err
		}
		it = &created // a concurrent add made it
	}
	out, _, err := s.change(line{Op: "tool", ID: it.ID, Tool: &call}, func(*Item) bool { return true }, nil)
	return out, err
}

// AnswerCard records a card's answer. The first answer wins; a later one gets ErrConflict with
// the item, which holds the first answer.
func (s *Store) AnswerCard(id, choice, by string) (Item, error) {
	a := &Answer{Choice: choice, By: by, Time: s.now().UTC()}
	out, _, err := s.change(line{Op: "answer", ID: id, Answer: a}, func(it *Item) bool { return it.Answer == nil }, func(it *Item) error {
		if it.Card == nil { // a card, or a note that offers a choice (a memory line's Undo)
			return fmt.Errorf("%w: item %s is not a card", ErrNotFound, id)
		}
		if it.Answer != nil {
			return ErrConflict
		}
		return nil
	})
	return out, err
}

// change writes l when ok says the item needs it, then applies it. check, when set, may refuse.
func (s *Store) change(l line, ok func(*Item) bool, check func(*Item) error) (Item, bool, error) {
	s.mu.Lock()
	it := s.byID[l.ID]
	if it == nil {
		s.mu.Unlock()
		return Item{}, false, fmt.Errorf("%w: %s", ErrNotFound, l.ID)
	}
	if check != nil {
		if err := check(it); err != nil {
			out := clone(it)
			s.mu.Unlock()
			return out, false, err
		}
	}
	if !ok(it) {
		out := clone(it)
		s.mu.Unlock()
		return out, false, nil
	}
	if err := s.write(s.now(), l); err != nil {
		s.mu.Unlock()
		return Item{}, false, err
	}
	s.apply(l)
	out := clone(it)
	s.mu.Unlock()
	s.notify(out)
	return out, true, nil
}

// Get returns one item.
func (s *Store) Get(id string) (Item, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := s.byID[id]
	if it == nil {
		return Item{}, false
	}
	return clone(it), true
}

// ByKey returns the user message stored with an idempotency key.
func (s *Store) ByKey(key string) (Item, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := s.byKey[key]
	if it == nil {
		return Item{}, false
	}
	return clone(it), true
}

// Last returns the newest item's id, or "".
func (s *Store) Last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items) == 0 {
		return ""
	}
	return s.items[len(s.items)-1].ID
}

// Page returns items older than before (all when before is ""), newest first, at most limit
// (PageMax when limit is 0 or above it), and whether older items remain.
func (s *Store) Page(before string, limit int) ([]Item, bool) {
	if limit <= 0 || limit > PageMax {
		limit = PageMax
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	end := len(s.items)
	if before != "" {
		end = sort.Search(len(s.items), func(i int) bool { return s.items[i].ID >= before })
	}
	start := max(0, end-limit)
	out := make([]Item, 0, end-start)
	for i := end - 1; i >= start; i-- {
		out = append(out, clone(s.items[i]))
	}
	return out, start > 0
}

// After returns the items after id (all when id is ""), oldest first, that match keep.
func (s *Store) After(id string, keep func(Item) bool) []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := 0
	if id != "" {
		i = sort.Search(len(s.items), func(i int) bool { return s.items[i].ID > id })
	}
	var out []Item
	for ; i < len(s.items); i++ {
		if keep == nil || keep(*s.items[i]) {
			out = append(out, clone(s.items[i]))
		}
	}
	return out
}

// Tail returns the newest n items that match keep, oldest first.
func (s *Store) Tail(n int, keep func(Item) bool) []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Item
	for i := len(s.items) - 1; i >= 0 && len(out) < n; i-- {
		if keep == nil || keep(*s.items[i]) {
			out = append(out, clone(s.items[i]))
		}
	}
	slices.Reverse(out)
	return out
}

// Files lists the month files, for tests and for the token check of the state directory.
func (s *Store) Files() []string {
	fs, _ := filepath.Glob(filepath.Join(s.dir, "*"))
	return slicesFilter(fs, func(f string) bool { return strings.Contains(filepath.Base(f), ".jsonl") })
}

func slicesFilter(in []string, keep func(string) bool) []string {
	var out []string
	for _, s := range in {
		if keep(s) {
			out = append(out, s)
		}
	}
	return out
}

func clone(it *Item) Item {
	c := *it
	c.Images = append([]string(nil), it.Images...)
	c.Context = append([]Context(nil), it.Context...)
	c.Tools = append([]ToolCall(nil), it.Tools...)
	if it.Card != nil {
		card := *it.Card
		card.Choices = append([]string(nil), it.Card.Choices...)
		c.Card = &card
	}
	if it.Answer != nil {
		a := *it.Answer
		c.Answer = &a
	}
	return c
}
