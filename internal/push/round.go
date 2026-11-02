package push

import (
	"fmt"
	"strings"
)

// KindRound is the push kind of round notifications (rounds.md 10.2). A device gets them only
// when it named the kind in push.register.
const KindRound = "round"

// The rows of rounds.md 10.2, as api.RoundNoticeKind words.
const (
	RoundRunFailed     = "run_failed"
	RoundRunSucceeded  = "run_succeeded"
	RoundPaused        = "paused"
	RoundSkips         = "skips"
	RoundTargetGone    = "target_gone"
	RoundStateSetAside = "state_set_aside"
)

// RoundEvent is one round notification in the words its banner needs.
type RoundEvent struct {
	Kind      string // a Round* row
	Round     string // the round id; empty for RoundStateSetAside
	Name      string
	Run       string // the run id, when a run raised it
	RunNumber int
	State     string // the run's end state: failed, timed_out, succeeded, skipped
	Reason    string // why the run ended
	Result    string // the run's final agent message
	Since     string // RoundSkips: when the round last ran well, in plain words
	Session   string // the run session, for Open; empty when there is none to open
	Failures  int    // RoundPaused: the failures in a row
}

// RoundAllowed says whether a round's notify setting (failures, always or never; empty is
// failures) lets a row through (rounds.md 10.2): never leaves only the self-pause, failures
// leaves out the success, always lets every row through. The state file row is no round's.
func RoundAllowed(notify, kind string) bool {
	switch {
	case kind == RoundPaused || kind == RoundStateSetAside:
		return true
	case notify == "never":
		return false
	case kind == RoundRunSucceeded:
		return notify == "always"
	}
	return true
}

// RoundCollapse is the collapse id of a round's notifications: one banner per round, updated
// (rounds.md 10.2).
func RoundCollapse(round string) string {
	if round == "" {
		return "round-state"
	}
	return collapseID("round-", round)
}

// firstLine is the first line of s with text on it.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// RoundNotice is the notice a round event raises: its banner's title, body and level, kind
// round, collapsed by round id. A banner about a run session opens it.
func RoundNotice(e RoundEvent) Notice {
	no := Notice{Kind: KindRound, Collapse: RoundCollapse(e.Round), Round: e.Round, Run: e.Run, Session: e.Session}
	if e.Session != "" {
		no.Category = CategoryOpen
	}
	switch e.Kind {
	case RoundRunFailed:
		no.Title, no.Level = "Round failed: "+e.Name, LevelActive
		if e.State == "timed_out" {
			no.Title = "Round timed out: " + e.Name
		}
		no.Body = e.Reason
		if no.Body == "" {
			no.Body = fmt.Sprintf("Run %d did not succeed.", e.RunNumber)
		}
	case RoundRunSucceeded:
		no.Title, no.Level = "Round done: "+e.Name, LevelPassive
		no.Body = firstLine(e.Result)
		if no.Body == "" {
			no.Body = fmt.Sprintf("Run %d succeeded.", e.RunNumber)
		}
	case RoundPaused:
		no.Title, no.Level = "Round paused: "+e.Name, LevelActive
		no.Body = fmt.Sprintf("%d failed runs in a row. Unpause it to run again.", max(e.Failures, 1))
	case RoundSkips:
		no.Title, no.Level = "Round not running: "+e.Name, LevelPassive
		no.Body = "Not run since " + e.Since
		if e.Reason != "" {
			no.Body += ": " + e.Reason
		}
	case RoundTargetGone:
		no.Title, no.Level = "Round completed: "+e.Name, LevelPassive
		no.Body = "Its target session is gone."
		no.Session, no.Category = "", "" // nothing left to open
	case RoundStateSetAside:
		no.Title, no.Level = "Rounds state set aside", LevelActive
		no.Body = "The rounds state file did not parse. gff-hab kept it beside the new one and started with no rounds."
		no.Session, no.Category = "", ""
	default:
		no.Title, no.Level = "Round: "+e.Name, LevelActive
		no.Body = e.Reason
	}
	return no
}
