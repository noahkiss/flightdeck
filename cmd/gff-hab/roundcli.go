package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/rounds"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// `gff-hab round` (rounds.md 13): the round commands, through the same doors as send and show:
// -socket or -via for the control connection, else the pane's hook endpoint from $GFF_HAB,
// which makes the pane's session the creator and the meaning of --target self.

const roundUsage = `usage:
  gff-hab round ls      [door] [-json]
  gff-hab round show    [door] [-json] <name>
  gff-hab round add     [door] <name> (-cron expr [-tz zone] | -every preset | -at time | -manual)
                        [-mode fresh|resume|fork] [-project dir] [-cwd dir] [-target session|self]
                        [-harness h] [-model m] [-harness-arg a]... [-timeout d] [-max-runs n]
                        [-expires time] [-catch-up once|skip] [-on-finish close|keep]
                        [-notify failures|always|never] [-paused] [-run-now]
                        [-command] <prompt... | program args...>
  gff-hab round edit    [door] <name> [the same fields] [-command] [prompt... | program args...]
  gff-hab round rm      [door] [-yes] <name>
  gff-hab round pause | unpause | run [door] <name>
  gff-hab round runs    [door] [-limit n] [-json] <name>
  gff-hab round export  [door]
  gff-hab round apply   [door] [-yes] <file>
door: -socket path | -via command; from a pane, $GFF_HAB
`

// roundDef holds the definition flags of add and edit.
type roundDef struct {
	mode, cron, tz, every, at, project, cwd, target, harness, model string
	timeout, wait, maxRuns, expires, catchUp, onFinish, notify      string
	manual, paused, command                                         bool
	via                                                             bool // the hab is reached through -via
	harnessArgs                                                     stringList
}

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, " ") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func (d *roundDef) flags(fs *flag.FlagSet) {
	fs.StringVar(&d.mode, "mode", "", "fresh (the default for add), resume or fork")
	fs.StringVar(&d.cron, "cron", "", "a 5-field cron expression")
	fs.StringVar(&d.tz, "tz", "", "the IANA time zone of -cron or -at (default: the hab's zone)")
	fs.StringVar(&d.every, "every", "", "a preset: 1m, 5m, 15m, 1h, 1d and the like")
	fs.StringVar(&d.at, "at", "", "run once at this time: RFC 3339, or 2006-01-02T15:04 in -tz (else this host's zone)")
	fs.BoolVar(&d.manual, "manual", false, "run only by hand: gff-hab round run, the app, or the API")
	fs.StringVar(&d.project, "project", "", "fresh: the project folder (default for add: this folder)")
	fs.StringVar(&d.cwd, "cwd", "", "fresh: start in this folder instead of the project's most recent one")
	fs.StringVar(&d.target, "target", "", "resume and fork: the target session id; self from inside a pane")
	fs.StringVar(&d.harness, "harness", "", "fresh with a prompt: claude, codex or opencode")
	fs.StringVar(&d.model, "model", "", "the model for the harness")
	fs.Var(&d.harnessArgs, "harness-arg", "one argument for the harness; repeat it for more")
	fs.StringVar(&d.timeout, "timeout", "", "the longest a run may take (default 60m)")
	fs.StringVar(&d.wait, "wait", "", "resume: how long a run waits for the session to be free (default 10m)")
	fs.StringVar(&d.maxRuns, "max-runs", "", "complete the round after this many finished runs")
	fs.StringVar(&d.expires, "expires", "", "complete the round at this time (RFC 3339)")
	fs.StringVar(&d.catchUp, "catch-up", "", "once (default) or skip: what a slot missed in sleep does")
	fs.StringVar(&d.onFinish, "on-finish", "", "close (default) or keep: the run's pane after a success")
	fs.StringVar(&d.notify, "notify", "", "failures (default), always or never")
	fs.BoolVar(&d.paused, "paused", false, "add: create the round paused")
	fs.BoolVar(&d.command, "command", false, "the words after the flags are a program and its arguments, not a prompt")
}

// apply writes the flags that were given into spec. words are the words after the flags.
func (d *roundDef) apply(spec *api.RoundSpec, set map[string]bool, words []string) error {
	if set["mode"] {
		spec.Mode = api.RoundMode(d.mode)
	}
	triggers := 0
	for _, f := range []string{"cron", "every", "at", "manual"} {
		if set[f] {
			triggers++
		}
	}
	if triggers > 1 {
		return errors.New("give one trigger: -cron, -every, -at or -manual")
	}
	switch {
	case set["cron"]:
		spec.Trigger = api.RoundTrigger{Kind: api.RoundTriggerKindCron, Cron: d.cron, Zone: d.tz}
	case set["every"]:
		spec.Trigger = api.RoundTrigger{Kind: api.RoundTriggerKindCron, Every: d.every, Zone: d.tz}
	case set["at"]:
		at, err := parseRoundTime(d.at, d.tz)
		if err != nil {
			return err
		}
		spec.Trigger = api.RoundTrigger{Kind: api.RoundTriggerKindOnce, At: at, Zone: d.tz}
	case set["manual"]:
		spec.Trigger = api.RoundTrigger{Kind: api.RoundTriggerKindManual}
	case set["tz"]:
		spec.Trigger.Zone = d.tz
	}
	if set["project"] {
		if d.via && !filepath.IsAbs(d.project) {
			return fmt.Errorf("-project %q: with -via, give the absolute folder on the hab's host", d.project)
		}
		dir, err := filepath.Abs(d.project)
		if err != nil {
			return err
		}
		spec.Project, spec.ProjectRemote, spec.ProjectDir = "", "", dir
	}
	if set["cwd"] {
		dir := d.cwd
		if dir != "" {
			var err error
			if dir, err = filepath.Abs(dir); err != nil {
				return err
			}
		}
		spec.Cwd = dir
	}
	if set["target"] {
		spec.Target = d.target
	}
	if set["harness"] {
		spec.Harness = d.harness
	}
	if set["model"] {
		spec.Model = d.model
	}
	if set["harness-arg"] {
		spec.HarnessArgs = d.harnessArgs
	}
	if set["timeout"] {
		t, err := time.ParseDuration(d.timeout)
		if err != nil || t <= 0 {
			return fmt.Errorf("-timeout %q is not a duration such as 30m", d.timeout)
		}
		spec.TimeoutMS = t.Milliseconds()
	}
	if set["wait"] {
		w, err := time.ParseDuration(d.wait)
		if err != nil || w <= 0 {
			return fmt.Errorf("-wait %q is not a duration such as 10m", d.wait)
		}
		spec.WaitMS = w.Milliseconds()
	}
	if set["max-runs"] {
		n, err := strconv.Atoi(d.maxRuns)
		if err != nil || n < 0 {
			return fmt.Errorf("-max-runs %q is not a whole number", d.maxRuns)
		}
		spec.MaxRuns = n
	}
	if set["expires"] {
		at := ""
		if d.expires != "" {
			var err error
			if at, err = parseRoundTime(d.expires, d.tz); err != nil {
				return err
			}
		}
		spec.ExpiresAt = at
	}
	if set["catch-up"] {
		spec.CatchUp = api.RoundCatchUp(d.catchUp)
	}
	if set["on-finish"] {
		spec.OnFinish = api.RoundOnFinish(d.onFinish)
	}
	if set["notify"] {
		spec.Notify = api.RoundNotify(d.notify)
	}
	if set["paused"] {
		spec.Paused = d.paused
	}
	switch {
	case d.command && len(words) == 0:
		return errors.New("-command needs a program after the flags")
	case d.command:
		spec.Command, spec.Prompt = words, ""
	case len(words) > 0:
		spec.Prompt, spec.Command = strings.Join(words, " "), nil
	}
	return nil
}

// parseRoundTime reads RFC 3339, or a local time without an offset in zone (else this host's
// zone), and returns RFC 3339.
func parseRoundTime(s, zone string) (string, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format(time.RFC3339), nil
	}
	loc := time.Local
	if zone != "" {
		l, err := rounds.LoadZone(zone)
		if err != nil {
			return "", err
		}
		loc = l
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t.UTC().Format(time.RFC3339), nil
		}
	}
	return "", fmt.Errorf("time %q: use RFC 3339 or 2006-01-02T15:04", s)
}

// roundCmd is `gff-hab round`.
func roundCmd(args []string, stdin io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(roundUsage)
	}
	verb, args := args[0], args[1:]
	fs := flag.NewFlagSet("round "+verb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sock := socketFlag(fs)
	via := fs.String("via", "", "run this command and speak over its stdio, e.g. \"ssh <host> gff-hab connect\"")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	yes := fs.Bool("yes", false, "rm and apply: do not ask")
	limit := fs.Int("limit", 20, "runs: at most this many runs")
	runNow := fs.Bool("run-now", false, "add: also start one run now")
	var def roundDef
	if verb == "add" || verb == "edit" {
		def.flags(fs)
	}
	// The name may come before the flags (round add nightly -every 1d ...) or after them.
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") && verb != "apply" {
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("round %s: %v\n%s", verb, err, roundUsage)
	}
	words := fs.Args()
	if name == "" && len(words) > 0 && verb != "apply" {
		name, words = words[0], words[1:]
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	def.via = *via != ""
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	needName := func() error {
		if name == "" {
			return fmt.Errorf("round %s needs a round name", verb)
		}
		return nil
	}
	noWords := func() error {
		if len(words) > 0 {
			return fmt.Errorf("round %s: unexpected %q", verb, strings.Join(words, " "))
		}
		return nil
	}
	switch verb {
	case "ls", "export":
		if name != "" || len(words) > 0 {
			return fmt.Errorf("round %s takes no name", verb)
		}
	case "apply":
		if len(words) != 1 {
			return errors.New("round apply needs one file")
		}
	case "add", "edit":
		if err := needName(); err != nil {
			return err
		}
	case "show", "rm", "pause", "unpause", "run", "runs":
		if err := needName(); err != nil {
			return err
		}
		if err := noWords(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("round %s: unknown command\n%s", verb, roundUsage)
	}
	c, done, err := channelCaller(ctx, *sock, *via, false, "", false)
	if err != nil {
		return err
	}
	defer done()
	ref := api.RoundRef{Name: name}
	show := func(v any, text func()) error {
		if *asJSON {
			return printJSONTo(out, v)
		}
		text()
		return nil
	}
	switch verb {
	case "ls":
		var l api.RoundList
		if err := c.Call(ctx, "round.list", api.RoundListParams{}, &l); err != nil {
			return err
		}
		return show(l, func() { printRounds(out, l.Rounds) })
	case "show":
		var r api.RoundResult
		if err := c.Call(ctx, "round.get", ref, &r); err != nil {
			return err
		}
		var runs api.RoundRuns
		if err := c.Call(ctx, "round.runs", api.RoundRunsParams{RoundRef: api.RoundRef{ID: r.Round.ID}, Limit: 5}, &runs); err != nil {
			return err
		}
		return show(struct {
			Round api.Round      `json:"round"`
			Runs  []api.RoundRun `json:"runs"`
		}{r.Round, runs.Runs}, func() { printRound(out, r.Round, runs.Runs) })
	case "add":
		spec := api.RoundSpec{Name: name, Mode: api.RoundModeFresh}
		if err := def.apply(&spec, set, words); err != nil {
			return err
		}
		if spec.Mode == api.RoundModeFresh && !set["project"] {
			if *via != "" {
				// This folder is on this machine, not on the hab's host.
				return errors.New("round add with -via needs -project: the project folder on the hab's host")
			}
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			spec.ProjectDir = wd
		}
		var made api.RoundCreated
		if err := c.Call(ctx, "round.create", api.RoundCreateParams{Round: spec, RunNow: *runNow}, &made); err != nil {
			return err
		}
		return show(made, func() {
			fmt.Fprintf(out, "%s %s next %s\n", made.Round.ID, made.Round.Name, orDash(made.Round.NextSlot))
			if made.Run != nil {
				fmt.Fprintf(out, "run %d %s %s\n", made.Run.Number, made.Run.ID, made.Run.State)
			}
		})
	case "edit":
		if set["paused"] {
			return errors.New("round edit does not pause: use gff-hab round pause or gff-hab round unpause")
		}
		var r api.RoundResult
		if err := c.Call(ctx, "round.get", ref, &r); err != nil {
			return err
		}
		spec := r.Round.RoundSpec
		spec.ProjectRemote = "" // read only here: the project id stays
		if err := def.apply(&spec, set, words); err != nil {
			return err
		}
		var res api.RoundResult
		if err := c.Call(ctx, "round.update", api.RoundUpdateParams{ID: r.Round.ID, Round: spec, Revision: r.Round.Revision}, &res); err != nil {
			return err
		}
		return show(res, func() { fmt.Fprintf(out, "%s %s next %s\n", res.Round.ID, res.Round.Name, orDash(res.Round.NextSlot)) })
	case "rm":
		if !*yes && !askYes(stdin, out, fmt.Sprintf("Delete round %s and its ended runs? A live run goes on to its end. [y/N] ", name)) {
			return errors.New("not deleted")
		}
		if err := c.Call(ctx, "round.delete", ref, nil); err != nil {
			return err
		}
		fmt.Fprintf(out, "deleted %s\n", name)
		return nil
	case "pause", "unpause":
		var r api.RoundResult
		if err := c.Call(ctx, "round."+verb, ref, &r); err != nil {
			return err
		}
		return show(r, func() { fmt.Fprintf(out, "%s %s\n", r.Round.Name, roundStatus(r.Round)) })
	case "run":
		var r api.RoundRunResult
		if err := c.Call(ctx, "round.run", ref, &r); err != nil {
			return err
		}
		return show(r, func() { fmt.Fprintf(out, "run %d %s %s\n", r.Run.Number, r.Run.ID, r.Run.State) })
	case "runs":
		var runs api.RoundRuns
		if err := c.Call(ctx, "round.runs", api.RoundRunsParams{RoundRef: ref, Limit: *limit}, &runs); err != nil {
			return err
		}
		return show(runs, func() { printRuns(out, runs.Runs) })
	case "export":
		var l api.RoundList
		if err := c.Call(ctx, "round.list", api.RoundListParams{}, &l); err != nil {
			return err
		}
		b, err := exportRounds(l.Rounds)
		if err != nil {
			return err
		}
		_, err = out.Write(b)
		return err
	default: // apply
		return applyRounds(ctx, c, words[0], *yes, stdin, out)
	}
}

func printJSONTo(out io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\n", b)
	return err
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func askYes(in io.Reader, out io.Writer, q string) bool {
	return askLine(bufio.NewReader(in), out, q)
}

// askLine asks q and reads one answer line from in, which may be asked again.
func askLine(in *bufio.Reader, out io.Writer, q string) bool {
	fmt.Fprint(out, q)
	line, _ := in.ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

func triggerText(t api.RoundTrigger) string {
	switch t.Kind {
	case api.RoundTriggerKindCron:
		return fmt.Sprintf("cron %q %s", t.Cron, t.Zone)
	case api.RoundTriggerKindOnce:
		return "once " + t.At
	}
	return string(t.Kind)
}

func roundStatus(r api.Round) string {
	switch {
	case r.Completed:
		return "completed: " + r.CompletedReason
	case r.Paused && r.PausedReason != "":
		return "paused: " + r.PausedReason
	case r.Paused:
		return "paused"
	case r.TriggerError != "":
		return "not scheduled: " + r.TriggerError
	}
	return "active"
}

func printRounds(out io.Writer, rs []api.Round) {
	tw := tabwriter.NewWriter(out, 2, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tMODE\tTRIGGER\tNEXT\tLAST RUN\tSTATUS")
	for _, r := range rs {
		last := "-"
		if r.LastRun != nil {
			last = fmt.Sprintf("%d %s", r.LastRun.Number, r.LastRun.State)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.Mode, triggerText(r.Trigger), orDash(r.NextSlot), last, roundStatus(r))
	}
	tw.Flush()
}

func printRound(out io.Writer, r api.Round, runs []api.RoundRun) {
	tw := tabwriter.NewWriter(out, 2, 8, 2, ' ', 0)
	row := func(k, v string) {
		if v != "" {
			fmt.Fprintf(tw, "%s\t%s\n", k, v)
		}
	}
	row("name", r.Name)
	row("id", r.ID)
	row("mode", string(r.Mode))
	row("trigger", triggerText(r.Trigger))
	row("status", roundStatus(r))
	row("prompt", r.Prompt)
	if len(r.Command) > 0 {
		row("command", strings.Join(r.Command, " "))
	}
	row("harness", r.Harness)
	row("model", r.Model)
	row("project", r.Project)
	row("remote", r.ProjectRemote)
	row("cwd", r.Cwd)
	row("target", r.Target)
	row("timeout", (time.Duration(r.TimeoutMS) * time.Millisecond).String())
	if r.WaitMS > 0 {
		row("wait", (time.Duration(r.WaitMS) * time.Millisecond).String())
	}
	if r.MaxRuns > 0 {
		row("max runs", strconv.Itoa(r.MaxRuns))
	}
	row("expires", r.ExpiresAt)
	row("catch up", string(r.CatchUp))
	row("on finish", string(r.OnFinish))
	row("notify", string(r.Notify))
	row("created by", r.CreatedBy)
	row("last ok", r.LastOK)
	if next := nextSlots(r, 3); len(next) > 0 {
		row("next", strings.Join(next, ", "))
	}
	tw.Flush()
	if len(runs) > 0 {
		fmt.Fprintln(out)
		printRuns(out, runs)
	}
}

// nextSlots computes a round's next n slots from now, in UTC, for show.
func nextSlots(r api.Round, n int) []string {
	if r.Paused || r.Completed {
		return nil
	}
	t := rounds.Trigger{Kind: rounds.TriggerKind(r.Trigger.Kind), Cron: r.Trigger.Cron, Zone: r.Trigger.Zone}
	if r.Trigger.At != "" {
		at, err := time.Parse(time.RFC3339, r.Trigger.At)
		if err != nil {
			return nil
		}
		t.At = &at
	}
	sched, err := t.Schedule()
	if err != nil {
		return nil
	}
	var out []string
	for _, s := range sched.NextN(time.Now(), n) {
		out = append(out, s.UTC().Format(time.RFC3339))
	}
	return out
}

func printRuns(out io.Writer, runs []api.RoundRun) {
	tw := tabwriter.NewWriter(out, 2, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tSTATE\tSLOT\tSTARTED\tENDED\tSESSION\tREASON")
	for _, r := range runs {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Number, r.State, orDash(r.Slot), orDash(r.StartedAt), orDash(r.EndedAt), orDash(r.Session), r.Reason)
	}
	tw.Flush()
}

// --- Export and apply (rounds.md 3: a file for version control) ---

// fileRound is one round in an exported file. It names its project by remote, so the file works
// on any host that has the project; a project with no remote is named by its id.
type fileRound struct {
	Name          string   `toml:"name"`
	Mode          string   `toml:"mode"`
	Cron          string   `toml:"cron,omitempty"`
	Every         string   `toml:"every,omitempty"`
	At            string   `toml:"at,omitempty"`
	Manual        bool     `toml:"manual,omitempty"`
	Zone          string   `toml:"zone,omitempty"`
	Prompt        string   `toml:"prompt,omitempty"`
	Harness       string   `toml:"harness,omitempty"`
	Model         string   `toml:"model,omitempty"`
	HarnessArgs   []string `toml:"harness_args,omitempty"`
	Command       []string `toml:"command,omitempty"`
	ProjectRemote string   `toml:"project_remote,omitempty"`
	Project       string   `toml:"project,omitempty"`
	Cwd           string   `toml:"cwd,omitempty"`
	Target        string   `toml:"target,omitempty"`
	MaxRuns       int      `toml:"max_runs,omitempty"`
	ExpiresAt     string   `toml:"expires_at,omitempty"`
	Timeout       string   `toml:"timeout,omitempty"`
	Wait          string   `toml:"wait,omitempty"`
	CatchUp       string   `toml:"catch_up,omitempty"`
	OnFinish      string   `toml:"on_finish,omitempty"`
	Notify        string   `toml:"notify,omitempty"`
	Paused        bool     `toml:"paused,omitempty"`
}

type roundsFile struct {
	Rounds []fileRound `toml:"rounds"`
}

func toFile(r api.Round) fileRound {
	f := fileRound{
		Name: r.Name, Mode: string(r.Mode), Zone: r.Trigger.Zone, Prompt: r.Prompt, Harness: r.Harness,
		Model: r.Model, HarnessArgs: r.HarnessArgs, Command: r.Command, Cwd: r.Cwd, Target: r.Target,
		MaxRuns: r.MaxRuns, ExpiresAt: r.ExpiresAt, CatchUp: string(r.CatchUp), OnFinish: string(r.OnFinish),
		Notify: string(r.Notify), Paused: r.Paused,
	}
	switch r.Trigger.Kind {
	case api.RoundTriggerKindCron:
		f.Cron = r.Trigger.Cron
	case api.RoundTriggerKindOnce:
		f.At = r.Trigger.At
	case api.RoundTriggerKindManual:
		f.Manual = true
	}
	if r.ProjectRemote != "" {
		f.ProjectRemote = r.ProjectRemote
	} else {
		f.Project = r.Project
	}
	if r.TimeoutMS > 0 {
		f.Timeout = (time.Duration(r.TimeoutMS) * time.Millisecond).String()
	}
	if r.WaitMS > 0 {
		f.Wait = (time.Duration(r.WaitMS) * time.Millisecond).String()
	}
	return f
}

// exportRounds prints the rounds as TOML, by name.
func exportRounds(rs []api.Round) ([]byte, error) {
	f := roundsFile{Rounds: []fileRound{}}
	for _, r := range rs {
		f.Rounds = append(f.Rounds, toFile(r))
	}
	sort.Slice(f.Rounds, func(i, j int) bool { return f.Rounds[i].Name < f.Rounds[j].Name })
	var b bytes.Buffer
	b.WriteString("# Rounds exported by gff-hab round export. Apply with gff-hab round apply <file>.\n\n")
	if err := toml.NewEncoder(&b).Encode(f); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// normalize fills a file round's defaults and puts its values in one form, so a round from a
// file and the same round read from the hab compare equal. have is the hab's round of that
// name, or nil: a file round with no zone takes its zone.
func (f fileRound) normalize(have *fileRound) (fileRound, error) {
	if f.Every != "" {
		if f.Cron != "" {
			return f, fmt.Errorf("round %s: give cron or every, not both", f.Name)
		}
		c, err := rounds.PresetCron(f.Every)
		if err != nil {
			return f, fmt.Errorf("round %s: %v", f.Name, err)
		}
		f.Cron, f.Every = c, ""
	}
	if f.Mode == "" {
		f.Mode = "fresh"
	}
	if f.Zone == "" && have != nil && (f.Cron != "" || f.At != "") {
		f.Zone = have.Zone
	}
	for _, p := range []*string{&f.At, &f.ExpiresAt} {
		if *p != "" {
			t, err := time.Parse(time.RFC3339, *p)
			if err != nil {
				return f, fmt.Errorf("round %s: %q is not an RFC 3339 time", f.Name, *p)
			}
			*p = t.UTC().Format(time.RFC3339)
		}
	}
	dur := func(p *string, def time.Duration) error {
		if *p == "" {
			if def > 0 {
				*p = def.String()
			}
			return nil
		}
		d, err := time.ParseDuration(*p)
		if err != nil || d <= 0 {
			return fmt.Errorf("round %s: %q is not a duration", f.Name, *p)
		}
		*p = d.String()
		return nil
	}
	if err := dur(&f.Timeout, rounds.DefaultTimeout); err != nil {
		return f, err
	}
	wdef := time.Duration(0)
	if f.Mode == "resume" {
		wdef = rounds.DefaultWait
	}
	if err := dur(&f.Wait, wdef); err != nil {
		return f, err
	}
	if f.CatchUp == "" {
		f.CatchUp = "once"
	}
	if f.Notify == "" {
		f.Notify = "failures"
	}
	if f.OnFinish == "" && f.Mode != "resume" {
		f.OnFinish = "close"
	}
	if f.ProjectRemote != "" {
		f.ProjectRemote = structure.NormalizeRemote(f.ProjectRemote)
	}
	return f, nil
}

func (f fileRound) spec() (api.RoundSpec, error) {
	s := api.RoundSpec{
		Name: f.Name, Mode: api.RoundMode(f.Mode), Prompt: f.Prompt, Harness: f.Harness, Model: f.Model,
		HarnessArgs: f.HarnessArgs, Command: f.Command, Project: f.Project, ProjectRemote: f.ProjectRemote,
		Cwd: f.Cwd, Target: f.Target, MaxRuns: f.MaxRuns, ExpiresAt: f.ExpiresAt, CatchUp: api.RoundCatchUp(f.CatchUp),
		OnFinish: api.RoundOnFinish(f.OnFinish), Notify: api.RoundNotify(f.Notify), Paused: f.Paused,
	}
	kinds := 0
	for _, b := range []bool{f.Cron != "", f.At != "", f.Manual} {
		if b {
			kinds++
		}
	}
	if kinds != 1 {
		return s, fmt.Errorf("round %s: give one trigger: cron, every, at or manual", f.Name)
	}
	switch {
	case f.Cron != "":
		s.Trigger = api.RoundTrigger{Kind: api.RoundTriggerKindCron, Cron: f.Cron, Zone: f.Zone}
	case f.At != "":
		s.Trigger = api.RoundTrigger{Kind: api.RoundTriggerKindOnce, At: f.At, Zone: f.Zone}
	default:
		s.Trigger = api.RoundTrigger{Kind: api.RoundTriggerKindManual}
	}
	for _, d := range []struct {
		v string
		p *int64
	}{{f.Timeout, &s.TimeoutMS}, {f.Wait, &s.WaitMS}} {
		if d.v != "" {
			t, err := time.ParseDuration(d.v)
			if err != nil {
				return s, fmt.Errorf("round %s: %q is not a duration", f.Name, d.v)
			}
			*d.p = t.Milliseconds()
		}
	}
	return s, nil
}

// fieldDiff lists the fields where two normalized file rounds differ, as "field: old -> new".
func fieldDiff(a, b fileRound) []string {
	var out []string
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	t := va.Type()
	for i := 0; i < t.NumField(); i++ {
		x, y := va.Field(i).Interface(), vb.Field(i).Interface()
		if reflect.DeepEqual(x, y) || (isEmpty(x) && isEmpty(y)) {
			continue
		}
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("toml"), ",")
		out = append(out, fmt.Sprintf("%s: %s -> %s", name, show(x), show(y)))
	}
	return out
}

func isEmpty(v any) bool {
	rv := reflect.ValueOf(v)
	return rv.IsZero() || (rv.Kind() == reflect.Slice && rv.Len() == 0)
}

func show(v any) string {
	if isEmpty(v) {
		return "(none)"
	}
	if s, ok := v.([]string); ok {
		return strconv.Quote(strings.Join(s, " "))
	}
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	return fmt.Sprint(v)
}

// roundChange is one step of an apply.
type roundChange struct {
	op   string // create, update, pause, unpause, delete
	have *api.Round
	want fileRound
	diff []string
}

// planApply compares a file with the hab's rounds: a round only in the file is created, one in
// both that differs is updated (a changed paused field pauses or unpauses it), and one only on
// the hab is deleted.
func planApply(file roundsFile, have []api.Round) ([]roundChange, error) {
	byName := map[string]*api.Round{}
	for i := range have {
		byName[have[i].Name] = &have[i]
	}
	seen := map[string]bool{}
	var plan []roundChange
	for _, f := range file.Rounds {
		if f.Name == "" {
			return nil, errors.New("a round in the file has no name")
		}
		if seen[f.Name] {
			return nil, fmt.Errorf("round %s is in the file twice", f.Name)
		}
		seen[f.Name] = true
		h := byName[f.Name]
		if h == nil {
			want, err := f.normalize(nil)
			if err != nil {
				return nil, err
			}
			plan = append(plan, roundChange{op: "create", want: want})
			continue
		}
		hf, err := toFile(*h).normalize(nil)
		if err != nil {
			return nil, err
		}
		want, err := f.normalize(&hf)
		if err != nil {
			return nil, err
		}
		if want.Project != "" && want.ProjectRemote == "" && hf.ProjectRemote != "" && h.Project == want.Project {
			want.ProjectRemote, want.Project = hf.ProjectRemote, ""
		}
		wantDef, hfDef := want, hf
		wantDef.Paused, hfDef.Paused = false, false
		if d := fieldDiff(hfDef, wantDef); len(d) > 0 {
			plan = append(plan, roundChange{op: "update", have: h, want: want, diff: d})
		}
		if want.Paused != hf.Paused {
			op := "unpause"
			if want.Paused {
				op = "pause"
			}
			plan = append(plan, roundChange{op: op, have: h, want: want})
		}
	}
	for i := range have {
		if !seen[have[i].Name] {
			plan = append(plan, roundChange{op: "delete", have: &have[i]})
		}
	}
	return plan, nil
}

func printPlan(out io.Writer, plan []roundChange) {
	for _, ch := range plan {
		switch ch.op {
		case "create":
			fmt.Fprintf(out, "+ create %s\n", ch.want.Name)
		case "delete":
			fmt.Fprintf(out, "- delete %s\n", ch.have.Name)
		case "update":
			fmt.Fprintf(out, "~ update %s\n", ch.have.Name)
			for _, d := range ch.diff {
				fmt.Fprintf(out, "    %s\n", d)
			}
		default:
			fmt.Fprintf(out, "~ %s %s\n", ch.op, ch.have.Name)
		}
	}
}

// applyRounds makes the hab's rounds match a file, after it prints the diff and the user
// confirms (rounds.md 3).
func applyRounds(ctx context.Context, c caller, path string, yes bool, stdin io.Reader, out io.Writer) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var file roundsFile
	md, err := toml.Decode(string(b), &file)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		return fmt.Errorf("%s: unknown key %s", path, und[0])
	}
	readPlan := func() ([]roundChange, string, error) {
		var l api.RoundList
		if err := c.Call(ctx, "round.list", api.RoundListParams{}, &l); err != nil {
			return nil, "", err
		}
		plan, err := planApply(file, l.Rounds)
		if err != nil {
			return nil, "", err
		}
		var b bytes.Buffer
		printPlan(&b, plan)
		return plan, b.String(), nil
	}
	plan, text, err := readPlan()
	if err != nil {
		return err
	}
	in := bufio.NewReader(stdin)
	for {
		if len(plan) == 0 {
			fmt.Fprintln(out, "no changes")
			return nil
		}
		fmt.Fprint(out, text)
		if yes {
			break
		}
		if !askLine(in, out, "Apply these changes? [y/N] ") {
			return errors.New("nothing applied")
		}
		// The rounds may have changed while the user read the plan: a run ended, a slot
		// passed, another client edited one. Apply only the plan the user saw, with the
		// revisions read now; a different plan is shown and asked again.
		again, againText, err := readPlan()
		if err != nil {
			return err
		}
		plan = again
		if againText == text {
			break
		}
		text = againText
		fmt.Fprintln(out, "The rounds changed while you answered. The plan now:")
	}
	for _, ch := range plan {
		var err error
		switch ch.op {
		case "create", "update":
			spec, serr := ch.want.spec()
			if serr != nil {
				return serr
			}
			if ch.op == "create" {
				err = c.Call(ctx, "round.create", api.RoundCreateParams{Round: spec}, nil)
			} else {
				spec.Paused = ch.have.Paused // pause and unpause are their own steps
				err = c.Call(ctx, "round.update", api.RoundUpdateParams{ID: ch.have.ID, Round: spec, Revision: ch.have.Revision}, nil)
			}
		case "delete":
			err = c.Call(ctx, "round.delete", api.RoundRef{ID: ch.have.ID}, nil)
		default:
			err = c.Call(ctx, "round."+ch.op, api.RoundRef{ID: ch.have.ID}, nil)
		}
		if err != nil {
			name := ch.want.Name
			if ch.have != nil {
				name = ch.have.Name
			}
			return fmt.Errorf("%s %s: %w", ch.op, name, err)
		}
	}
	fmt.Fprintf(out, "applied %d changes\n", len(plan))
	return nil
}
