package service

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/internal/holderreg"
)

// ErrNoGUI means the user has no graphical login, so there is no gui/<uid> domain (spec 5.3, Q5).
var ErrNoGUI = errors.New("service: no graphical login session for this user (launchd domain gui/<uid>). " +
	"Holders run in that domain so that programs in panes get the clipboard, the login keychain and the " +
	"privacy grants a terminal app gets; jobs started over SSH get none of these. " +
	"Log in at the Mac's screen (a Mac that must recover alone after a reboot needs automatic login), then try again")

// domain is the launchd domain the jobs live in.
func (c Config) domain() string { return "gui/" + strconv.Itoa(c.UID) }

// target is one job in the domain.
func (c Config) target(label string) string { return c.domain() + "/" + label }

// checkGUI fails with ErrNoGUI when gui/<uid> does not exist.
func checkGUI(ctx context.Context, c Config, run Runner) error {
	if _, err := run(ctx, "launchctl", "print", c.domain()); err != nil {
		return fmt.Errorf("%w (%v)", ErrNoGUI, err)
	}
	return nil
}

// plist is a small property list writer.
type plist struct{ b bytes.Buffer }

func (p *plist) str(s string) {
	p.b.WriteString("<string>")
	xml.EscapeText(&p.b, []byte(s))
	p.b.WriteString("</string>")
}

func (p *plist) key(k string) {
	p.b.WriteString("\t<key>")
	xml.EscapeText(&p.b, []byte(k))
	p.b.WriteString("</key>\n")
}

func (p *plist) kv(k, v string) {
	p.key(k)
	p.b.WriteString("\t")
	p.str(v)
	p.b.WriteString("\n")
}

func (p *plist) kb(k string, v bool) {
	p.key(k)
	if v {
		p.b.WriteString("\t<true/>\n")
	} else {
		p.b.WriteString("\t<false/>\n")
	}
}

func (p *plist) ki(k string, v int) {
	p.key(k)
	fmt.Fprintf(&p.b, "\t<integer>%d</integer>\n", v)
}

func (p *plist) args(k string, argv []string) {
	p.key(k)
	p.b.WriteString("\t<array>\n")
	for _, a := range argv {
		p.b.WriteString("\t\t")
		p.str(a)
		p.b.WriteString("\n")
	}
	p.b.WriteString("\t</array>\n")
}

// env writes PATH and the locale settings (spec 5.2: a job gets a bare environment).
func (p *plist) env(c Config) {
	vars := c.Locale
	if c.Path != "" {
		vars = append([]string{"PATH=" + c.Path}, vars...)
	}
	if len(vars) == 0 {
		return
	}
	p.key("EnvironmentVariables")
	p.b.WriteString("\t<dict>\n")
	for _, kv := range vars {
		k, v, _ := strings.Cut(kv, "=")
		p.b.WriteString("\t\t<key>")
		xml.EscapeText(&p.b, []byte(k))
		p.b.WriteString("</key>\n\t\t")
		p.str(v)
		p.b.WriteString("\n")
	}
	p.b.WriteString("\t</dict>\n")
}

func (p *plist) begin() {
	p.b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	p.b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	p.b.WriteString("<!-- Written by gff-hab; gff-hab uninstall removes it. -->\n")
	p.b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
}

func (p *plist) end() []byte {
	p.b.WriteString("</dict>\n</plist>\n")
	return p.b.Bytes()
}

// common writes the keys both jobs share: the GUI session only (spec 5.3), a working directory,
// a log file under the state directory, PATH and the locale.
func (p *plist) common(c Config, log string) {
	p.kv("LimitLoadToSessionType", "Aqua")
	p.kv("WorkingDirectory", c.StateDir)
	p.kv("StandardOutPath", log)
	p.kv("StandardErrorPath", log)
	p.env(c)
}

// logicPlist renders <prefix>.gff-hab.plist: kept alive always, restarted after a short delay
// (spec 5.1).
func logicPlist(c Config) []byte {
	var p plist
	p.begin()
	p.kv("Label", c.LogicJob())
	p.args("ProgramArguments", append([]string{c.HabPath()}, c.ServeArgs()...))
	p.kb("RunAtLoad", true)
	p.kb("KeepAlive", true)
	p.ki("ThrottleInterval", 1)
	p.common(c, c.logicLog())
	return p.end()
}

// holderPlist renders <prefix>.gff-hab-holder.<instance>.plist: restarted only when it did not
// exit cleanly, so a holder that exits 0 after drain stays down (spec 5.1). AbandonProcessGroup
// keeps launchd from signalling anything but the holder when the job ends; the holder's shutdown
// ends programs when that is meant (spec 2.8). ExitTimeOut covers its 5 s grace for children.
func holderPlist(c Config, instance string) []byte {
	var p plist
	p.begin()
	p.kv("Label", c.HolderJob(instance))
	p.args("ProgramArguments", append([]string{c.HolderPath()}, c.HolderArgs(instance)...))
	p.kb("RunAtLoad", true)
	p.key("KeepAlive")
	p.b.WriteString("\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	p.kb("AbandonProcessGroup", true)
	p.ki("ExitTimeOut", 30)
	p.ki("ThrottleInterval", 1)
	p.common(c, c.holderLog(instance))
	return p.end()
}

func launchctl(ctx context.Context, run Runner, args ...string) ([]byte, error) {
	return run(ctx, "launchctl", args...)
}

// loaded reports whether label is a job in c's domain now.
func loaded(ctx context.Context, c Config, run Runner, label string) bool {
	_, err := launchctl(ctx, run, "print", c.target(label))
	return err == nil
}

// running reports whether label is a job in c's domain with a live process.
func running(ctx context.Context, c Config, run Runner, label string) bool {
	out, err := launchctl(ctx, run, "print", c.target(label))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "pid = ") {
			return true
		}
	}
	return false
}

// bootout unloads label and waits until launchd no longer lists it. Unloading a running job
// sends it SIGTERM, and SIGKILL after its ExitTimeOut.
func bootout(ctx context.Context, c Config, run Runner, label string) error {
	if !loaded(ctx, c, run, label) {
		return nil
	}
	launchctl(ctx, run, "bootout", c.target(label)) // its error says no more than the wait below
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for loaded(ctx, c, run, label) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("service: %s is still loaded after bootout", label)
		case <-t.C:
		}
	}
	return nil
}

func installLaunchd(ctx context.Context, c Config, o Options) error {
	// A second install replaces the logic job: unload the old one (it owns no programs), then
	// load the new plist. Holder jobs are not touched.
	if err := bootout(ctx, c, o.Run, c.LogicJob()); err != nil {
		return err
	}
	if _, err := launchctl(ctx, o.Run, "bootstrap", c.domain(), c.logicFile()); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "installed %s in %s, loaded into %s\n", c.LogicJob(), c.UnitDir, c.domain())
	return nil
}

// runningLaunchdHolders lists the holder jobs with a live process.
func runningLaunchdHolders(ctx context.Context, c Config, run Runner) ([]string, error) {
	plists, err := holderPlists(c)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range plists {
		label := strings.TrimSuffix(filepath.Base(p), ".plist")
		if running(ctx, c, run, label) {
			out = append(out, label)
		}
	}
	return out, nil
}

func uninstallLaunchd(ctx context.Context, c Config, o Options, m *manifest) error {
	if err := bootout(ctx, c, o.Run, c.LogicJob()); err != nil {
		return err
	}
	plists, err := holderPlists(c)
	if err != nil {
		return err
	}
	for _, p := range plists {
		// SIGTERM to the holder: its shutdown ends its programs, then it exits (spec 2.8).
		if err := bootout(ctx, c, o.Run, strings.TrimSuffix(filepath.Base(p), ".plist")); err != nil {
			return err
		}
	}
	if err := removeFiles(c, m); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "removed %s and %d holder job(s) from %s\n", c.LogicJob(), len(plists), c.UnitDir)
	return nil
}

// launchdStarter starts each holder as its own launch agent with a per-instance plist (spec 5.4).
type launchdStarter struct {
	c           Config
	run         Runner
	newInstance func() string
	logf        func(string, ...any)
}

func (s *launchdStarter) Start(ctx context.Context) error {
	if err := checkGUI(ctx, s.c, s.run); err != nil {
		return err
	}
	if err := os.MkdirAll(s.c.LogDir(), 0o700); err != nil {
		return err
	}
	inst := s.newInstance()
	path := s.c.holderPlist(inst)
	if err := writeAtomic(path, holderPlist(s.c, inst), 0o644); err != nil {
		return err
	}
	if _, err := launchctl(ctx, s.run, "bootstrap", s.c.domain(), path); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

// Retire waits for the holder's process to end, then unloads its job and deletes its plist and
// its log. Unloading a running holder would end it, so a holder still alive after a minute keeps
// its job. The wait ends when the pid is gone or when launchd runs no process for the job: a
// live pid that launchd does not run for the job is another process that reused the pid.
func (s *launchdStarter) Retire(instance string, pid int) {
	if !holderreg.ValidInstance(instance) {
		return
	}
	label := s.c.HolderJob(instance)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	deadline := time.Now().Add(time.Minute)
	for pid > 0 && holderreg.Alive(pid) && running(ctx, s.c, s.run, label) {
		if time.Now().After(deadline) {
			s.logf("service: holder %s (pid %d) still runs after it said bye; its job stays loaded", instance, pid)
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	if running(ctx, s.c, s.run, label) {
		s.logf("service: %s runs again; its job stays loaded", label)
		return
	}
	s.remove(ctx, instance)
}

// remove unloads the job of a holder with no process, then deletes its plist and its log.
func (s *launchdStarter) remove(ctx context.Context, instance string) bool {
	label := s.c.HolderJob(instance)
	if err := bootout(ctx, s.c, s.run, label); err != nil {
		s.logf("service: remove %s: %v", label, err)
		return false
	}
	ok := true
	for _, p := range []string{s.c.holderPlist(instance), s.c.holderLog(instance)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.logf("service: remove %s: %v", label, err)
			ok = false
		}
	}
	return ok
}

// Sweep removes the jobs of holders that are gone (spec 5.4): each holder plist whose instance
// has no live registry entry and whose job launchd runs no process for. Such a plist is left by a
// holder that exited while gff-hab was down, or by a gff-hab restart during Retire's wait, and
// launchd would start a holder from it at every login. Holder logs with no plist go too.
func (s *launchdStarter) Sweep(ctx context.Context) {
	live := map[string]bool{}
	entries, _ := holderreg.List(s.c.HolderDir) // a missing directory lists nothing
	for _, e := range entries {
		if holderreg.Alive(e.Pid) {
			live[e.Instance] = true
		}
	}
	plists, err := holderPlists(s.c)
	if err != nil {
		s.logf("service: sweep: %v", err)
		return
	}
	jobs := map[string]bool{}
	for _, p := range plists {
		inst := strings.TrimPrefix(strings.TrimSuffix(filepath.Base(p), ".plist"), s.c.Prefix+".gff-hab-holder.")
		if !holderreg.ValidInstance(inst) {
			continue
		}
		jobs[inst] = true
		if live[inst] || running(ctx, s.c, s.run, s.c.HolderJob(inst)) {
			continue
		}
		if s.remove(ctx, inst) {
			s.logf("service: removed the job of holder %s, which is gone", inst)
		}
	}
	logs, err := holderLogs(s.c)
	if err != nil {
		s.logf("service: sweep: %v", err)
		return
	}
	for _, l := range logs {
		inst := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(l), "gff-hab-holder."), ".log")
		if !jobs[inst] && !live[inst] {
			if err := os.Remove(l); err != nil && !errors.Is(err, os.ErrNotExist) {
				s.logf("service: sweep: %v", err)
			}
		}
	}
}
