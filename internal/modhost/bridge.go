package modhost

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dop251/goja"
)

// Bridge limits (modules.md 7.3).
const (
	maxParamsBytes = 1 << 20 // hab params, a fetch request body
	maxArgv        = 256
	maxStateKey    = 200
	maxLogLine     = 2000 // runes
)

// installBridge sets the global fd object, the whole surface a module may call (modules.md
// 7.1). Nothing else is added to the global object: goja itself has no file, network, process
// or timer API.
func (m *Module) installBridge() error {
	r := m.r
	fd := r.NewObject()
	hab := r.NewObject()
	state := r.NewObject()
	fn := func(f func(goja.FunctionCall) goja.Value) goja.Value { return r.ToValue(f) }

	_ = hab.Set("get", fn(func(c goja.FunctionCall) goja.Value {
		cur := m.need("fd.hab.get", "")
		method, params := m.habArgs("fd.hab.get", c)
		if m.opts.Hab == nil {
			m.throw(BridgeError, "fd.hab.get: this host gives modules no hab access")
		}
		res, err := m.opts.Hab.Get(cur.ctx, method, params)
		return m.result("fd.hab.get", res, err)
	}))
	_ = hab.Set("act", fn(func(c goja.FunctionCall) goja.Value {
		cur := m.need("fd.hab.act", "act")
		method, params := m.habArgs("fd.hab.act", c)
		if m.opts.Hab == nil {
			m.throw(BridgeError, "fd.hab.act: this host gives modules no hab access")
		}
		res, err := m.opts.Hab.Act(cur.ctx, method, params)
		return m.result("fd.hab.act", res, err)
	}))
	_ = fd.Set("hab", hab)

	_ = fd.Set("exec", fn(func(c goja.FunctionCall) goja.Value {
		cur := m.need("fd.exec", "exec")
		req := m.execArgs(cur, c)
		if m.opts.Exec == nil {
			m.throw(BridgeError, "fd.exec: this host runs no programs for modules")
		}
		res, err := m.opts.Exec.Run(cur.ctx, req)
		if err != nil {
			m.throw(BridgeError, "fd.exec: "+err.Error())
		}
		out := r.NewObject()
		stdout, outCut := cut(res.Stdout, m.limits.Output)
		stderr, errCut := cut(res.Stderr, m.limits.Output)
		_ = out.Set("code", res.Code)
		_ = out.Set("stdout", stdout)
		_ = out.Set("stderr", stderr)
		_ = out.Set("stdout_cut", outCut)
		_ = out.Set("stderr_cut", errCut)
		return out
	}))

	_ = fd.Set("fetch", fn(func(c goja.FunctionCall) goja.Value {
		cur := m.need("fd.fetch", "fetch")
		req := m.fetchArgs(c)
		if m.opts.Fetch == nil {
			m.throw(BridgeError, "fd.fetch: this host makes no requests for modules")
		}
		res, err := m.opts.Fetch.Do(cur.ctx, req)
		if err != nil {
			m.throw(BridgeError, "fd.fetch: "+err.Error())
		}
		out := r.NewObject()
		headers := r.NewObject()
		for _, k := range slices.Sorted(maps.Keys(res.Headers)) {
			_ = headers.Set(strings.ToLower(k), res.Headers[k])
		}
		body, bodyCut := cut(res.Body, m.limits.Output)
		_ = out.Set("status", res.Status)
		_ = out.Set("headers", headers)
		_ = out.Set("body", body)
		_ = out.Set("body_cut", bodyCut)
		return out
	}))

	_ = state.Set("get", fn(func(c goja.FunctionCall) goja.Value {
		key := m.stateKey("fd.state.get", c.Argument(0))
		raw, ok := m.state[key]
		if !ok {
			return goja.Undefined()
		}
		v, err := m.importJSON(raw)
		if err != nil {
			m.throw(BridgeError, "fd.state.get: "+err.Error())
		}
		return v
	}))
	_ = state.Set("set", fn(func(c goja.FunctionCall) goja.Value {
		m.need("fd.state.set", "")
		key := m.stateKey("fd.state.set", c.Argument(0))
		m.setState(key, c.Argument(1))
		return goja.Undefined()
	}))
	_ = fd.Set("state", state)

	_ = fd.Set("log", fn(func(c goja.FunctionCall) goja.Value {
		parts := make([]string, len(c.Arguments))
		for i, a := range c.Arguments {
			if s, ok := a.Export().(string); ok {
				parts[i] = s
				continue
			}
			if b, err := exportJSON(r, a, maxLogLine*4); err == nil {
				parts[i] = string(b)
			} else {
				parts[i] = a.String()
			}
		}
		line := strings.Join(parts, " ")
		if utf8.RuneCountInString(line) > maxLogLine {
			line = string([]rune(line)[:maxLogLine-1]) + "…"
		}
		if m.opts.Log != nil {
			m.opts.Log(line)
		}
		return goja.Undefined()
	}))

	settings, err := m.settingsValue()
	if err != nil {
		return err
	}
	_ = fd.Set("settings", settings)
	_, _ = m.freeze(goja.Undefined(), hab)
	_, _ = m.freeze(goja.Undefined(), state)
	_, _ = m.freeze(goja.Undefined(), fd)
	return r.Set("fd", fd)
}

// throw ends the bridge call with a JS error the module may catch.
func (m *Module) throw(name, msg string) {
	panic(m.newError(name, msg))
}

// need checks that a bridge call is allowed now: inside refresh or act (render and load may only
// log, read settings and read state, modules.md 5), and with its power declared.
func (m *Module) need(call, power string) *callState {
	cur := m.cur
	if cur == nil {
		m.throw(BridgeError, call+": no call is running")
	}
	if power != "" && !m.powers[power] {
		m.throw(PowerError, fmt.Sprintf("%s needs the %s power, which the manifest does not declare", call, power))
	}
	if cur.phase == phaseRender || cur.phase == phaseLoad {
		m.throw(BridgeError, fmt.Sprintf("%s: not allowed in %s; call it in refresh or act and keep what render needs in fd.state", call, cur.phase))
	}
	return cur
}

func (m *Module) habArgs(call string, c goja.FunctionCall) (string, json.RawMessage) {
	method, ok := c.Argument(0).Export().(string)
	if !ok || method == "" {
		m.throw(BridgeError, call+": the first argument is a method name, such as \"session.list\"")
	}
	p := c.Argument(1)
	if goja.IsUndefined(p) || goja.IsNull(p) {
		return method, json.RawMessage("{}")
	}
	if o, ok := p.(*goja.Object); !ok || o.ClassName() == "Array" {
		m.throw(BridgeError, call+": params is an object")
	}
	b, err := exportJSON(m.r, p, maxParamsBytes)
	if err != nil {
		m.throw(BridgeError, call+": params: "+err.Error())
	}
	return method, b
}

// result hands a backend's JSON answer to JS, or throws its error.
func (m *Module) result(call string, res json.RawMessage, err error) goja.Value {
	if err != nil {
		m.throw(BridgeError, call+": "+err.Error())
	}
	v, err := m.importJSON(res)
	if err != nil {
		m.throw(BridgeError, call+": the answer is not JSON")
	}
	return v
}

// options reads an options object with only the named keys.
func (m *Module) options(call string, v goja.Value, keys ...string) *goja.Object {
	if goja.IsUndefined(v) || goja.IsNull(v) {
		return nil
	}
	o, ok := v.(*goja.Object)
	if !ok || o.ClassName() == "Array" {
		m.throw(BridgeError, call+": options is an object")
	}
	for _, k := range o.Keys() {
		if !slices.Contains(keys, k) {
			m.throw(BridgeError, fmt.Sprintf("%s: options has no %q; it takes %s", call, k, strings.Join(keys, ", ")))
		}
	}
	return o
}

func (m *Module) execArgs(cur *callState, c goja.FunctionCall) ExecRequest {
	argvV, ok := c.Argument(0).(*goja.Object)
	if !ok || argvV.ClassName() != "Array" {
		m.throw(BridgeError, "fd.exec: the first argument is the argv array, such as [\"git\", \"status\"]")
	}
	n := argvV.Get("length").ToInteger()
	if n < 1 || n > maxArgv {
		m.throw(BridgeError, fmt.Sprintf("fd.exec: argv has 1 to %d items", maxArgv))
	}
	var req ExecRequest
	for i := int64(0); i < n; i++ {
		s, ok := argvV.Get(fmt.Sprint(i)).Export().(string)
		if !ok || strings.IndexByte(s, 0) >= 0 || (i == 0 && s == "") {
			m.throw(BridgeError, "fd.exec: argv holds strings, the first not empty")
		}
		req.Argv = append(req.Argv, s)
	}
	remaining := time.Until(deadline(cur.ctx))
	req.Timeout = remaining
	if o := m.options("fd.exec", c.Argument(1), "cwd", "timeout"); o != nil {
		if v := o.Get("cwd"); v != nil && !goja.IsUndefined(v) {
			s, ok := v.Export().(string)
			if !ok {
				m.throw(BridgeError, "fd.exec: cwd is a path")
			}
			req.Dir = s
		}
		if v := o.Get("timeout"); v != nil && !goja.IsUndefined(v) {
			secs := v.ToFloat()
			if math.IsNaN(secs) || secs <= 0 {
				m.throw(BridgeError, "fd.exec: timeout is a number of seconds above 0")
			}
			if d := time.Duration(secs * float64(time.Second)); d < req.Timeout {
				req.Timeout = d
			}
		}
	}
	return req
}

func deadline(ctx context.Context) time.Time {
	d, ok := ctx.Deadline()
	if !ok {
		return time.Now().Add(time.Hour)
	}
	return d
}

func (m *Module) fetchArgs(c goja.FunctionCall) FetchRequest {
	s, ok := c.Argument(0).Export().(string)
	u, err := url.Parse(s)
	if !ok || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		m.throw(BridgeError, "fd.fetch: the first argument is an http or https URL")
	}
	req := FetchRequest{URL: s, Method: "GET"}
	o := m.options("fd.fetch", c.Argument(1), "method", "headers", "body")
	if o == nil {
		return req
	}
	if v := o.Get("method"); v != nil && !goja.IsUndefined(v) {
		meth, ok := v.Export().(string)
		if !ok || meth == "" || strings.ContainsAny(meth, " \t\r\n") {
			m.throw(BridgeError, "fd.fetch: method is a word such as GET or POST")
		}
		req.Method = strings.ToUpper(meth)
	}
	if v := o.Get("headers"); v != nil && !goja.IsUndefined(v) {
		ho, ok := v.(*goja.Object)
		if !ok || ho.ClassName() == "Array" {
			m.throw(BridgeError, "fd.fetch: headers is an object of strings")
		}
		req.Headers = map[string]string{}
		for _, k := range ho.Keys() {
			hv, ok := ho.Get(k).Export().(string)
			if !ok || strings.ContainsAny(k+hv, "\r\n") {
				m.throw(BridgeError, "fd.fetch: headers is an object of strings")
			}
			req.Headers[k] = hv
		}
	}
	if v := o.Get("body"); v != nil && !goja.IsUndefined(v) {
		body, ok := v.Export().(string)
		if !ok || len(body) > maxParamsBytes {
			m.throw(BridgeError, fmt.Sprintf("fd.fetch: body is a string of at most %d bytes", maxParamsBytes))
		}
		req.Body = body
	}
	return req
}

func (m *Module) stateKey(call string, v goja.Value) string {
	k, ok := v.Export().(string)
	if !ok || k == "" || len(k) > maxStateKey {
		m.throw(BridgeError, fmt.Sprintf("%s: a key is a string of 1 to %d bytes", call, maxStateKey))
	}
	return k
}

// setState stores one key, or deletes it when the value is undefined, then saves the module's
// state through the Store. A save that fails, or a state over Limits.State, changes nothing.
func (m *Module) setState(key string, v goja.Value) {
	old, had := m.state[key]
	size := m.size
	if had {
		size -= len(key) + len(old)
	}
	next := maps.Clone(m.state)
	if goja.IsUndefined(v) {
		delete(next, key)
	} else {
		b, err := exportJSON(m.r, v, m.limits.State)
		if err != nil {
			m.throw(BridgeError, "fd.state.set: "+err.Error())
		}
		size += len(key) + len(b)
		if size > m.limits.State {
			m.throw(BridgeError, fmt.Sprintf("fd.state.set: the module's state would be %d bytes; the limit is %d", size, m.limits.State))
		}
		next[key] = b
	}
	if m.opts.State != nil {
		if err := m.opts.State.Save(m.b.Manifest.ID, next); err != nil {
			m.throw(BridgeError, "fd.state.set: "+err.Error())
		}
	}
	m.state, m.size = next, size
}

// settingsValue builds the frozen fd.settings: each declared setting, the person's value or the
// manifest's default. A setting with neither is absent.
func (m *Module) settingsValue() (goja.Value, error) {
	vals := map[string]any{}
	for _, s := range m.b.Manifest.Settings {
		if v, ok := m.opts.Settings[s.Key]; ok {
			vals[s.Key] = v
		} else if s.Default != nil {
			vals[s.Key] = s.Default
		}
	}
	b, err := json.Marshal(vals)
	if err != nil {
		return nil, fmt.Errorf("module settings: %w", err)
	}
	v, err := m.importJSON(b)
	if err != nil {
		return nil, err
	}
	_, _ = m.freeze(goja.Undefined(), v)
	return v, nil
}

// cut returns at most max bytes of b as a string, ending on a whole UTF-8 sequence, and whether
// it cut.
func cut(b []byte, max int) (string, bool) {
	if len(b) <= max {
		return string(b), false
	}
	b = b[:max]
	if i := lastStart(b); !utf8.FullRune(b[i:]) {
		b = b[:i]
	}
	return string(b), true
}

// lastStart is the index of the last UTF-8 start byte in b.
func lastStart(b []byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if utf8.RuneStart(b[i]) {
			return i
		}
	}
	return 0
}
