package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/grants"
	"github.com/noahkiss/flightdeck/internal/habconn"
)

// gff setup (macos-identity.md 7): check, and with no --check walk, the permission grants of
// the processes that run programs. It checks nothing in its own process: the hab answers
// hab.permissions from a probe pane in its holder and from itself (7.2).
//
//	gff setup [--check] [--json] [--host <hab>] [--grant <name>]... [--timeout 5m] [--no-open]
//	          [--socket path | --via command]

// Exit codes of gff setup (7.6).
const (
	exitOK       = 0
	exitMissing  = 1  // a required grant is not granted, a signature fails, or a re-grant is owed
	exitCannot   = 2  // the check could not run
	exitUsage    = 64 // a usage error
	pollInterval = 2 * time.Second
)

// callTimeout bounds one hab.permissions call; a request waits for the user up to two minutes.
const callTimeout = 4 * time.Minute

// stopWait bounds the final check after Ctrl-C stops the walk.
const stopWait = 5 * time.Second

type setupOptions struct {
	check, json, noOpen bool
	host, socket, via   string
	grants              []string
	timeout             time.Duration
}

func parseSetup(args []string, stderr io.Writer) (setupOptions, error) {
	var o setupOptions
	fs := flag.NewFlagSet("gff setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&o.check, "check", false, "only report; open nothing and cause no dialog")
	fs.BoolVar(&o.json, "json", false, "with --check: print the rows as JSON")
	fs.StringVar(&o.host, "host", "", "check the hab on this host, through the system ssh (--check only)")
	fs.StringVar(&o.socket, "socket", "", "the local hab's control socket")
	fs.StringVar(&o.via, "via", "", "run this command and speak over its stdio")
	var gs multi
	fs.Var(&gs, "grant", "walk only this grant (repeatable): full-disk-access, local-network, accessibility, screen-recording, automation:<bundle-id>, developer-tools, ...")
	fs.DurationVar(&o.timeout, "timeout", 5*time.Minute, "how long to wait for each grant")
	fs.BoolVar(&o.noOpen, "no-open", false, "print the Settings location instead of opening it")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	n := 0
	for _, s := range []string{o.host, o.socket, o.via} {
		if s != "" {
			n++
		}
	}
	if n > 1 {
		return o, errors.New("give one of --host, --socket and --via")
	}
	for _, g := range gs {
		if !grants.Known(g) {
			return o, fmt.Errorf("--grant %q: unknown grant", g)
		}
	}
	if o.json && !o.check {
		return o, errors.New("--json needs --check")
	}
	if o.timeout <= 0 {
		return o, errors.New("--timeout must be positive")
	}
	o.grants = gs
	return o, nil
}

// setupMain runs gff setup and returns its exit code.
func setupMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	o, err := parseSetup(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "gff setup:", err)
		}
		return exitUsage
	}
	tr, local, err := transport(o.socket, o.via, o.host)
	if err != nil {
		fmt.Fprintln(stderr, "gff setup:", err)
		return exitCannot
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop() // the first Ctrl-C stops the walk; a second one ends the process at once
	}()
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	conn, err := habconn.Connect(cctx, habconn.Config{
		Transport: tr,
		Client:    api.ClientInfo{Name: "gff", Version: build, Kind: api.ClientKindCli},
	})
	cancel()
	if err != nil {
		if o.json {
			writeJSON(stdout, exitCannot, api.Permissions{}, "no hab: "+err.Error())
		} else {
			fmt.Fprintf(stdout, "The check could not run: no hab: %v\n", err)
		}
		return exitCannot
	}
	defer conn.Close()
	where := "this Mac"
	if runtime.GOOS != "darwin" {
		where = "this host"
	}
	if o.host != "" {
		where = o.host
	} else if o.via != "" {
		where = "the hab"
	}
	w := &walker{
		opts: o, out: stdout, where: where, local: local && o.host == "",
		call: func(ctx context.Context, p api.PermissionsParams) (api.Permissions, error) {
			ctx, cancel := context.WithTimeout(ctx, callTimeout)
			defer cancel()
			return conn.API().HabPermissions(ctx, p)
		},
		open: func(args ...string) error {
			return exec.Command("open", args...).Run()
		},
		lines: lineReader(stdin),
		tick:  pollInterval,
	}
	return w.run(ctx)
}

// lineReader sends each line of r on a channel, closed at EOF.
func lineReader(r io.Reader) <-chan string {
	ch := make(chan string)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			ch <- strings.TrimSpace(sc.Text())
		}
	}()
	return ch
}

// walker runs the check and the walk. Every outside effect goes through its fields, so tests
// drive it with fakes.
type walker struct {
	opts  setupOptions
	out   io.Writer
	where string
	local bool // the hab runs on this Mac, so Settings can open here
	call  func(context.Context, api.PermissionsParams) (api.Permissions, error)
	open  func(args ...string) error
	lines <-chan string
	tick  time.Duration
	// stopWait bounds the final check after a stop; 0 means the stopWait constant.
	stopWait time.Duration
}

func (w *walker) check(ctx context.Context, p api.PermissionsParams) (api.Permissions, int, string) {
	res, err := w.call(ctx, p)
	if err != nil {
		return res, exitCannot, "hab.permissions: " + err.Error()
	}
	code, why := exitCode(res)
	return res, code, why
}

func (w *walker) run(ctx context.Context) int {
	res, code, why := w.check(ctx, api.PermissionsParams{})
	if w.opts.check {
		if w.opts.json {
			writeJSON(w.out, code, res, why)
		} else {
			printCheck(w.out, w.where, res, code, why)
		}
		return code
	}
	printCheck(w.out, w.where, res, code, why)
	todo := w.todo(res)
	switch {
	case code == exitCannot:
		return code
	case len(todo) == 0:
		return code // a set-up Mac: nothing opens, nothing changes (7.7)
	case res.OS != "darwin":
		w.linuxHints(todo)
		return code
	case !w.local:
		fmt.Fprintln(w.out, "\nThe walk opens System Settings on the hab's Mac. Run gff setup there.")
		return code
	}
	fmt.Fprintln(w.out)
	stopped := false
	for _, r := range todo {
		if err := w.walkOne(ctx, r); err != nil {
			fmt.Fprintln(w.out, "\nStopped.")
			stopped = true
			break
		}
	}
	// The walk ends with the full check (7.5). After a stop the hab may still run the probe the
	// walk left, for up to 2 minutes, and the final check would queue behind it. So it waits a
	// few seconds at most; leaving closes the connection, which ends that probe.
	cctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if stopped {
		wait := w.stopWait
		if wait <= 0 {
			wait = stopWait
		}
		cctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), wait)
	}
	defer cancel()
	res, code, why = w.check(cctx, api.PermissionsParams{})
	if stopped && code == exitCannot && cctx.Err() != nil {
		fmt.Fprintln(w.out, "The final check did not finish. Run `gff setup --check` to see where things stand.")
		return exitCannot
	}
	fmt.Fprintln(w.out)
	printCheck(w.out, w.where, res, code, why)
	return code
}

// todo lists the rows the walk visits: required rows not granted, and the rows --grant names.
// A remembered row counts as not granted: only a new request shows whether the grant is still
// on. One row per process and grant.
func (w *walker) todo(res api.Permissions) []api.PermissionRow {
	var out []api.PermissionRow
	seen := map[string]bool{}
	for _, r := range res.Rows {
		named := slices.Contains(w.opts.grants, r.Grant)
		if len(w.opts.grants) > 0 && !named {
			continue
		}
		if r.State == api.PermissionStateGranted && !r.Remembered || r.State == api.PermissionStateNote || r.Draining {
			continue
		}
		if !r.Required && !named {
			continue
		}
		k := string(r.Process) + " " + r.Grant
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, r)
	}
	return out
}

func (w *walker) linuxHints(todo []api.PermissionRow) {
	fmt.Fprintln(w.out)
	for _, r := range todo {
		switch r.Grant {
		case grants.Lingering:
			fmt.Fprintln(w.out, "Run: loginctl enable-linger")
		case grants.Units:
			fmt.Fprintln(w.out, "Run: gff-hab install")
		}
	}
}

var errStopped = errors.New("stopped")

// walkOne fixes one row: say what is missing, open its Settings pane, wait and verify (7.3 to
// 7.5). It returns errStopped when the user stops the walk.
func (w *walker) walkOne(ctx context.Context, r api.PermissionRow) error {
	who := "GFF Holder"
	if r.Process == api.PermissionProcessHab {
		who = "GFF Hab"
	}
	name := grants.MenuPath(r.Grant)
	fmt.Fprintf(w.out, "%s %s: %s.", r.Process, grants.Label(r.Grant), stateWord(r.State))
	if r.Detail != "" {
		fmt.Fprintf(w.out, " %s.", r.Detail)
	}
	if r.Remembered {
		fmt.Fprint(w.out, " It may have changed since; asking again.")
	}
	fmt.Fprintln(w.out)
	switch r.Grant {
	case grants.Signature:
		fmt.Fprintln(w.out, "  Reinstall the release; gff setup cannot fix a signature.")
		return nil
	case grants.RegrantOwed:
		fmt.Fprintln(w.out, "  Grant each missing row again; the final check clears this.")
		return nil
	case grants.OptIns:
		fmt.Fprintln(w.out, "  Fix GFF_MACOS_GRANTS, then run gff-hab install with it set.")
		return nil
	case grants.Domain, grants.DevBuild:
		return nil
	}
	request := ""
	if grants.Requestable(r.Grant) && r.Process == api.PermissionProcessHolder {
		request = r.Grant
		switch r.Grant {
		case grants.LocalNetwork:
			fmt.Fprintln(w.out, "  A pane connects to your router now. If macOS asks whether GFF Holder may find devices on the local network, click Allow.")
		case grants.FilesAndFolders:
			fmt.Fprintln(w.out, "  A pane opens Desktop, Documents and Downloads now. Click Allow on each dialog that names GFF Holder.")
		default:
			fmt.Fprintf(w.out, "  Asking macOS to list %s in %s.\n", who, name)
		}
		got, ok := w.recheck(ctx, r, request)
		if ok && got == api.PermissionStateGranted {
			fmt.Fprintf(w.out, "  %s: ok\n", grants.Label(r.Grant))
			return nil
		}
	}
	w.openSettings(r)
	switch r.Grant {
	case grants.FullDiskAccess:
		fmt.Fprintf(w.out, "  Add %s to the list and turn it on. Drag it from the Finder window, or press + and Cmd-Shift-G:\n    %s\n", who, r.Reveal)
	case grants.BackgroundItem:
		fmt.Fprintln(w.out, "  Turn on the Flightdeck items under Allow in the Background.")
	case grants.LocalNetwork:
		fmt.Fprintf(w.out, "  Open the Local Network row and turn on %s.\n", who)
	case grants.DeveloperTools:
		fmt.Fprintf(w.out, "  Add %s and turn it on. No check can confirm this grant.\n", who)
		return nil
	default:
		fmt.Fprintf(w.out, "  Turn on %s there.\n", who)
	}
	// Local Network and Files and Folders have no check without a connection or an open, so
	// the wait repeats the request (a decided folder or network answers at once).
	return w.wait(ctx, r, request)
}

// openSettings opens the row's Settings pane and, for Full Disk Access, shows the bundle in
// Finder (7.4). Without a URL that opens, it prints the menu path in words.
func (w *walker) openSettings(r api.PermissionRow) {
	place := grants.MenuPath(r.Grant)
	url := r.Settings
	if url == "" {
		url = grants.SettingsURL(r.Grant)
	}
	if w.opts.noOpen || url == "" || w.open(url) != nil {
		fmt.Fprintf(w.out, "  Open %s.\n", place)
	} else {
		fmt.Fprintf(w.out, "  Opened %s.\n", place)
	}
	if r.Reveal != "" && !w.opts.noOpen {
		w.open("-R", r.Reveal)
	}
}

// recheck runs the check of one row's grant and returns that row's state.
func (w *walker) recheck(ctx context.Context, r api.PermissionRow, request string) (api.PermissionState, bool) {
	p := api.PermissionsParams{Grants: []string{r.Grant}, Request: request}
	res, err := w.call(ctx, p)
	if err != nil {
		return "", false
	}
	for _, x := range res.Rows {
		if x.Process == r.Process && x.Grant == r.Grant && !x.Draining {
			if x.Remembered && request == "" {
				return api.PermissionStateUnknown, true // an old answer proves nothing now
			}
			return x.State, true
		}
	}
	return "", false
}

// wait re-runs one check every tick until the probe says granted, the timeout passes, the user
// skips (s and Enter) or stops (Ctrl-C). Enter alone re-checks at once (7.5).
func (w *walker) wait(ctx context.Context, r api.PermissionRow, request string) error {
	fmt.Fprintf(w.out, "  Waiting up to %v. Enter checks now, s and Enter skips, Ctrl-C stops.\n", w.opts.timeout)
	deadline := time.NewTimer(w.opts.timeout)
	defer deadline.Stop()
	t := time.NewTicker(w.tick)
	defer t.Stop()
	last := r.State
	for {
		select {
		case <-ctx.Done():
			return errStopped
		case <-deadline.C:
			fmt.Fprintf(w.out, "  %s: still %s after %v; going on.\n", grants.Label(r.Grant), stateWord(last), w.opts.timeout)
			if r.Grant == grants.FullDiskAccess && r.Process == api.PermissionProcessHolder {
				fmt.Fprintln(w.out, "  If it is turned on in Settings, a holder that ran before the grant may not see it (macos-identity.md 6.3).")
			}
			return nil
		case line, ok := <-w.lines:
			if !ok {
				w.lines = nil // stdin ended: keep polling
				continue
			}
			if strings.EqualFold(line, "s") {
				fmt.Fprintf(w.out, "  %s: skipped\n", grants.Label(r.Grant))
				return nil
			}
		case <-t.C:
		}
		st, ok := w.recheck(ctx, r, request)
		if ctx.Err() != nil {
			return errStopped
		}
		if !ok {
			continue
		}
		if st == api.PermissionStateGranted {
			fmt.Fprintf(w.out, "  %s: ok\n", grants.Label(r.Grant))
			return nil
		}
		if st != last {
			fmt.Fprintf(w.out, "  %s: now %s\n", grants.Label(r.Grant), stateWord(st))
			last = st
		}
	}
}

// exitCode is the --check exit code of a result (7.6), and the reason for exit 2.
func exitCode(res api.Permissions) (int, string) {
	code := exitOK
	for _, r := range res.Rows {
		if r.State == api.PermissionStateError {
			return exitCannot, r.Detail
		}
		if r.Required && r.State != api.PermissionStateGranted {
			code = exitMissing
		}
	}
	return code, ""
}

// stateWord is the word --check prints for a state (7.6).
func stateWord(s api.PermissionState) string {
	switch s {
	case api.PermissionStateGranted:
		return "ok"
	case api.PermissionStateDenied, api.PermissionStateUnanswered:
		return "missing"
	case "":
		return "unknown"
	}
	return string(s)
}

// processWord is the first column: a draining holder is named apart from the current one.
func processWord(r api.PermissionRow) string {
	if r.Draining {
		return "draining"
	}
	return string(r.Process)
}

// printCheck prints the --check output: the host line, a line per grant and process, then a
// summary (7.6).
func printCheck(out io.Writer, where string, res api.Permissions, code int, why string) {
	host := where
	var info []string
	switch {
	case res.OS == "darwin" && res.OSVersion != "":
		info = append(info, "macOS "+res.OSVersion)
	case res.OS == "darwin":
		info = append(info, "macOS")
	case res.OS != "":
		info = append(info, res.OS)
	}
	aqua := false
	for _, r := range res.Rows {
		if r.Grant == grants.Domain {
			aqua = r.State == api.PermissionStateGranted
			if !aqua {
				break
			}
		}
	}
	if aqua {
		info = append(info, "Aqua")
	}
	if len(info) > 0 {
		host += " (" + strings.Join(info, ", ") + ")"
	}
	fmt.Fprintf(out, "%-8s  %s\n", "host", host)
	width := 0
	for _, r := range res.Rows {
		if r.Grant != grants.Domain || r.State != api.PermissionStateGranted {
			width = max(width, len(grants.Label(r.Grant)))
		}
	}
	missing, unknown := 0, 0
	for _, r := range res.Rows {
		if r.Grant == grants.Domain && r.State == api.PermissionStateGranted {
			continue // said on the host line
		}
		detail := r.Detail
		if r.State == api.PermissionStateUnanswered && detail == "" {
			detail = "no answer in time"
		}
		if r.Draining && r.Instance != "" {
			detail = strings.TrimSpace("holder " + r.Instance + ": " + detail)
		}
		line := fmt.Sprintf("%-8s  %-*s  %-7s  %s", processWord(r), width, grants.Label(r.Grant), stateWord(r.State), detail)
		fmt.Fprintln(out, strings.TrimRight(line, " "))
		if r.Required {
			switch r.State {
			case api.PermissionStateGranted:
			case api.PermissionStateUnknown:
				unknown++
			default:
				missing++
			}
		}
	}
	if res.OS != "" && res.OS != "darwin" {
		fmt.Fprintln(out, "macOS grants: not applicable")
	}
	fmt.Fprintln(out)
	switch code {
	case exitCannot:
		fmt.Fprintf(out, "The check could not run: %s\n", why)
	case exitOK:
		fmt.Fprintln(out, "Every required grant is in place.")
	default:
		var parts []string
		if missing > 0 {
			parts = append(parts, fmt.Sprintf("%d missing", missing))
		}
		if unknown > 0 {
			parts = append(parts, fmt.Sprintf("%d unknown", unknown))
		}
		fmt.Fprintf(out, "%s. Run `gff setup` to fix.\n", strings.Join(parts, ", "))
	}
}

// writeJSON prints the --check --json output: the same rows, the exit code and its reason.
func writeJSON(out io.Writer, code int, res api.Permissions, why string) {
	rows := res.Rows
	if rows == nil {
		rows = []api.PermissionRow{}
	}
	v := struct {
		Exit      int                 `json:"exit"`
		Reason    string              `json:"reason,omitempty"`
		OS        string              `json:"os,omitempty"`
		OSVersion string              `json:"os_version,omitempty"`
		Rows      []api.PermissionRow `json:"rows"`
	}{code, why, res.OS, res.OSVersion, rows}
	b, _ := json.MarshalIndent(v, "", "  ")
	out.Write(append(b, '\n'))
}
