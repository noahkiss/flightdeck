package grants

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// The probe protocol. gff-hab starts `<gff-hab> probe <ProbeArgs>` in a pane and reads the
// pane's bytes for one line:
//
//	GFFPROBE1 <nonce> <base64 of the ProbeResult JSON>
//
// The nonce is random per probe, so stale or stray output never passes for a result. The line
// is the only thing the probe prints on success.
const (
	marker = "GFFPROBE1"
	// MaxProbeOutput bounds the bytes gff-hab reads from a probe pane.
	MaxProbeOutput = 256 << 10
)

// Timeouts of the checks (7.3).
const (
	// FDATimeout: a grant answers at once, a refusal took about 5.5 s on the fork's Macs; no
	// answer in 10 s reads as unanswered (1.1).
	FDATimeout = 10 * time.Second
	// LANKeepAlive is how long a local network request keeps the probe alive: a short-lived
	// process gets no alert (TN3179).
	LANKeepAlive = 30 * time.Second
	// RequestTimeout bounds a request that waits for the user to answer a dialog.
	RequestTimeout = 2 * time.Minute
	dialTimeout    = 3 * time.Second
)

// The local network request's window and retry interval; tests shorten them.
var (
	lanWindow = LANKeepAlive
	lanRetry  = 2 * time.Second
)

// FDAPath is the folder whose open answers the Full Disk Access check (1.1). access(2) reads
// only permission bits, and the per-user TCC.db is gone on the newest macOS.
const FDAPath = "/Library/Application Support/com.apple.TCC"

// ProtectedFolders are the Files and Folders locations the walk opens, under the home directory.
var ProtectedFolders = []string{"Desktop", "Documents", "Downloads"}

// Check is one grant's answer from a probe or from gff-hab's own checks.
type Check struct {
	Grant  string              `json:"grant"`
	State  api.PermissionState `json:"state"`
	Detail string              `json:"detail,omitempty"`
}

// ProbeResult is what a probe prints.
type ProbeResult struct {
	Nonce  string  `json:"nonce"`
	Pid    int     `json:"pid"`
	Checks []Check `json:"checks"`
}

// NewNonce returns a random probe nonce.
func NewNonce() string {
	var b [12]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ProbeArgs are the arguments after the gff-hab path that start a probe.
func ProbeArgs(nonce string, checks []string, request string) []string {
	args := []string{"probe", "-nonce", nonce, "-checks", strings.Join(checks, ",")}
	if request != "" {
		args = append(args, "-request", request)
	}
	return args
}

// ProbeOptions is a parsed probe command line.
type ProbeOptions struct {
	Nonce   string
	Checks  []string
	Request string
}

// ParseProbeArgs parses the arguments after "probe".
func ParseProbeArgs(args []string) (ProbeOptions, error) {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	nonce := fs.String("nonce", "", "")
	checks := fs.String("checks", "", "")
	req := fs.String("request", "", "")
	if err := fs.Parse(args); err != nil {
		return ProbeOptions{}, err
	}
	if fs.NArg() > 0 || *nonce == "" || len(*nonce) > 64 {
		return ProbeOptions{}, errors.New("probe: want -nonce <nonce> -checks <grant,...> [-request <grant>]")
	}
	o := ProbeOptions{Nonce: *nonce, Request: *req}
	for _, c := range strings.Split(*checks, ",") {
		if c == "" {
			continue
		}
		if !Known(c) {
			return ProbeOptions{}, fmt.Errorf("probe: unknown grant %q", c)
		}
		o.Checks = append(o.Checks, c)
	}
	if o.Request != "" && !Requestable(o.Request) {
		return ProbeOptions{}, fmt.Errorf("probe: %q cannot be requested", o.Request)
	}
	return o, nil
}

// EncodeResult is the line a probe prints.
func EncodeResult(r ProbeResult) []byte {
	b, _ := json.Marshal(r)
	return []byte(marker + " " + r.Nonce + " " + base64.StdEncoding.EncodeToString(b) + "\n")
}

// ErrNoResult means the bytes hold no complete result line for the nonce yet.
var ErrNoResult = errors.New("grants: no probe result")

// FindResult finds the result line for nonce in a probe pane's bytes. The terminal turns the
// newline into CR LF; either ends the line.
func FindResult(out []byte, nonce string) (ProbeResult, error) {
	head := []byte(marker + " " + nonce + " ")
	i := bytes.Index(out, head)
	if i < 0 {
		return ProbeResult{}, ErrNoResult
	}
	rest := out[i+len(head):]
	end := bytes.IndexAny(rest, "\r\n")
	if end < 0 {
		return ProbeResult{}, ErrNoResult
	}
	raw, err := base64.StdEncoding.DecodeString(string(rest[:end]))
	if err != nil {
		return ProbeResult{}, fmt.Errorf("grants: probe result: %w", err)
	}
	var r ProbeResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return ProbeResult{}, fmt.Errorf("grants: probe result: %w", err)
	}
	if r.Nonce != nonce {
		return ProbeResult{}, errors.New("grants: probe result names another nonce")
	}
	return r, nil
}

// System is what the checks need from the operating system. The darwin one calls the real
// APIs; tests use a fake.
type System interface {
	// ManagerName is what `launchctl managername` prints: Aqua in a graphical session.
	ManagerName(ctx context.Context) (string, error)
	// OpenDir opens a directory and reads one entry; it may block until the user answers a
	// dialog.
	OpenDir(path string) error
	// AXTrusted is AXIsProcessTrusted; AXRequest calls AXIsProcessTrustedWithOptions with the
	// prompt option, which lists the process in Settings.
	AXTrusted() (bool, error)
	AXRequest() error
	// ScreenPreflight is CGPreflightScreenCaptureAccess; ScreenRequest is
	// CGRequestScreenCaptureAccess.
	ScreenPreflight() (bool, error)
	ScreenRequest() error
	// Automation is AEDeterminePermissionToAutomateTarget for a target bundle id, any event,
	// askUserIfNeeded false. It returns the OSStatus.
	Automation(target string) (int32, error)
	// Gateway is the default route's gateway address.
	Gateway(ctx context.Context) (string, error)
	// Dial connects to addr over TCP and closes the connection.
	Dial(ctx context.Context, addr string) error
	// Clipboard reads the pasteboard's text; SetClipboard writes it.
	Clipboard(ctx context.Context) (string, error)
	SetClipboard(ctx context.Context, s string) error
	// Home is the user's home directory.
	Home() string
}

// The OSStatus values of AEDeterminePermissionToAutomateTarget (1.3).
const (
	osNoErr                 = 0
	errAEEventNotPermitted  = -1743
	errAEEventWouldRequire  = -1744
	procNotFound            = -600
	errAETargetAddressError = -1719
)

// Probe runs the named checks and the request in this process. ctx bounds the whole probe.
func Probe(ctx context.Context, sys System, o ProbeOptions) []Check {
	p := prober{ctx: ctx, sys: sys}
	switch o.Request {
	case Accessibility:
		if err := sys.AXRequest(); err != nil {
			p.requestErr = err
		}
	case ScreenRecording:
		if err := sys.ScreenRequest(); err != nil {
			p.requestErr = err
		}
	}
	var out []Check
	for _, g := range o.Checks {
		out = append(out, p.check(g, o.Request == g))
	}
	return out
}

type prober struct {
	ctx        context.Context
	sys        System
	fda        *Check
	requestErr error
}

func (p *prober) check(g string, requested bool) Check {
	c := Check{Grant: g}
	switch {
	case g == Domain:
		name, err := p.sys.ManagerName(p.ctx)
		switch {
		case err != nil:
			c.State, c.Detail = api.PermissionStateError, "launchctl managername: "+err.Error()
		case name == "Aqua":
			c.State, c.Detail = api.PermissionStateGranted, "Aqua"
		default:
			c.State, c.Detail = api.PermissionStateError, fmt.Sprintf("launchd domain %q, not Aqua: log in at the Mac's screen", name)
		}
	case g == FullDiskAccess:
		c = p.fullDisk()
	case g == FilesAndFolders:
		c = p.files(requested)
	case g == Accessibility:
		ok, err := p.sys.AXTrusted()
		c = boolCheck(g, ok, err)
		if p.requestErr != nil && requested {
			c.Detail = "the request failed: " + p.requestErr.Error()
		}
	case g == ScreenRecording:
		ok, err := p.sys.ScreenPreflight()
		c = boolCheck(g, ok, err)
		if p.requestErr != nil && requested {
			c.Detail = "the request failed: " + p.requestErr.Error()
		}
	case strings.HasPrefix(g, AutomationPrefix):
		c = automation(g, p.sys)
	case g == LocalNetwork:
		if !requested {
			c.State, c.Detail = api.PermissionStateUnknown, "no check exists without a connection; gff setup makes one"
			break
		}
		c = localNetwork(p.ctx, p.sys)
	case g == Pasteboard:
		c = pasteboard(p.ctx, p.sys)
	case g == DeveloperTools:
		c.State, c.Detail = api.PermissionStateUnknown, "no API can check it; see Settings"
	default:
		c.State, c.Detail = api.PermissionStateError, "the probe does not check this grant"
	}
	return c
}

func boolCheck(g string, ok bool, err error) Check {
	switch {
	case err != nil:
		return Check{Grant: g, State: api.PermissionStateError, Detail: err.Error()}
	case ok:
		return Check{Grant: g, State: api.PermissionStateGranted}
	}
	return Check{Grant: g, State: api.PermissionStateDenied}
}

// fullDisk opens FDAPath on its own goroutine with a deadline (7.3).
func (p *prober) fullDisk() Check {
	if p.fda == nil {
		c := OpenCheck(p.ctx, p.sys, FullDiskAccess, FDAPath, FDATimeout)
		p.fda = &c
	}
	return *p.fda
}

// OpenCheck opens dir on its own goroutine and reads the answer: granted when it opens, denied
// on a permission error, unanswered when nothing answers within timeout. A goroutine stuck in
// the open is left behind: the probe exits soon after.
func OpenCheck(ctx context.Context, sys System, grant, dir string, timeout time.Duration) Check {
	done := make(chan error, 1)
	go func() { done <- sys.OpenDir(dir) }()
	t := time.NewTimer(timeout)
	defer t.Stop()
	c := Check{Grant: grant}
	select {
	case err := <-done:
		switch {
		case err == nil:
			c.State = api.PermissionStateGranted
		case errors.Is(err, fs.ErrPermission):
			c.State = api.PermissionStateDenied
		case errors.Is(err, fs.ErrNotExist):
			c.State, c.Detail = api.PermissionStateUnknown, dir+" does not exist"
		default:
			c.State, c.Detail = api.PermissionStateUnknown, err.Error()
		}
	case <-t.C:
		c.State, c.Detail = api.PermissionStateUnanswered, fmt.Sprintf("no answer in %v", timeout)
	case <-ctx.Done():
		c.State, c.Detail = api.PermissionStateUnanswered, "the probe ran out of time"
	}
	return c
}

// files answers Files and Folders. With Full Disk Access it is covered. Without, an open of an
// undecided folder raises a dialog, so only a request opens them (7.3).
func (p *prober) files(requested bool) Check {
	if p.fullDisk().State == api.PermissionStateGranted {
		return Check{Grant: FilesAndFolders, State: api.PermissionStateGranted, Detail: "covered by Full Disk Access"}
	}
	if !requested {
		return Check{Grant: FilesAndFolders, State: api.PermissionStateUnknown, Detail: "cannot be checked without risking a dialog; gff setup opens each folder"}
	}
	type res struct {
		name string
		c    Check
	}
	ch := make(chan res, len(ProtectedFolders))
	for _, f := range ProtectedFolders {
		go func() {
			ch <- res{f, OpenCheck(p.ctx, p.sys, FilesAndFolders, filepath.Join(p.sys.Home(), f), RequestTimeout)}
		}()
	}
	var denied, unanswered, unknown []string
	for range ProtectedFolders {
		r := <-ch
		switch r.c.State {
		case api.PermissionStateDenied:
			denied = append(denied, r.name)
		case api.PermissionStateUnanswered:
			unanswered = append(unanswered, r.name)
		case api.PermissionStateUnknown:
			unknown = append(unknown, r.name)
		}
	}
	switch {
	case len(denied) > 0:
		return Check{Grant: FilesAndFolders, State: api.PermissionStateDenied, Detail: "denied: " + strings.Join(sorted(denied), ", ")}
	case len(unanswered) > 0:
		return Check{Grant: FilesAndFolders, State: api.PermissionStateUnanswered, Detail: "no answer for " + strings.Join(sorted(unanswered), ", ")}
	case len(unknown) > 0:
		return Check{Grant: FilesAndFolders, State: api.PermissionStateUnknown, Detail: "could not open " + strings.Join(sorted(unknown), ", ")}
	}
	return Check{Grant: FilesAndFolders, State: api.PermissionStateGranted}
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

func automation(g string, sys System) Check {
	target := strings.TrimPrefix(g, AutomationPrefix)
	st, err := sys.Automation(target)
	c := Check{Grant: g}
	switch {
	case err != nil:
		c.State, c.Detail = api.PermissionStateError, err.Error()
	case st == osNoErr:
		c.State = api.PermissionStateGranted
	case st == errAEEventNotPermitted:
		c.State = api.PermissionStateDenied
	case st == errAEEventWouldRequire:
		c.State, c.Detail = api.PermissionStateUnknown, "undecided or denied: macOS does not say which"
	case st == procNotFound:
		c.State, c.Detail = api.PermissionStateUnknown, target+" is not running"
	case st == errAETargetAddressError:
		c.State, c.Detail = api.PermissionStateUnknown, target+" is not an app macOS knows"
	default:
		c.State, c.Detail = api.PermissionStateUnknown, fmt.Sprintf("OSStatus %d", st)
	}
	return c
}

// localNetwork connects to the default gateway (7.3). A connection, or a refusal from the
// gateway, means packets reach the LAN. No route means Local Network may be off. The probe stays
// alive up to LANKeepAlive, trying every 2 s, so macOS can show its alert.
func localNetwork(ctx context.Context, sys System) Check {
	c := Check{Grant: LocalNetwork}
	gw, err := sys.Gateway(ctx)
	if err != nil {
		c.State, c.Detail = api.PermissionStateUnknown, "no default gateway: "+err.Error()
		return c
	}
	addr := net.JoinHostPort(gw, "443")
	deadline := time.Now().Add(lanWindow)
	for {
		dctx, cancel := context.WithTimeout(ctx, dialTimeout)
		err := sys.Dial(dctx, addr)
		cancel()
		switch {
		case err == nil:
			c.State, c.Detail = api.PermissionStateGranted, "connected to the gateway "+gw
			return c
		case errors.Is(err, syscall.ECONNREFUSED):
			c.State, c.Detail = api.PermissionStateGranted, "the gateway "+gw+" answered"
			return c
		case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EPERM):
			c.State, c.Detail = api.PermissionStateDenied, "no route to the gateway "+gw+": Local Network may be off"
		default:
			c.State, c.Detail = api.PermissionStateUnknown, "the gateway "+gw+" did not answer"
		}
		if !time.Now().Add(lanRetry).Before(deadline) {
			return c
		}
		select {
		case <-ctx.Done():
			return c
		case <-time.After(lanRetry):
		}
	}
}

// pasteboard writes a random token and reads it back, then puts the old text back (7.3). Only a
// request or an explicit grant runs it: it replaces the clipboard for a moment, and a clipboard
// that held something other than text loses it.
func pasteboard(ctx context.Context, sys System) Check {
	c := Check{Grant: Pasteboard}
	old, oldErr := sys.Clipboard(ctx)
	tok := "gff-probe-" + NewNonce()
	if err := sys.SetClipboard(ctx, tok); err != nil {
		c.State, c.Detail = api.PermissionStateError, "pbcopy: "+err.Error()
		return c
	}
	got, err := sys.Clipboard(ctx)
	if oldErr == nil {
		sys.SetClipboard(ctx, old)
	}
	switch {
	case err != nil:
		c.State, c.Detail = api.PermissionStateDenied, "pbpaste: "+err.Error()
	case strings.TrimSpace(got) == tok:
		c.State = api.PermissionStateGranted
	default:
		c.State, c.Detail = api.PermissionStateDenied, "pbpaste did not return the token"
	}
	return c
}
