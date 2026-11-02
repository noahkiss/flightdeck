package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/catalog"
	"github.com/noahkiss/flightdeck/internal/channels"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/suggest"
)

// handOffWait is how long hand_off waits for an idle agent to take the message, so it can say
// "delivered" rather than "handed". Delivery is tried as soon as the message arrives, so an idle
// agent on this hab or an online peer takes it well within this (capcom.md 3.2).
const handOffWait = 5 * time.Second

// handOffTarget is one running agent on the project, a candidate for a hand-off.
type handOffTarget struct {
	Host    string `json:"host" doc:"The host's label as this hab names it; this hab's host id for itself."`
	Address string `json:"address" doc:"The channel address the message went to: label/name, or the name on this hab."`
	api.CatalogAgent
	Offline bool `json:"offline,omitempty" doc:"The host is offline; the row is from its last catalog."`
}

// handOffResult is hand_off's -json result.
type handOffResult struct {
	Sent     bool               `json:"sent"`
	Project  string             `json:"project"`
	Agent    *handOffTarget     `json:"agent,omitempty"`
	Why      string             `json:"why,omitempty" doc:"Why this agent was picked."`
	Others   []handOffTarget    `json:"others,omitempty" doc:"The other running agents on the project."`
	Delivery string             `json:"delivery,omitempty" doc:"delivered, queued (waits for the agent to be idle), held (its host is offline) or failed."`
	Message  *api.ChannelRecord `json:"message,omitempty"`
}

// ready reports whether an agent in state s takes a message now (mesh.md 6.5, Delivery).
func ready(s api.AgentStatus) bool {
	return s == api.AgentStatusIdle || s == api.AgentStatusDone || s == api.AgentStatusError
}

// projectAgents finds the running agents on a project in the merged catalog. The project is the
// catalog group named by name, key, remote or folder (matchProject); a name two groups share
// returns both groups and no agents. An agent names its project by its own host's name for it,
// so on each host the agents count whose project name is one that host gives a project of the
// group's key; a name the host also gives another project counts for neither. A project the
// catalog does not list falls back to the agents' project name. host, when given, keeps one
// host. It returns the project's name.
func projectAgents(cl api.CatalogList, project, host string) (string, []handOffTarget, []api.CatalogProjectGroup) {
	var groups []api.CatalogProjectGroup
	for _, p := range cl.Projects {
		if matchProject(p, project) {
			groups = append(groups, p)
		}
	}
	if len(groups) > 1 {
		return project, nil, groups
	}
	name, key := project, ""
	if len(groups) == 1 {
		name, key = groups[0].Name, groups[0].Key
	}
	var out []handOffTarget
	for _, h := range cl.Hosts {
		label := h.Label
		if label == "" {
			label = h.Catalog.Host
		}
		if host != "" && host != label && host != h.Catalog.Host {
			continue
		}
		names := map[string]bool{} // this host's names for the project, folded
		if key == "" {
			names[strings.ToLower(name)] = true
		} else {
			other := map[string]bool{}
			for _, cp := range h.Catalog.Projects {
				if catalog.ProjectKey(h.Catalog.DeviceID, cp) == key {
					names[strings.ToLower(cp.Name)] = true
				} else {
					other[strings.ToLower(cp.Name)] = true
				}
			}
			for n := range other {
				delete(names, n)
			}
		}
		for _, ag := range h.Catalog.Agents {
			if ag.Project == "" || !names[strings.ToLower(ag.Project)] || ag.State == api.AgentStatusNone || ag.State == api.AgentStatusExited {
				continue
			}
			addr := ag.Session
			if !h.Own {
				addr = label + "/" + ag.Session
			}
			out = append(out, handOffTarget{Host: label, Address: addr, CatalogAgent: ag, Offline: h.Stale})
		}
	}
	return name, out, nil
}

// matchProject is suggest.Matches, and for an absolute folder on this host also the folder
// with its links resolved, as the catalog names a checkout (on macOS /var is /private/var).
func matchProject(p api.CatalogProjectGroup, want string) bool {
	if suggest.Matches(p, want) {
		return true
	}
	if filepath.IsAbs(want) {
		if r, err := filepath.EvalSymlinks(want); err == nil && r != want {
			return suggest.Matches(p, r)
		}
	}
	return false
}

// pickTarget orders the candidates by capcom.md 3.2: the agent the user named; else an online
// agent that takes a message now; else an online busy one; else one on an offline host; within
// each, the most recently active. It returns the pick and why, or "" when named matches none.
func pickTarget(ts []handOffTarget, named string) (int, string) {
	if named != "" {
		for i, t := range ts {
			if t.Session == named || t.Address == named || t.Host+"/"+t.Session == named {
				return i, "the agent the user named"
			}
		}
		return -1, ""
	}
	tier := func(t handOffTarget) int {
		switch {
		case t.Offline:
			return 2
		case ready(t.State):
			return 0
		}
		return 1
	}
	order := make([]int, len(ts))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		if c := cmp.Compare(tier(ts[a]), tier(ts[b])); c != 0 {
			return c
		}
		return laterFirst(ts[a].Since, ts[b].Since)
	})
	i := order[0]
	if len(ts) == 1 {
		return i, "the only agent running on the project"
	}
	switch tier(ts[i]) {
	case 0:
		return i, fmt.Sprintf("%s, so it takes the message now; the most recently active of %d agents that are", ts[i].State, countTier(ts, tier, 0))
	case 1:
		return i, fmt.Sprintf("no agent on the project is idle; the most recently active of %d busy agents", len(ts)-countTier(ts, tier, 2))
	}
	return i, "every agent on the project is on an offline host; the most recently active"
}

// laterFirst orders two RFC 3339 times, the later first, as times: the hab trims trailing zeros
// of the fraction, so the strings do not sort. A time that does not parse goes last.
func laterFirst(a, b string) int {
	ta, ea := time.Parse(time.RFC3339Nano, a)
	tb, eb := time.Parse(time.RFC3339Nano, b)
	switch {
	case ea != nil || eb != nil:
		return cmp.Compare(boolInt(ea != nil), boolInt(eb != nil))
	case ta.After(tb):
		return -1
	case tb.After(ta):
		return 1
	}
	return 0
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func countTier(ts []handOffTarget, tier func(handOffTarget) int, n int) int {
	c := 0
	for _, t := range ts {
		if tier(t) == n {
			c++
		}
	}
	return c
}

// paneDoor is a caller that reaches the hab through a pane's GFF_HAB route, where the hab knows
// the calling session.
type paneDoor interface{ fromPane() bool }

func (hookCaller) fromPane() bool { return true }

// runHandOff sends the user's request to the agent already on a project, over channels, with
// no confirmation (capcom.md 3.2, owner 2026-11-01). The sender is the calling pane's session.
func runHandOff(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	text := a.str("message")
	if strings.TrimSpace(text) == "" {
		return nil, "", control.Errorf(api.ErrorKindInvalid, "hand_off needs message: the request to send")
	}
	if len(text) > channels.MaxText {
		return nil, "", control.Errorf(api.ErrorKindInvalid, "the message is %d bytes; at most %d", len(text), channels.MaxText)
	}
	if p, ok := c.(paneDoor); !ok || !p.fromPane() {
		return nil, "", control.Errorf(api.ErrorKindInvalid, "hand_off needs to run from a session's pane: your session is the sender. Outside one, gff-hab send -from <session> sends as a named session")
	}
	cl, err := readCatalog(ctx, c)
	if err != nil {
		return nil, "", err
	}
	project, host := a.str("project"), a.str("host")
	name, ts, groups := projectAgents(cl, project, host)
	res := handOffResult{Project: name}
	var b strings.Builder
	if len(groups) > 1 {
		fmt.Fprintf(&b, "%s names %d projects: nothing was sent. Pass the project's remote or a folder instead:\n", project, len(groups))
		for _, g := range groups {
			fmt.Fprintf(&b, "  %s (%s)\n", g.Name, g.Key)
		}
		return res, b.String(), nil
	}
	on := name
	if host != "" {
		on += " on " + host
	}
	if len(ts) == 0 {
		fmt.Fprintf(&b, "No agent is running on %s: nothing was sent. To start one, run suggest_host for the project, then start_session on the host it suggests.\n", on)
		return res, b.String(), nil
	}
	named := a.str("agent")
	var (
		t    handOffTarget
		why  string
		sent api.ChannelSent
	)
	for {
		i, w := pickTarget(ts, named)
		if i < 0 {
			fmt.Fprintf(&b, "No agent named %s is running on %s: nothing was sent. Running there:\n", named, on)
			for _, t := range ts {
				fmt.Fprintf(&b, "  %s: %s, %s, since %s\n", t.Address, t.Harness, t.State, t.Since)
			}
			return res, b.String(), nil
		}
		t, why = ts[i], w
		err := c.Call(ctx, "channel.send", api.ChannelSendParams{To: t.Address, Text: text}, &sent)
		if err != nil && strings.Contains(err.Error(), channels.SelfRefusal) {
			// The pick is the calling session itself: it never hands off to itself.
			ts = slices.Delete(ts, i, i+1)
			if named != "" {
				fmt.Fprintf(&b, "%s is your own session: nothing was sent.\n", named)
				return res, b.String(), nil
			}
			if len(ts) == 0 {
				fmt.Fprintf(&b, "No agent other than you is running on %s: nothing was sent. To start one, run suggest_host for the project, then start_session on the host it suggests.\n", on)
				return res, b.String(), nil
			}
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("hand_off to %s: %w", t.Address, err)
		}
		for _, o := range ts {
			if o.Address != t.Address {
				res.Others = append(res.Others, o)
			}
		}
		break
	}
	res.Agent, res.Why = &t, why
	res.Sent = true
	rec := api.ChannelRecord{ID: sent.ID, To: sent.To, State: sent.State}
	if ready(t.State) && !t.Offline {
		var st api.ChannelStatusResult
		if err := c.Call(ctx, "channel.status", api.ChannelStatusParams{ID: sent.ID, WaitMS: int(handOffWait / time.Millisecond)}, &st); err == nil {
			rec = st.Record
		}
	}
	res.Message = &rec
	who := fmt.Sprintf("%s (%s, %s, on %s)", t.Address, t.Harness, t.State, t.Host)
	switch {
	case rec.State == api.ChannelStateDelivered:
		res.Delivery = "delivered"
		fmt.Fprintf(&b, "Delivered: %s has the message now.\n", who)
	case channels.Final(rec.State):
		res.Delivery = "failed"
		fmt.Fprintf(&b, "Not delivered (%s): %s. %s does not have the message.\n", rec.State, rec.Reason, who)
	case t.Offline:
		res.Delivery = "held"
		fmt.Fprintf(&b, "Held: %s is offline. The message waits in this hab's outbox and goes when the host is back, for up to 7 days.\n", who)
	case t.State == api.AgentStatusBlocked:
		res.Delivery = "queued"
		fmt.Fprintf(&b, "Queued: %s is blocked on a permission request; it gets the message once that is answered. Point the user at the open approval in Flightdeck.\n", who)
	case !ready(t.State):
		res.Delivery = "queued"
		fmt.Fprintf(&b, "Queued: %s is working; it gets the message when idle.\n", who)
	default:
		res.Delivery = "queued"
		fmt.Fprintf(&b, "Queued: %s did not take the message within %d s; it may have started working, and gets it when idle.\n", who, int(handOffWait/time.Second))
	}
	fmt.Fprintf(&b, "Why this agent: %s.\n", why)
	for _, o := range res.Others {
		fmt.Fprintf(&b, "Also on %s: %s (%s, %s).\n", name, o.Address, o.Harness, o.State)
	}
	fmt.Fprintf(&b, "Message %s, state %s. A reply reaches you as an agent message.\n", rec.ID, rec.State)
	return res, b.String(), nil
}

// --- file_record ---

// The Logbook write that file_record makes (logbook.md 10.3). Logbook (Part 12) owns the
// method and its shapes; these are only what Capcom sends and reads back (capcom.md 3.4, 6.2).
// No hab serves logbook.write yet, so they stay here, out of the API, until Part 12 adds the
// method.

// logbookWrite is one batch of changes. Propose makes it a pending proposal whatever the hab's
// logbook.approval says: Capcom files proposals by its own rule (logbook.md 9.1).
type logbookWrite struct {
	IdempotencyKey string          `json:"idempotency_key"`
	Propose        bool            `json:"propose"`
	Changes        []logbookChange `json:"changes"`
	Source         *logbookSource  `json:"source,omitempty"`
}

// logbookChange is one change; file_record makes only creates.
type logbookChange struct {
	Op      string `json:"op"`
	Type    string `json:"type"`
	Project string `json:"project,omitempty"`
	Title   string `json:"title"`
	Body    string `json:"body,omitempty"`
}

// logbookSource is where a record came from (logbook.md 2.5). The hab fills in the session
// from the pane's token.
type logbookSource struct {
	Message string `json:"message,omitempty"`
	Quote   string `json:"quote"`
}

// logbookWritten answers logbook.write.
type logbookWritten struct {
	State    string   `json:"state"` // applied or proposed
	Proposal string   `json:"proposal,omitempty"`
	IDs      []string `json:"ids,omitempty"`
}

// recordTypes are the types file_record files (logbook.md 2.2, less proposal).
var recordTypes = []string{"task", "finding", "issue", "plan", "guide"}

// fileRecordResult is file_record's -json result.
type fileRecordResult struct {
	Filed    bool   `json:"filed"`
	State    string `json:"state,omitempty"`
	Proposal string `json:"proposal,omitempty"`
}

// runFileRecord files a Logbook record as a pending proposal, with the conversation quote as its
// source (capcom.md 3.4). Until a hab has Logbook it answers unavailable and sends no write.
func runFileRecord(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	typ, title, body, quote := a.str("type"), strings.TrimSpace(a.str("title")), a.str("body"), a.str("quote")
	switch {
	case !slices.Contains(recordTypes, typ):
		return nil, "", control.Errorf(api.ErrorKindInvalid, "type %q is not one of %s", typ, strings.Join(recordTypes, ", "))
	case title == "" || strings.ContainsAny(title, "\r\n"):
		return nil, "", control.Errorf(api.ErrorKindInvalid, "title must be one line")
	case utf8.RuneCountInString(title) > 200:
		return nil, "", control.Errorf(api.ErrorKindInvalid, "title is over 200 characters")
	case len(body) > 256<<10:
		return nil, "", control.Errorf(api.ErrorKindInvalid, "body is over 256 KiB")
	case strings.TrimSpace(quote) == "":
		return nil, "", control.Errorf(api.ErrorKindInvalid, "file_record needs quote: the user's words the record came from")
	}
	// The capability check. A hab without Logbook answers logbook.find unavailable, at either
	// door (the pane route takes it as a read); then nothing is written anywhere.
	var probe api.LogbookRecords
	if err := c.Call(ctx, "logbook.find", api.LogbookFindParams{Limit: 1}, &probe); err != nil {
		if errKindOf(err) == api.ErrorKindUnavailable {
			return nil, "", control.Errorf(api.ErrorKindUnavailable, "Logbook is not on this host yet, so the record was not filed and nothing was written. Tell the user, and keep the draft in this conversation")
		}
		return nil, "", err
	}
	project := a.str("project")
	if project != "" {
		cl, err := readCatalog(ctx, c)
		if err != nil {
			return nil, "", err
		}
		key := ""
		for _, p := range cl.Projects {
			if matchProject(p, project) {
				key = p.Key
				break
			}
		}
		if key == "" {
			return nil, "", control.Errorf(api.ErrorKindNotFound, "no project %q in the catalog; the projects tool lists them. Leave project out for a record that belongs to no project", project)
		}
		project = key
	}
	k := make([]byte, 16)
	rand.Read(k)
	w := logbookWrite{IdempotencyKey: hex.EncodeToString(k), Propose: true,
		Changes: []logbookChange{{Op: "create", Type: typ, Project: project, Title: title, Body: body}},
		Source:  &logbookSource{Message: a.str("source_message"), Quote: quote}}
	var r logbookWritten
	if err := c.Call(ctx, "logbook.write", w, &r); err != nil {
		return nil, "", fmt.Errorf("file_record: the hab did not take the proposal, so nothing was filed: %w", err)
	}
	res := fileRecordResult{Filed: true, State: r.State, Proposal: r.Proposal}
	if r.State != "proposed" {
		// A hab that ignored propose: say what happened, plainly.
		return res, fmt.Sprintf("The hab filed the %s %q at once (state %s), not as a proposal: %s. Tell the user it is in Logbook already.\n", typ, title, r.State, strings.Join(r.IDs, ", ")), nil
	}
	return res, fmt.Sprintf("Proposed: the %s %q waits for the user's review as proposal %s. Nothing is in Logbook until the user approves it. Tell the user in one line; do not file it again.\n", typ, title, r.Proposal), nil
}

// errKindOf returns an API error's kind, or "".
func errKindOf(err error) api.ErrorKind {
	var ce *control.Error
	if errors.As(err, &ce) {
		return ce.Data.Kind
	}
	return ""
}
