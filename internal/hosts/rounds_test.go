package hosts_test

import (
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/board"
	"github.com/noahkiss/flightdeck/internal/hosts"
)

func fakeRound(id, name, next string) api.Round {
	return api.Round{ID: id, RoundSpec: api.RoundSpec{Name: name, Mode: api.RoundModeFresh,
		Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindCron, Cron: "0 9 * * *", Zone: "UTC"}}, NextSlot: next}
}

func roundOrder(hs []hosts.Host) string {
	rows := board.RoundRows(hs, nil)
	board.SortRounds(rows)
	var out []string
	for _, r := range rows {
		s := r.Host + "/" + r.Name + "/" + r.Mark()
		if r.Stale {
			s += "/stale"
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

// TestRoundsAcrossHosts: two fake habs' rounds reach the host models from the snapshot and then
// from round.updated and round.run_updated; the board lists them in next-run order across both;
// a stopped hab's rounds stay, stale, and come back live with its next snapshot.
func TestRoundsAcrossHosts(t *testing.T) {
	d := shortDir(t)
	a := startHab(t, d, "a")
	b := startHab(t, d, "b")
	a.PutRound(fakeRound("ra", "alpha", "2026-01-02T09:00:00Z"))
	b.PutRound(fakeRound("rb", "beta", "2026-01-01T09:00:00Z"))
	s := open(t, hosts.Spec{Label: "one", Endpoints: []hosts.Endpoint{{Kind: hosts.EndpointSocket, Value: a.Sock}}},
		hosts.Spec{Label: "two", Endpoints: []hosts.Endpoint{{Kind: hosts.EndpointSocket, Value: b.Sock}}})
	want := func(what, order string) {
		t.Helper()
		waitHosts(t, s, what, func(hs []hosts.Host) bool { return roundOrder(hs) == order })
	}
	want("both hosts' rounds, soonest first", "two/beta/no runs yet one/alpha/no runs yet")

	a.PutRound(fakeRound("ra", "alpha", "2026-01-01T08:00:00Z"))
	want("alpha moved ahead by round.updated", "one/alpha/no runs yet two/beta/no runs yet")

	b.PutRound(fakeRound("rc", "gamma", "2026-01-03T09:00:00Z"))
	waitHosts(t, s, "gamma", func(hs []hosts.Host) bool { return strings.Contains(roundOrder(hs), "gamma") })
	if _, err := hostAPI(t, s, "two").RoundRun(t.Context(), api.RoundRef{ID: "rc"}); err != nil {
		t.Fatal(err)
	}
	want("gamma's run from round.run_updated", "one/alpha/no runs yet two/beta/no runs yet two/gamma/running")

	b.Stop()
	want("two's rounds stale while it is down",
		"one/alpha/no runs yet two/beta/no runs yet/stale two/gamma/running/stale")
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	want("two live again", "one/alpha/no runs yet two/beta/no runs yet two/gamma/running")
}

func hostAPI(t *testing.T, s *hosts.Set, label string) api.Client {
	t.Helper()
	h, ok := s.Host(label)
	if !ok {
		t.Fatalf("no host %s", label)
	}
	return h.Conn.API()
}
