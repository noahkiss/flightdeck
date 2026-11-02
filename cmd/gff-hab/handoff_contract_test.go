package main

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// The patterns the app's hand-off card and proposal line read the answers with. They are
// copies of the regexes in apple/FlightdeckKit/Sources/FlightdeckKit/Chat/CapcomCards.swift
// (HandOffCard.from and ProposalLine.from; app.md 4.8). Change both together: a wording change
// in handoff.go that the app cannot read fails here.
var (
	appWhoLead   = regexp.MustCompile(`^(?:Delivered|Queued|Held): (\S+) \(([^,()]*), ([^,()]*), on ([^()]+)\)`)
	appWhoFailed = regexp.MustCompile(`(\S+) \(([^,()]*), ([^,()]*), on ([^()]+)\) does not have the message\.$`)
	appWhy       = regexp.MustCompile(`^Why this agent: (.*?)\.?$`)
	appAlso      = regexp.MustCompile(`^Also on .+: (\S+) \(([^,()]*), ([^,()]*)\)\.$`)
	appMessage   = regexp.MustCompile(`^Message (\S+), state (\S+)\.`)
	appProposal  = regexp.MustCompile(`review as proposal (\S+)\. Nothing is in Logbook`)
)

// contractHab is fakeChannelHab with the channel.status answer set per case.
type contractHab struct {
	*fakeChannelHab
	status api.ChannelRecord
}

func (f *contractHab) Call(ctx context.Context, method string, params, result any) error {
	if method == "channel.status" {
		*result.(*api.ChannelStatusResult) = api.ChannelStatusResult{Record: f.status}
		return nil
	}
	return f.fakeChannelHab.Call(ctx, method, params, result)
}

// TestHandOffAnswerContract drives every delivery branch of hand_off and checks each line of
// the answer against the app's patterns: the lead word and "who" on the first line, then the
// why, the other candidates and the message line, and nothing the card cannot place.
func TestHandOffAnswerContract(t *testing.T) {
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cases := []struct {
		name    string
		lead    string
		state   api.AgentStatus
		stale   bool
		status  api.ChannelRecord
		channel string
	}{
		{"delivered", "Delivered:", api.AgentStatusIdle, false, api.ChannelRecord{ID: id, State: api.ChannelStateDelivered}, "delivered"},
		{"queued working", "Queued:", api.AgentStatusWorking, false, api.ChannelRecord{}, "queued"},
		{"queued blocked", "Queued:", api.AgentStatusBlocked, false, api.ChannelRecord{}, "queued"},
		{"queued not taken", "Queued:", api.AgentStatusIdle, false, api.ChannelRecord{ID: id, State: api.ChannelStateQueued}, "queued"},
		{"held", "Held:", api.AgentStatusIdle, true, api.ChannelRecord{}, "queued"},
		{"not delivered", "Not delivered (not_delivered): ", api.AgentStatusIdle, false,
			api.ChannelRecord{ID: id, State: api.ChannelStateNotDelivered, Reason: "the pane of x (a, b, on c) closed"}, "not_delivered"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &contractHab{fakeChannelHab: &fakeChannelHab{fakeReadHab: twoHosts()}, status: tc.status}
			cl := &f.catalog
			withHostProjects(cl)
			cl.Hosts[0].Catalog.Agents = nil
			cl.Hosts[1].Stale = tc.stale
			cl.Hosts[1].Catalog.Agents = []api.CatalogAgent{
				{Session: "app-api", Harness: "codex", Project: "app", State: tc.state, Since: "2026-03-01T11:00:00Z"},
				{Session: "app-two", Harness: "claude", Project: "app", State: api.AgentStatusWorking, Since: "2026-03-01T10:00:00Z"},
			}
			_, text, err := runAction(t, f, "hand_off", toolArgs{"project": "app", "agent": "app-api", "message": "fix it"})
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
			first := lines[0]
			if !strings.HasPrefix(first, tc.lead) {
				t.Fatalf("first line %q does not start with %q", first, tc.lead)
			}
			who := appWhoLead
			if tc.name == "not delivered" {
				who = appWhoFailed
			}
			m := who.FindStringSubmatch(first)
			if m == nil || m[1] != "devbox/app-api" || m[2] != "codex" || m[3] != string(tc.state) || m[4] != "devbox" {
				t.Fatalf("who in %q: %q", first, m)
			}
			var why, also, msg bool
			for _, l := range lines[1:] {
				switch {
				case appWhy.MatchString(l):
					why = appWhy.FindStringSubmatch(l)[1] == "the agent the user named"
				case appAlso.MatchString(l):
					a := appAlso.FindStringSubmatch(l)
					also = a[1] == "devbox/app-two" && a[2] == "claude" && a[3] == "working"
				case appMessage.MatchString(l):
					s := appMessage.FindStringSubmatch(l)
					msg = s[1] == id && s[2] == tc.channel
				default:
					t.Errorf("a line the card cannot read: %q", l)
				}
			}
			if !why || !also || !msg {
				t.Fatalf("why %v, also %v, message %v:\n%s", why, also, msg, text)
			}
		})
	}
}

// TestFileRecordAnswerContract: the proposed and filed answers start with the words the app's
// proposal line reads, and the proposal id is found even when the title holds the same words.
func TestFileRecordAnswerContract(t *testing.T) {
	lb := &fakeLogbookHab{fakeReadHab: twoHosts()}
	_, text, err := runAction(t, lb, "file_record", toolArgs{"type": "task", "title": "Rename as proposal X. later", "quote": "q"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, "Proposed:") {
		t.Fatalf("proposed answer: %q", text)
	}
	if m := appProposal.FindStringSubmatch(text); m == nil || m[1] != "proposal-00000001" {
		t.Fatalf("proposal id in %q: %q", text, m)
	}

	// A hab that ignores propose: the fake applies a write without it.
	f := &filedHab{fakeLogbookHab: &fakeLogbookHab{fakeReadHab: twoHosts()}}
	_, text, err = runAction(t, f, "file_record", toolArgs{"type": "task", "title": "x", "quote": "q"})
	if err != nil || !strings.HasPrefix(text, "The hab filed") {
		t.Fatalf("filed answer: %v %q", err, text)
	}
}

// filedHab answers logbook.write as applied, as a hab that ignored propose would.
type filedHab struct{ *fakeLogbookHab }

func (f *filedHab) Call(ctx context.Context, method string, params, result any) error {
	if method == "logbook.write" {
		*result.(*api.LogbookWritten) = api.LogbookWritten{State: "applied", IDs: []string{"task-1"}}
		return nil
	}
	return f.fakeLogbookHab.Call(ctx, method, params, result)
}
