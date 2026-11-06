package logbook

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// ExportRecord is one line of the JSONL export (13, 13.1): a record as it folds now. The same
// shape is what an import reads.
type ExportRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Title   string `json:"title"`
	Body    string `json:"body,omitempty"`
	Status  string `json:"status,omitempty"`
	Project string `json:"project,omitempty"`
	// Fields holds the other scalar fields the record has: assignee, priority, pinned and the
	// like.
	Fields     map[string]json.RawMessage `json:"fields,omitempty"`
	Tags       []string                   `json:"tags,omitempty"`
	Identities []string                   `json:"identities,omitempty"`
	Links      []Link                     `json:"links,omitempty"`
	Notes      []ExportNote               `json:"notes,omitempty"`
	// Created is when the record was made, RFC 3339 in UTC: an imported record's original date.
	Created   string          `json:"created"`
	CreatedBy *Actor          `json:"created_by,omitempty"`
	Source    json.RawMessage `json:"source,omitempty"`
	Deleted   bool            `json:"deleted,omitempty"`
	Imported  bool            `json:"imported,omitempty"`
}

// ExportNote is one note of an exported record.
type ExportNote struct {
	Time  string `json:"time"`
	Text  string `json:"text"`
	Actor *Actor `json:"actor,omitempty"`
}

// exportSkip are the fields ExportRecord carries on its own, or that only the store sets.
var exportSkip = []string{"type", "title", "body", "status", "project", "deleted", "created", "imported"}

// stampTime writes a stamp's wall time as RFC 3339 in UTC.
func stampTime(h HLC) string {
	return time.UnixMilli(h.Wall).UTC().Format(time.RFC3339Nano)
}

// ExportOf turns a record into its export line.
func ExportOf(r *Record) ExportRecord {
	e := ExportRecord{ID: r.ID, Type: r.Type, Title: r.Title, Body: r.Body, Status: r.Status, Project: r.Project,
		Tags: r.Tags, Identities: r.Identities, Created: stampTime(r.Created), CreatedBy: r.CreatedBy,
		Deleted: r.Deleted}
	if len(r.Source) > 0 && string(r.Source) != "null" {
		e.Source = r.Source
	}
	for name, fv := range r.Fields {
		switch name {
		case "created":
			var s string
			if json.Unmarshal(fv.Value, &s) == nil && s != "" {
				e.Created = s
			}
			continue
		case "imported":
			_ = json.Unmarshal(fv.Value, &e.Imported)
			continue
		}
		if slices.Contains(exportSkip, name) {
			continue
		}
		if e.Fields == nil {
			e.Fields = map[string]json.RawMessage{}
		}
		e.Fields[name] = fv.Value
	}
	for _, l := range r.Links {
		e.Links = append(e.Links, Link{Kind: l.Kind, To: l.To, Pos: l.Pos})
	}
	for _, n := range r.Notes {
		e.Notes = append(e.Notes, ExportNote{Time: stampTime(n.At), Text: n.Text, Actor: n.Actor})
	}
	return e
}

// Export writes every record, deleted ones included, one JSON object per line: projects first,
// then the rest, each by id. It returns the count written.
func (s *Store) Export(ctx context.Context, w io.Writer) (int, error) {
	ids, err := s.IDs(ctx)
	if err != nil {
		return 0, err
	}
	var projects, rest []ExportRecord
	for _, id := range ids {
		r, err := s.Get(ctx, id)
		if errors.Is(err, ErrNotFound) {
			continue // gone between the list and the read
		}
		if err != nil {
			return 0, err
		}
		if r.Type == TypeProject {
			projects = append(projects, ExportOf(r))
		} else {
			rest = append(rest, ExportOf(r))
		}
	}
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	n := 0
	for _, e := range append(projects, rest...) {
		if err := enc.Encode(e); err != nil {
			return n, err
		}
		n++
	}
	return n, bw.Flush()
}

// ImportResult counts what an import did.
type ImportResult struct {
	Written int `json:"written"`
	// Exists counts records whose id this replica already holds: an import run twice writes
	// nothing the second time.
	Exists int `json:"exists"`
	// Skipped counts proposals: a proposal is the filing hab's review item, not a record to carry.
	Skipped int `json:"skipped"`
}

// MaxImportLine bounds one line of an import: a record with its notes.
const MaxImportLine = 16 << 20

// Import reads JSONL records (ExportRecord) and writes each as one create batch from this
// replica, with its id verbatim (2.3), its original date as `created` and `imported` set (13.1).
// It stops at the first line it cannot write, naming the line; the lines before it stay
// written, and a second run skips them.
func (s *Store) Import(ctx context.Context, r io.Reader, actor *Actor) (ImportResult, error) {
	var res ImportResult
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), MaxImportLine)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var e ExportRecord
		dec := json.NewDecoder(strings.NewReader(text))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&e); err != nil {
			return res, fmt.Errorf("line %d: %v", line, err)
		}
		if e.Type == TypeProposal {
			res.Skipped++
			continue
		}
		if _, err := s.Get(ctx, e.ID); err == nil {
			res.Exists++
			continue
		} else if !errors.Is(err, ErrNotFound) {
			return res, err
		}
		steps, err := importSteps(e)
		if err != nil {
			return res, fmt.Errorf("line %d (%s): %w", line, e.ID, err)
		}
		if _, err := s.Write(ctx, Batch{Actor: actor, Steps: steps}); err != nil {
			return res, fmt.Errorf("line %d (%s): %w", line, e.ID, err)
		}
		res.Written++
	}
	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("line %d: %v", line+1, err)
	}
	return res, nil
}

// importSteps makes one export line into a create batch.
func importSteps(e ExportRecord) ([]Step, error) {
	if !ValidID(e.ID) {
		return nil, invalid("", "id %q: want <letters>-<3 to 16 letters or digits>", e.ID)
	}
	created := time.Now().UTC().Format(time.RFC3339Nano)
	if e.Created != "" {
		t, err := time.Parse(time.RFC3339Nano, e.Created)
		if err != nil {
			return nil, invalid("", "created is an RFC 3339 time")
		}
		created = t.UTC().Format(time.RFC3339Nano)
	}
	f := map[string]any{"title": e.Title, "created": created, "imported": true}
	if e.Body != "" {
		f["body"] = e.Body
	}
	if e.Status != "" {
		f["status"] = e.Status
	}
	if e.Project != "" {
		f["project"] = e.Project
	}
	if e.Deleted {
		f["deleted"] = true
	}
	for name, v := range e.Fields {
		if slices.Contains(exportSkip, name) {
			continue
		}
		f[name] = v
	}
	create := Step{Kind: KindCreate, Record: e.ID, Type: e.Type, Fields: f}
	if len(e.Source) > 0 {
		var src Source
		if json.Unmarshal(e.Source, &src) == nil && src != (Source{}) {
			create.Source = &src
		}
	}
	steps := []Step{create}
	for _, t := range e.Tags {
		steps = append(steps, Step{Kind: KindAdd, Record: e.ID, Field: "tags", Value: t})
	}
	for _, id := range e.Identities {
		steps = append(steps, Step{Kind: KindAdd, Record: e.ID, Field: "identities", Value: id})
	}
	for _, l := range e.Links {
		steps = append(steps, Step{Kind: KindAdd, Record: e.ID, Field: "links", Value: l})
	}
	for _, n := range e.Notes {
		steps = append(steps, Step{Kind: KindAppend, Record: e.ID, Field: "notes", Value: n.Text})
	}
	return steps, nil
}
