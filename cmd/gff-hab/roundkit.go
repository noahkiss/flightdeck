package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/confirm"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/rounds"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// Rounds in the agent kit (rounds.md 12 Q1, 13, stage 10): the round tools of the MCP server and
// the manual, and a round an agent makes on another host. A round on the agent's own hab is
// made at once through the pane route, with no card. A round on another host goes through
// confirm.start_elsewhere like any start there: with the setting on, the owner confirms it
// first; with it off, it is made at once and its card is a record.

// roundsCapability is the capability word of a hab that serves round.*.
const roundsCapability = "rounds"

var _ confirm.RoundRunner = (*confirmRunner)(nil)

// CheckRound checks a round on another host and fills in its trigger words and project.
func (r *confirmRunner) CheckRound(c api.ConfirmRound) (api.ConfirmRound, error) {
	if r.self(c.Host) {
		return c, control.Errorf(api.ErrorKindInvalid, "a round on this hab needs no confirmation: make it with round.create (gff round add without -host)")
	}
	s := &c.Round
	if s.Mode == "" {
		s.Mode = api.RoundModeFresh
	}
	switch {
	case s.Name == "":
		return c, control.Errorf(api.ErrorKindInvalid, "a round needs a name")
	case s.Mode == api.RoundModeResume || s.Mode == api.RoundModeFork || s.Target != "":
		// From a pane a round may target only the calling session, which lives on this hab.
		return c, control.Errorf(api.ErrorKindInvalid, "a round on another host is not a resume or fork round: its target is a session on this hab, so make it here")
	case len(s.Prompt) > rounds.MaxPromptBytes:
		return c, control.Errorf(api.ErrorKindInvalid, "prompt: %d bytes; the limit is %d", len(s.Prompt), rounds.MaxPromptBytes)
	}
	l, ok := r.link(c.Host)
	switch {
	case !ok:
		return c, control.Errorf(api.ErrorKindNotFound, "no peer labelled %q; the hosts tool lists them", c.Host)
	case !l.Online || l.Caller == nil:
		return c, control.Errorf(api.ErrorKindUnavailable, "%s: %s is offline, or only its own link to this hab is up; rounds never queue for a sleeping host", errHostOffline, c.Host)
	case !slices.Contains(l.Caps, roundsCapability):
		return c, control.Errorf(api.ErrorKindUnavailable, "%s runs a gff-hab without rounds", c.Host)
	}
	t := s.Trigger
	cron := t.Cron
	if t.Every != "" {
		var err error
		if cron, err = rounds.PresetCron(t.Every); err != nil {
			return c, control.Errorf(api.ErrorKindInvalid, "every: %v", err)
		}
	}
	c.Trigger = rounds.TriggerWords(string(t.Kind), cron, t.Zone, t.At, t.Event)
	c.Project = r.peerProject(c.Host, s.ProjectRemote, s.ProjectDir)
	return c, nil
}

// peerProject is the name of the project a peer's catalog lists by remote or by folder, or "".
func (r *confirmRunner) peerProject(host, remote, dir string) string {
	if r.catalog == nil || (remote == "" && dir == "") {
		return ""
	}
	if remote != "" {
		remote = structure.NormalizeRemote(remote)
	}
	for _, h := range r.catalog().Hosts {
		if h.Own || h.Label != host {
			continue
		}
		for _, p := range h.Catalog.Projects {
			if remote != "" && p.Remote == remote {
				return p.Name
			}
			for _, f := range p.Folders {
				if dir != "" && filepath.Clean(f.Path) == filepath.Clean(dir) {
					return p.Name
				}
			}
		}
	}
	return ""
}

// RunRound makes the round on its runner with round.create over the link this hab dialed. The
// runner records no creator: the round is the owner's, confirmed or recorded here.
func (r *confirmRunner) RunRound(ctx context.Context, id, from string, c api.ConfirmRound) (api.ConfirmResult, error) {
	l, ok := r.link(c.Host)
	if !ok || !l.Online || l.Caller == nil {
		return api.ConfirmResult{}, control.Errorf(api.ErrorKindUnavailable, "%s: %s went offline before the round was sent; nothing was made", errHostOffline, c.Host)
	}
	var made api.RoundCreated
	if err := l.Caller.Call(ctx, "round.create", api.RoundCreateParams{Round: c.Round, RunNow: c.RunNow}, &made); err != nil {
		var ce *control.Error
		if errors.As(err, &ce) {
			return api.ConfirmResult{}, control.Errorf(ce.Data.Kind, "%s refused the round: %s", c.Host, strings.TrimPrefix(ce.Message, "gff-hab: "))
		}
		return api.ConfirmResult{}, fmt.Errorf("%w: the answer from %s was lost (%v); a round named %s may be there", confirm.ErrUnknown, c.Host, err, c.Round.Name)
	}
	return api.ConfirmResult{Host: c.Host, Name: made.Round.Name, Round: made.Round.ID}, nil
}

// roundElsewhere asks the hab behind c for a round on another host and waits up to startWait for
// the owner's answer.
func roundElsewhere(ctx context.Context, c caller, host string, spec api.RoundSpec, runNow bool) (api.Confirmation, error) {
	p := api.ConfirmRequestParams{Kind: "round", WaitMS: int(startWait / time.Millisecond),
		Round: &api.ConfirmRound{Host: host, Round: spec, RunNow: runNow}}
	var r api.ConfirmRequested
	if err := c.Call(ctx, "confirm.request", p, &r); err != nil {
		return api.Confirmation{}, err
	}
	return r.Confirmation, nil
}

// roundConfirmationText says where a round's confirmation stands, for an agent.
func roundConfirmationText(c api.Confirmation) string {
	what := "the round"
	if r := c.Round; r != nil {
		what = fmt.Sprintf("round %s (%s) on %s", r.Round.Name, orDash(r.Trigger), r.Host)
	}
	switch c.State {
	case "open":
		return fmt.Sprintf("Still waiting for the owner to confirm %s. Confirmation %s. Do not ask again: the answer reaches you as a message from Flightdeck, and confirmation_read shows where it stands.\n", what, c.ID)
	case "running":
		return fmt.Sprintf("The owner confirmed %s; it is being made now. Confirmation %s.\n", what, c.ID)
	case "done":
		how := "after the owner confirmed it"
		if c.Record {
			how = "at once, with no confirmation asked"
		}
		id := ""
		if c.Result != nil {
			id = " (" + c.Result.Round + ")"
		}
		return fmt.Sprintf("Made %s%s, %s. Confirmation %s.\n", what, id, how, c.ID)
	case "cancelled":
		return fmt.Sprintf("The owner cancelled %s. Nothing was made. Confirmation %s.\n", what, c.ID)
	case "expired":
		return fmt.Sprintf("No answer within 60 minutes: %s expired and nothing was made. Confirmation %s.\n", what, c.ID)
	case "unknown":
		return fmt.Sprintf("The outcome of %s is unknown: %s. Check round_ls with all before you offer a retry. Confirmation %s.\n", what, c.Reason, c.ID)
	}
	return fmt.Sprintf("%s was not made (%s): %s. Confirmation %s.\n", strings.ToUpper(what[:1])+what[1:], c.State, c.Reason, c.ID)
}

// roundConfirmationErr is the error of a round's confirmation that made nothing, for the CLI's
// exit status; nil while it waits or once it is made.
func roundConfirmationErr(c api.Confirmation) error {
	switch c.State {
	case "open", "running", "done":
		return nil
	}
	return fmt.Errorf("round %s on %s: %s", c.Round.Round.Name, c.Round.Host, c.State)
}

// addElsewhere is `round add -host` from a pane: the round goes to the owner's confirmation.
func addElsewhere(ctx context.Context, c caller, spec api.RoundSpec, host string, runNow, asJSON bool, out io.Writer) error {
	if spec.Mode == api.RoundModeFresh && spec.Project == "" && spec.ProjectDir == "" && spec.ProjectRemote == "" {
		remote, err := localRemote(ctx, c)
		if err != nil {
			return err
		}
		spec.ProjectRemote = remote
	}
	cf, err := roundElsewhere(ctx, c, host, spec, runNow)
	if err != nil {
		return err
	}
	if asJSON {
		if err := printJSONTo(out, api.ConfirmRequested{Confirmation: cf}); err != nil {
			return err
		}
	} else {
		io.WriteString(out, roundConfirmationText(cf))
	}
	return roundConfirmationErr(cf)
}

// --- The round tools (rounds.md 13: the agent kit's MCP tools with the CLI's verbs) ---

// roundFieldArgs are the definition arguments of round_add and round_edit, named as the CLI's
// flags with _ for -.
var roundFieldArgs = []toolArg{
	{Name: "mode", Type: "string", Doc: "fresh (the default for add): a new session each run; resume: your own session gets the prompt (target self); fork: a new session from your conversation; capcom: each run messages Capcom; group: each run messages a group's coordinator."},
	{Name: "on", Type: "string", Doc: "Run on this host's events instead of a schedule: project.updated, agent.state, command.updated, hab.woke, notice.raised or logbook.updated."},
	{Name: "where", Type: "array", Doc: "With on: filter fields, each key=value, a list comma-separated (status=done,dropped)."},
	{Name: "debounce", Type: "string", Doc: "With on: how long a batch gathers events after its first. Absent: 30s."},
	{Name: "cooldown", Type: "string", Doc: "With on: the least time between two event runs' starts. Absent: 5m."},
	{Name: "group", Type: "string", Doc: "Mode group: the group id whose coordinator gets each run's message."},
	{Name: "every", Type: "string", Doc: "A preset: 1m, 5m, 10m, 15m, 30m, 1h, 2h, 6h, 12h, 1d and the like."},
	{Name: "cron", Type: "string", Doc: "A 5-field cron expression, instead of every."},
	{Name: "tz", Type: "string", Doc: "The IANA time zone of cron or at. Absent: the runner's zone."},
	{Name: "at", Type: "string", Doc: "Run once at this time: RFC 3339, or 2006-01-02T15:04 in tz."},
	{Name: "manual", Type: "boolean", Doc: "Run only by hand (round_run)."},
	{Name: "prompt", Type: "string", Doc: "The text the agent gets each run, at most 64 KiB."},
	{Name: "command", Type: "array", Doc: "Fresh only: a program and its arguments to run instead of a harness and a prompt. Exit 0 is a success."},
	{Name: "harness", Type: "string", Doc: "Fresh with a prompt: claude, codex or opencode."},
	{Name: "model", Type: "string", Doc: "The model for the harness."},
	{Name: "harness_args", Type: "array", Doc: "Arguments for the harness at start."},
	{Name: "project", Type: "string", Doc: "Fresh: the project folder. On another host, its absolute folder there. Absent for add: this folder here, or this folder's project by its remote on another host."},
	{Name: "remote", Type: "string", Doc: "Fresh: the project by its git remote, instead of project."},
	{Name: "cwd", Type: "string", Doc: "Fresh: the folder to start in, instead of the project's most recent one."},
	{Name: "target", Type: "string", Doc: "Resume and fork: self, your own session. A round may target no other session."},
	{Name: "timeout", Type: "string", Doc: "The longest a run may take, such as 30m. Absent: 60m."},
	{Name: "wait", Type: "string", Doc: "Resume and group: how long a run waits for your session to be free, or for its delivery. Absent: 10m."},
	{Name: "max_runs", Type: "integer", Doc: "Complete the round after this many finished runs."},
	{Name: "expires", Type: "string", Doc: "Complete the round at this RFC 3339 time."},
	{Name: "catch_up", Type: "string", Doc: "once (the default) or skip: what a slot missed while the host slept does."},
	{Name: "on_finish", Type: "string", Doc: "close (the default) or keep: the run's pane after a success."},
	{Name: "notify", Type: "string", Doc: "failures (the default), always or never."},
}

var roundNameArg = toolArg{Name: "name", Type: "string", Doc: "The round's name, or its id ($GFF_ROUND in a run).", Required: true}

func roundRefTool(verb, doc string) kitTool {
	return kitTool{Name: "round_" + verb, Action: true, Doc: doc, Args: []toolArg{roundNameArg},
		Shell: "gff round " + verb + " <name>", run: func(ctx context.Context, c caller, a toolArgs) (any, string, error) {
			return runRoundRef(ctx, c, verb, a)
		}}
}

var roundTools = []kitTool{
	{Name: "round_ls", Doc: "List rounds, Flightdeck's scheduled jobs: this host's, every host's, or one host's. Each row has the trigger, the next run, the last run's state and the status.",
		Args: []toolArg{{Name: "all", Type: "boolean", Doc: "Every host's rounds, from the catalog; a stale host says since when."},
			{Name: "host", Type: "string", Doc: "Only the rounds of this host: a label from the hosts tool."}},
		Shell: "gff round ls [-all]", run: runRoundLs},
	{Name: "round_show", Doc: "Show one round on this host: its definition, next three run times and last runs.",
		Args: []toolArg{roundNameArg}, Shell: "gff round show <name>", run: runRoundShow},
	{Name: "round_runs", Doc: "A round's run history on this host, newest first: state, slot, times, session and reason.",
		Args:  []toolArg{roundNameArg, {Name: "limit", Type: "integer", Doc: "At most this many runs. Absent: 20."}},
		Shell: "gff round runs <name>", run: runRoundRuns},
	{Name: "round_add", Action: true,
		Doc: "Make a round: a job Flightdeck runs on a schedule, on events, by hand, or once. On this host it is made at once, with you as its creator. On another host (host) it goes through the owner's confirmation setting, as start_session does: with it on, the owner confirms it in Flightdeck first; the tool waits up to 45 s, then says it is still waiting, and the answer reaches you as a message. Give one trigger: every, cron, at, on or manual. Give a prompt, or a command.",
		Args: append(append([]toolArg{roundNameArg}, roundFieldArgs...),
			toolArg{Name: "paused", Type: "boolean", Doc: "Make the round paused."},
			toolArg{Name: "run_now", Type: "boolean", Doc: "Also start one run now."},
			toolArg{Name: "host", Type: "string", Doc: "Make the round on this host instead: a label from the hosts tool. Not a resume or fork round."}),
		Shell: "gff round add <name> (-every <preset> | -cron <expr> | -at <time> | -manual) [-host <label>] <prompt>", run: runRoundAdd},
	{Name: "round_edit", Action: true, Doc: "Change fields of a round you made on this host. Fields left out keep their value. round_pause and round_unpause change the pause.",
		Args:  append([]toolArg{roundNameArg}, roundFieldArgs...),
		Shell: "gff round edit <name> [fields] [prompt]", run: runRoundEdit},
	roundRefTool("rm", "Delete a round you made on this host, with its ended runs. A live run goes on to its end. In a run of the round, $GFF_ROUND names it."),
	roundRefTool("pause", "Pause a round you made on this host: it starts no runs until round_unpause."),
	roundRefTool("unpause", "Unpause a round you made on this host."),
	roundRefTool("run", "Start one run of a round you made on this host now. It does not move the next slot."),
}

func (a toolArgs) boolean(name string) bool {
	switch v := a[name].(type) {
	case bool:
		return v
	case string:
		b, _ := strconv.ParseBool(v)
		return b
	}
	return false
}

func (a toolArgs) list(name string) []string {
	switch v := a[name].(type) {
	case []any:
		var out []string
		for _, x := range v {
			out = append(out, fmt.Sprint(x))
		}
		return out
	case []string:
		return v
	}
	return nil
}

// roundSpecArgs writes a tool call's definition arguments into spec, as the CLI's flags would.
// It returns whether a project was named.
func roundSpecArgs(spec *api.RoundSpec, a toolArgs, elsewhere bool) (bool, error) {
	// A null argument is no argument: clients often send null for an optional field.
	given := toolArgs{}
	for k, v := range a {
		if v != nil {
			given[k] = v
		}
	}
	a = given
	d := roundDef{via: elsewhere}
	set := map[string]bool{}
	str := map[string]*string{"mode": &d.mode, "cron": &d.cron, "tz": &d.tz, "every": &d.every, "at": &d.at,
		"project": &d.project, "remote": &d.remote, "cwd": &d.cwd, "target": &d.target, "harness": &d.harness,
		"model": &d.model, "timeout": &d.timeout, "wait": &d.wait, "expires": &d.expires, "catch_up": &d.catchUp,
		"on_finish": &d.onFinish, "notify": &d.notify, "on": &d.on, "debounce": &d.debounce, "cooldown": &d.cooldown,
		"group": &d.group}
	for name, p := range str {
		if _, ok := a[name]; ok {
			*p, set[strings.ReplaceAll(name, "_", "-")] = a.str(name), true
		}
	}
	if _, ok := a["max_runs"]; ok {
		d.maxRuns, set["max-runs"] = strconv.FormatInt(a.num("max_runs"), 10), true
	}
	if _, ok := a["where"]; ok {
		d.where, set["where"] = a.list("where"), true
	}
	if _, ok := a["harness_args"]; ok {
		d.harnessArgs, set["harness-arg"] = a.list("harness_args"), true
	}
	for name, p := range map[string]*bool{"manual": &d.manual, "paused": &d.paused} {
		if _, ok := a[name]; ok {
			*p = a.boolean(name)
			set[name] = *p || name == "paused"
		}
	}
	var words []string
	_, hasPrompt := a["prompt"]
	_, hasCommand := a["command"]
	switch {
	case hasPrompt && hasCommand:
		return false, errors.New("give a prompt or a command, not both")
	case hasCommand:
		d.command, words = true, a.list("command")
	case hasPrompt:
		words = []string{a.str("prompt")}
	}
	if err := d.apply(spec, set, words); err != nil {
		return false, err
	}
	return set["project"] || set["remote"], nil
}

func runRoundLs(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	host := a.str("host")
	if a.boolean("all") || host != "" {
		cl, err := readCatalog(ctx, c)
		if err != nil {
			return nil, "", err
		}
		rows := hostRounds(cl)
		if host != "" {
			rows = slices.DeleteFunc(rows, func(r hostRound) bool { return r.Label != host && r.Host != host })
		}
		var b strings.Builder
		printHostRounds(&b, rows)
		return rows, b.String(), nil
	}
	var l api.RoundList
	if err := c.Call(ctx, "round.list", api.RoundListParams{}, &l); err != nil {
		return nil, "", err
	}
	var b strings.Builder
	printRounds(&b, l.Rounds)
	return l, b.String(), nil
}

func runRoundShow(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	var r api.RoundResult
	if err := c.Call(ctx, "round.get", api.RoundRef{Name: a.str("name")}, &r); err != nil {
		return nil, "", err
	}
	var runs api.RoundRuns
	if err := c.Call(ctx, "round.runs", api.RoundRunsParams{RoundRef: api.RoundRef{ID: r.Round.ID}, Limit: 5}, &runs); err != nil {
		return nil, "", err
	}
	var b strings.Builder
	printRound(&b, r.Round, runs.Runs)
	return struct {
		Round api.Round      `json:"round"`
		Runs  []api.RoundRun `json:"runs"`
	}{r.Round, runs.Runs}, b.String(), nil
}

func runRoundRuns(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	limit := int(a.num("limit"))
	if limit <= 0 {
		limit = 20
	}
	var runs api.RoundRuns
	if err := c.Call(ctx, "round.runs", api.RoundRunsParams{RoundRef: api.RoundRef{Name: a.str("name")}, Limit: limit}, &runs); err != nil {
		return nil, "", err
	}
	var b strings.Builder
	printRuns(&b, runs.Runs)
	return runs, b.String(), nil
}

func runRoundAdd(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	host := a.str("host") // a null host is "": this hab
	spec := api.RoundSpec{Name: a.str("name"), Mode: api.RoundModeFresh}
	named, err := roundSpecArgs(&spec, a, host != "")
	if err != nil {
		return nil, "", control.Errorf(api.ErrorKindInvalid, "%v", err)
	}
	runNow := a.boolean("run_now")
	if host != "" {
		if spec.Mode == api.RoundModeFresh && !named {
			if spec.ProjectRemote, err = localRemote(ctx, c); err != nil {
				return nil, "", err
			}
		}
		cf, err := roundElsewhere(ctx, c, host, spec, runNow)
		if err != nil {
			return nil, "", err
		}
		if err := roundConfirmationErr(cf); err != nil {
			return nil, "", errors.New(strings.TrimSpace(roundConfirmationText(cf)))
		}
		return api.ConfirmRequested{Confirmation: cf}, roundConfirmationText(cf), nil
	}
	if spec.Mode == api.RoundModeFresh && !named {
		wd, err := os.Getwd()
		if err != nil {
			return nil, "", err
		}
		spec.ProjectDir = wd
	}
	var made api.RoundCreated
	if err := c.Call(ctx, "round.create", api.RoundCreateParams{Round: spec, RunNow: runNow}, &made); err != nil {
		return nil, "", err
	}
	text := fmt.Sprintf("Made round %s (%s) on this host, with no confirmation asked. Next run: %s.\n", made.Round.Name, made.Round.ID, orDash(made.Round.NextSlot))
	if made.Run != nil {
		text += fmt.Sprintf("Run %d started: %s.\n", made.Run.Number, made.Run.State)
	}
	return made, text, nil
}

func runRoundEdit(ctx context.Context, c caller, a toolArgs) (any, string, error) {
	var r api.RoundResult
	if err := c.Call(ctx, "round.get", api.RoundRef{Name: a.str("name")}, &r); err != nil {
		return nil, "", err
	}
	spec := r.Round.RoundSpec
	spec.ProjectRemote = "" // read only here: the project id stays
	if _, err := roundSpecArgs(&spec, a, false); err != nil {
		return nil, "", control.Errorf(api.ErrorKindInvalid, "%v", err)
	}
	var res api.RoundResult
	if err := c.Call(ctx, "round.update", api.RoundUpdateParams{ID: r.Round.ID, Round: spec, Revision: r.Round.Revision}, &res); err != nil {
		return nil, "", err
	}
	return res, fmt.Sprintf("Changed round %s. Next run: %s.\n", res.Round.Name, orDash(res.Round.NextSlot)), nil
}

func runRoundRef(ctx context.Context, c caller, verb string, a toolArgs) (any, string, error) {
	ref := api.RoundRef{Name: a.str("name")}
	switch verb {
	case "rm":
		if err := c.Call(ctx, "round.delete", ref, nil); err != nil {
			return nil, "", err
		}
		return api.Empty{}, fmt.Sprintf("Deleted round %s. A live run goes on to its end.\n", ref.Name), nil
	case "run":
		var r api.RoundRunResult
		if err := c.Call(ctx, "round.run", ref, &r); err != nil {
			return nil, "", err
		}
		return r, fmt.Sprintf("Started run %d of round %s (%s): %s.\n", r.Run.Number, ref.Name, r.Run.ID, r.Run.State), nil
	}
	var r api.RoundResult
	if err := c.Call(ctx, "round."+verb, ref, &r); err != nil {
		return nil, "", err
	}
	return r, fmt.Sprintf("Round %s is %s.\n", r.Round.Name, roundStatus(r.Round)), nil
}
