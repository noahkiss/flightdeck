package service

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
)

// logicUnit renders <prefix>-gff-hab.service: the logic process, restarted always after a short
// delay (spec 5.1, 5.2). StartLimitIntervalSec=0 turns off systemd's start rate limit (5 starts in
// 10 s by default): the logic job restarts freely, by hand or after a crash, and RestartSec
// already bounds a crash loop to one start a second.
func logicUnit(c Config) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Written by gff-hab install; gff-hab uninstall removes it.\n")
	fmt.Fprintf(&b, "[Unit]\nDescription=Flightdeck logic process (gff-hab)\nStartLimitIntervalSec=0\n\n")
	fmt.Fprintf(&b, "[Service]\nType=simple\n")
	fmt.Fprintf(&b, "ExecStart=%s\n", execLine(c.HabPath(), c.ServeArgs()...))
	writeEnv(&b, c)
	fmt.Fprintf(&b, "Restart=always\nRestartSec=1\n\n")
	fmt.Fprintf(&b, "[Install]\nWantedBy=default.target\n")
	return b.Bytes()
}

// holderUnit renders <prefix>-gff-hab-holder@.service: one holder per instance, restarted only on
// failure. KillMode=process: a stop or a restart signals the holder alone, never its children;
// the holder's own shutdown ends programs when that is meant (spec 2.8, 5.2). The stop timeout
// covers the holder's 5 s grace for its children.
func holderUnit(c Config) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Written by gff-hab install; gff-hab uninstall removes it.\n")
	fmt.Fprintf(&b, "[Unit]\nDescription=Flightdeck holder %%i (gff-hab-holder)\n\n")
	fmt.Fprintf(&b, "[Service]\nType=simple\n")
	args := []string{systemdQuote(c.HolderPath(), true)}
	for _, a := range c.HolderArgs("\x00") {
		if a == "\x00" {
			args = append(args, "%i") // the instance name, from the unit name
			continue
		}
		args = append(args, systemdQuote(a, true))
	}
	fmt.Fprintf(&b, "ExecStart=%s\n", strings.Join(args, " "))
	writeEnv(&b, c)
	fmt.Fprintf(&b, "Restart=on-failure\nRestartSec=1\nKillMode=process\nTimeoutStopSec=30\n")
	return b.Bytes()
}

// writeEnv writes PATH and the locale settings: a unit gets a bare environment (spec 5.2).
func writeEnv(b *bytes.Buffer, c Config) {
	if c.Path != "" {
		fmt.Fprintf(b, "Environment=%s\n", systemdQuote("PATH="+c.Path, false))
	}
	for _, kv := range c.JobEnv() {
		fmt.Fprintf(b, "Environment=%s\n", systemdQuote(kv, false))
	}
}

// execLine quotes a command line for ExecStart.
func execLine(bin string, args ...string) string {
	out := []string{systemdQuote(bin, true)}
	for _, a := range args {
		out = append(out, systemdQuote(a, true))
	}
	return strings.Join(out, " ")
}

// systemdQuote quotes s as one word of a unit file setting. '%' starts a specifier everywhere;
// '$' starts a variable only in command lines.
func systemdQuote(s string, command bool) string {
	r := []string{`\`, `\\`, `"`, `\"`, `%`, `%%`}
	if command {
		r = append(r, `$`, `$$`)
	}
	return `"` + strings.NewReplacer(r...).Replace(s) + `"`
}

func systemctl(ctx context.Context, run Runner, args ...string) ([]byte, error) {
	return run(ctx, "systemctl", append([]string{"--user"}, args...)...)
}

func installSystemd(ctx context.Context, c Config, o Options) error {
	if _, err := systemctl(ctx, o.Run, "daemon-reload"); err != nil {
		return err
	}
	if _, err := systemctl(ctx, o.Run, "enable", c.LogicJob()); err != nil {
		return err
	}
	// restart starts a stopped job and restarts a running one, so a second install runs the
	// new gff-hab. Holder jobs are not touched.
	if _, err := systemctl(ctx, o.Run, "restart", c.LogicJob()); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "installed %s and %s in %s\n", c.LogicJob(), c.holderTemplate(), c.UnitDir)
	out, err := o.Run(ctx, "loginctl", "show-user", strconv.Itoa(c.UID), "--property=Linger", "--value")
	if err != nil || strings.TrimSpace(string(out)) != "yes" {
		fmt.Fprintf(o.Out, "lingering is off for this user: the jobs stop at logout and do not start at boot.\n"+
			"To keep them running, run: loginctl enable-linger\n"+
			"(some systems need an administrator to allow it)\n")
	}
	return nil
}

// holderPattern matches every holder instance unit of c.
func (c Config) holderPattern() string { return c.Prefix + "-gff-hab-holder@*.service" }

// runningSystemdHolders lists the holder units that are active or starting.
func runningSystemdHolders(ctx context.Context, c Config, run Runner) ([]string, error) {
	out, err := systemctl(ctx, run, "list-units", "--all", "--plain", "--no-legend", c.holderPattern())
	if err != nil {
		return nil, err
	}
	var units []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		switch f[2] {
		case "active", "activating", "deactivating", "reloading":
			units = append(units, f[0])
		}
	}
	return units, nil
}

func uninstallSystemd(ctx context.Context, c Config, o Options, m *manifest) error {
	// A unit that is not loaded makes disable fail; that is fine here. A logic job that still
	// runs is not: it would start a holder after the holders stop.
	if _, err := systemctl(ctx, o.Run, "disable", "--now", c.LogicJob()); err != nil {
		out, _ := systemctl(ctx, o.Run, "is-active", c.LogicJob())
		switch state := strings.TrimSpace(string(out)); state {
		case "active", "activating", "deactivating", "reloading", "refreshing":
			return fmt.Errorf("service: %s is %s after disable --now: %w", c.LogicJob(), state, err)
		}
	}
	// SIGTERM to each holder: its shutdown ends its programs, then it exits (spec 2.8).
	if _, err := systemctl(ctx, o.Run, "stop", c.holderPattern()); err != nil {
		return err
	}
	if err := removeFiles(c, m); err != nil {
		return err
	}
	if _, err := systemctl(ctx, o.Run, "daemon-reload"); err != nil {
		return err
	}
	systemctl(ctx, o.Run, "reset-failed", c.LogicJob(), c.holderPattern())
	fmt.Fprintf(o.Out, "removed %s and %s from %s\n", c.LogicJob(), c.holderTemplate(), c.UnitDir)
	return nil
}

// systemdStarter starts each holder as an instance of the template unit (spec 5.4).
type systemdStarter struct {
	c           Config
	run         Runner
	newInstance func() string
}

func (s *systemdStarter) Start(ctx context.Context) error {
	_, err := systemctl(ctx, s.run, "start", s.c.HolderJob(s.newInstance()))
	return err
}

// Retire has nothing to do: a stopped instance of a template unit leaves no file behind.
func (s *systemdStarter) Retire(string, int) {}

// Sweep has nothing to do, for the same reason.
func (s *systemdStarter) Sweep(context.Context) {}
