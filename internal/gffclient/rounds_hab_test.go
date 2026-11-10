package gffclient

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// TestBoardMatchesRoundLs: with the real holder and gff-hab, rounds made with the CLI show on
// gff's board exactly as `gff-hab round ls -all` lists them: the same rounds, triggers and
// marks (rounds.md 10.3).
func TestBoardMatchesRoundLs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	h := startHab(t)
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"},
		{"-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-q", "--allow-empty", "-m", "start"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	cli := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(habBin, append([]string{"round"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GFF_HAB_SOCKET="+h.sock, "GFF_HAB=")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gff-hab round %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	cli("add", "hourly", "-every", "1h", "-project", dir, "-command", "true")
	cli("add", "weekdays", "-cron", "0 9 * * 1-5", "-tz", "UTC", "-project", dir, "-command", "true")
	cli("add", "byhand", "-manual", "-project", dir, "-command", "true")
	cli("pause", "weekdays")

	type lsRow struct {
		Round api.CatalogRound `json:"round"`
	}
	var rows []lsRow
	waitUntil(t, "round ls -all with the three rounds, weekdays paused", func() bool {
		rows = nil
		if json.Unmarshal([]byte(cli("ls", "-all", "-json")), &rows) != nil || len(rows) != 3 {
			return false
		}
		for _, r := range rows {
			if r.Round.Name == "weekdays" && !r.Round.Paused {
				return false
			}
		}
		return true
	})

	hst := newHost(t, 160, 30, false)
	startWorkspace(t, hst, h.sock)
	hst.typ(cmdKey + "g")
	hst.waitScreen("the board's rounds as round ls lists them", func(s string) bool {
		lines := strings.Split(s, "\n")
		start := line(s, " Rounds")
		if start < 0 {
			return false
		}
		// Count the rows of the Rounds section only: it ends at the next section's header, such as
		// Heavy runs, whose row for host one shows when the test's disk is below the heavy-run floor.
		shown := 0
		for _, l := range lines[start+1:] {
			if !strings.HasPrefix(l, "  ") {
				break
			}
			if strings.Contains(l, " one ") {
				shown++
			}
		}
		if shown != len(rows) {
			return false
		}
		for _, r := range rows {
			l := line(s, "  "+r.Round.Name+" ")
			if l <= start {
				return false
			}
			mark := "no runs yet"
			switch {
			case r.Round.Completed:
				mark = "completed"
			case r.Round.Paused:
				mark = "paused"
			case r.Round.LastRun != "":
				mark = string(r.Round.LastRun)
			}
			if !strings.Contains(lines[l], r.Round.Trigger) || !strings.Contains(lines[l], mark) {
				t.Logf("row %q, want trigger %q and %q", lines[l], r.Round.Trigger, mark)
				return false
			}
		}
		return true
	})
}
