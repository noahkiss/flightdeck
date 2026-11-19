package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/health"
	"github.com/noahkiss/flightdeck/internal/holderreg"
	"github.com/noahkiss/flightdeck/internal/logfile"
)

// statusReport is what gff-hab status prints (diagnostics.md 4.8).
type statusReport struct {
	Hab       habStatus      `json:"hab"`
	Holders   []holderStatus `json:"holders"`
	Health    healthStatus   `json:"health"`
	Hooks     string         `json:"hooks"`
	Peers     string         `json:"peers"`
	Capcom    string         `json:"capcom"`
	Debug     string         `json:"debug"`
	Logs      string         `json:"logs"`
	LogsBytes int64          `json:"logs_bytes"`
}

type habStatus struct {
	State   string `json:"state"` // running, not answering, stopped, never started
	Pid     int    `json:"pid,omitempty"`
	Epoch   string `json:"epoch,omitempty"`
	Build   string `json:"build,omitempty"`
	Version string `json:"version,omitempty"`
	Uptime  string `json:"uptime,omitempty"`
	Started string `json:"started,omitempty"`
}

type holderStatus struct {
	Instance string `json:"instance"`
	Build    string `json:"build"`
	Protocol string `json:"protocol"`
	Uptime   string `json:"uptime"`
	Draining bool   `json:"draining,omitempty"`
}

type healthStatus struct {
	Crashes   int            `json:"crashes"`
	Unclean   int            `json:"unclean"`
	Recovered int            `json:"recovered"`
	Last      *health.Record `json:"last,omitempty"`
}

// statusCmd prints the state of this host's hab: gff-hab and its holders, the crashes and
// unclean ends of the last 7 days, the hook endpoint, peers, Capcom, debug and the log folder.
// It reads files, so it works while gff-hab is down; peers and Capcom need gff-hab to answer.
func statusCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	sock := socketFlag(fs)
	state := fs.String("state", "", "state directory (default: see gff-hab -h)")
	holders := fs.String("holders", "", "holder registry directory (default: $GFF_HOLDER_DIR, else the holder's default)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return errors.New("status takes no arguments")
	}
	dir, err := stateDir(*state)
	if err != nil {
		return err
	}
	now := time.Now()
	r := statusReport{Hooks: "unknown", Peers: "unknown", Capcom: "unknown", Debug: "off", Logs: logsDir(dir)}
	m, ok, _ := health.ReadMarker(filepath.Join(dir, runDir, habMarkerFile))
	alive := ok && !m.Clean && m.Pid > 0 && syscall.Kill(m.Pid, 0) == nil
	switch {
	case !ok:
		r.Hab.State = "never started"
	case alive:
		r.Hab.State = "not answering"
	default:
		r.Hab.State = "stopped"
	}
	if ok {
		r.Hab.Epoch, r.Hab.Build, r.Hab.Version = m.Run, m.Build, m.Version
		r.Hab.Started = m.Started.Local().Format(time.RFC3339)
		if alive {
			r.Hab.Pid = m.Pid
			r.Hab.Uptime = now.Sub(m.Started).Round(time.Second).String()
			if m.Hook != "" {
				r.Hooks = m.Hook
			}
		}
	}
	if hd := holderDirOf(*holders); hd != "" {
		es, _ := holderreg.List(hd)
		for _, e := range es {
			r.Holders = append(r.Holders, holderStatus{Instance: e.Instance, Build: e.Build,
				Protocol: fmt.Sprint(e.Protocol), Uptime: now.Sub(e.Started).Round(time.Second).String(), Draining: e.Draining})
		}
	}
	recs, _ := health.Read(filepath.Join(dir, healthFile))
	for i := range recs {
		rec := recs[i]
		if now.Sub(rec.T) > 7*24*time.Hour {
			continue
		}
		switch rec.Kind {
		case health.KindCrash:
			r.Health.Crashes++
		case health.KindUnclean:
			r.Health.Unclean++
		case health.KindRecovered:
			r.Health.Recovered++
		}
		r.Health.Last = &rec
	}
	if alive && m.Debug {
		r.Debug = "on (GFF_HAB_DEBUG)"
	}
	filepath.WalkDir(r.Logs, func(_ string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			if info, err := e.Info(); err == nil {
				r.LogsBytes += info.Size()
			}
		}
		return nil
	})
	if alive {
		statusFromHab(*sock, &r)
	}
	if *asJSON {
		return printJSON(r)
	}
	printStatus(out, r)
	return nil
}

// statusFromHab asks a running gff-hab for its peers and its Capcom state. Capcom is a state
// word only, never its reason, which can carry the engine's stderr (4.8).
func statusFromHab(sock string, r *statusReport) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := open(sock, "")
	if err != nil {
		return
	}
	defer c.Close()
	if _, err := c.Hello(ctx, cliClient, "", ""); err != nil {
		return
	}
	r.Hab.State = "running"
	cl := api.Client{Caller: c}
	if pl, err := cl.PeersList(ctx, api.PeersListParams{}); err == nil {
		on := 0
		for _, p := range pl.Peers {
			if p.State == api.PeerStateOnline {
				on++
			}
		}
		r.Peers = fmt.Sprintf("%d of %d online", on, len(pl.Peers))
	}
	if st, err := cl.CapcomStatus(ctx, api.CapcomServiceParams{}); err == nil {
		r.Capcom = string(st.State)
	} else {
		r.Capcom = "off"
	}
}

func printStatus(out io.Writer, r statusReport) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	h := r.Hab
	line := h.State
	if h.Pid != 0 {
		line += fmt.Sprintf(", pid %d, up %s", h.Pid, h.Uptime)
	}
	if h.Build != "" {
		line += fmt.Sprintf(", version %s, build %s, epoch %s", h.Version, h.Build, h.Epoch)
	}
	fmt.Fprintf(tw, "gff-hab\t%s\n", line)
	if len(r.Holders) == 0 {
		fmt.Fprintf(tw, "holders\tnone registered\n")
	}
	for i, x := range r.Holders {
		label := ""
		if i == 0 {
			label = "holders"
		}
		d := ""
		if x.Draining {
			d = ", draining"
		}
		fmt.Fprintf(tw, "%s\t%s: build %s, protocol %s, up %s%s\n", label, x.Instance, x.Build, x.Protocol, x.Uptime, d)
	}
	hl := fmt.Sprintf("last 7 days: %d crashes, %d unclean ends, %d recovered panics", r.Health.Crashes, r.Health.Unclean, r.Health.Recovered)
	fmt.Fprintf(tw, "health\t%s\n", hl)
	if l := r.Health.Last; l != nil {
		who := l.Proc
		if l.Instance != "" {
			who += " " + l.Instance
		}
		fmt.Fprintf(tw, "\tlatest: %s %s, %s: %s\n", l.T.Local().Format("2006-01-02 15:04"), who, l.Kind, l.Cause)
	}
	fmt.Fprintf(tw, "hooks\t%s\n", r.Hooks)
	fmt.Fprintf(tw, "peers\t%s\n", r.Peers)
	fmt.Fprintf(tw, "capcom\t%s\n", r.Capcom)
	fmt.Fprintf(tw, "debug\t%s\n", r.Debug)
	fmt.Fprintf(tw, "logs\t%s, %s\n", r.Logs, fmtSize(r.LogsBytes))
	tw.Flush()
}

// logSources lists the log files gff-hab logs merges: hab.log, each holder's file and hook.log
// (4.8). A holder's lines are in local time: the holder's log flags are the standard ones.
func logSources(dir string) []logfile.Source {
	logs := logsDir(dir)
	out := []logfile.Source{{Kind: "hab", Name: "hab", Path: filepath.Join(logs, habLogFile)}}
	entries, _ := os.ReadDir(logs)
	for _, e := range entries {
		inst, ok := holderInstance(e.Name())
		if ok && e.Name() == holderLogPrefix+inst+".log" {
			out = append(out, logfile.Source{Kind: "holder", Name: "holder " + inst,
				Path: filepath.Join(logs, e.Name()), Loc: time.Local})
		}
	}
	return append(out, logfile.Source{Kind: "hook", Name: "hook", Path: filepath.Join(logs, hookLogFile)})
}

var logKinds = []string{"hab", "holder", "hook"}

// logsCmd prints the hab's log files merged by time (4.8). It reads the files, so it works while
// gff-hab is down.
func logsCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	state := fs.String("state", "", "state directory (default: see gff-hab -h)")
	follow := fs.Bool("f", false, "follow: print new lines as they are written, across rotation")
	since := fs.String("since", "", "only lines from this long ago (2h) or this time (RFC 3339)")
	until := fs.String("until", "", "only lines up to this long ago (30m) or this time (RFC 3339)")
	proc := fs.String("proc", "", "only these process kinds, comma separated: "+strings.Join(logKinds, ", "))
	grep := fs.String("grep", "", "only lines that hold this text")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return errors.New("logs takes no arguments; see gff-hab logs -h")
	}
	dir, err := stateDir(*state)
	if err != nil {
		return err
	}
	now := time.Now()
	var f logfile.Filter
	if f.Since, err = parseWhen(*since, now); err != nil {
		return fmt.Errorf("-since: %w", err)
	}
	if f.Until, err = parseWhen(*until, now); err != nil {
		return fmt.Errorf("-until: %w", err)
	}
	if *proc != "" {
		for k := range strings.SplitSeq(*proc, ",") {
			k = strings.TrimSpace(k)
			if !slices.Contains(logKinds, k) {
				return fmt.Errorf("-proc %q: want one of %s", k, strings.Join(logKinds, ", "))
			}
			f.Kinds = append(f.Kinds, k)
		}
	}
	f.Grep = *grep
	print := func(e logfile.Entry) { fmt.Fprint(out, formatEntry(e)) }
	var fw *logfile.Follower
	if *follow {
		fw = logfile.NewFollower(func() []logfile.Source { return logSources(dir) }, f)
	}
	if err := logfile.Merge(logSources(dir), f, print); err != nil {
		return err
	}
	if fw == nil {
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fw.Run(ctx, 500*time.Millisecond, print)
	return nil
}

// formatEntry is one entry in local time, with its process in front (4.8).
func formatEntry(e logfile.Entry) string {
	t := "-"
	if !e.T.IsZero() {
		t = e.T.Local().Format("2006-01-02 15:04:05.000000")
	}
	return fmt.Sprintf("%s %-15s %s\n", t, e.Name, e.Text)
}

// parseWhen reads a duration ago (2h) or a time (RFC 3339). Empty is the zero time.
func parseWhen(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	return time.Parse(time.RFC3339, s)
}
