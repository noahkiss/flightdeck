package mcpreg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/internal/push"
)

// Secret resolution at launch (spec 8.5). `gff-hab mcp launch` and `gff-hab mcp header` read
// the registry themselves and resolve each secret or command value in their own process; the
// values exist only in that process's memory and, for launch, in the environment of the server
// it execs. Nothing here logs a value, writes one to a file, or puts one in an error text.

// DefaultSecretTimeout bounds one provider or command run when the config sets none. Claude
// gives up on a header helper after 10 s, so the default stays under it.
const DefaultSecretTimeout = 8 * time.Second

// maxSecretOutput bounds what one provider run may print.
const maxSecretOutput = 64 << 10

// SecretRunner runs a command that prints a secret: the argv with no shell, stdin from
// /dev/null, its own process group, killed past timeout. use reads stdout. Its errors never
// hold stdout or stderr. push.RunSecretNoStderr is the real one; tests give a fake.
type SecretRunner func(ctx context.Context, what string, argv []string, timeout time.Duration, limit int, use func([]byte) error) error

// Resolver resolves a server's values at launch.
type Resolver struct {
	Config Config
	Lookup func(string) (string, bool) // the launcher's own environment; nil: os.LookupEnv
	Run    SecretRunner                // nil: push.RunSecretNoStderr
}

func (r *Resolver) lookup(k string) (string, bool) {
	if r.Lookup == nil {
		return os.LookupEnv(k)
	}
	return r.Lookup(k)
}

// resolve returns one value. kind and key name it in an error ("env DOCS_TOKEN").
func (r *Resolver) resolve(ctx context.Context, server, kind, key string, v Value) (string, error) {
	where := fmt.Sprintf("mcp server %s: %s %s", server, kind, key)
	switch {
	case v.IsLiteral():
		return v.Literal, nil
	case v.Env != "":
		val, ok := r.lookup(v.Env)
		if !ok {
			return "", fmt.Errorf("%s: variable %s is not set", where, v.Env)
		}
		return v.Prefix + val, nil
	}
	var argv []string
	timeout := DefaultSecretTimeout
	what := where
	if v.Secret != "" {
		name, ref, _ := strings.Cut(v.Secret, ":")
		p, ok := r.Config.provider(name)
		if !ok {
			return "", fmt.Errorf("%s: no provider %q", where, name)
		}
		argv = append(append([]string(nil), p.Command...), ref)
		if p.Timeout > 0 {
			timeout = time.Duration(p.Timeout) * time.Second
		}
		what = fmt.Sprintf("%s: provider %s", where, name)
	} else {
		argv = v.Command
		what = where + ": command"
	}
	run := r.Run
	if run == nil {
		run = push.RunSecretNoStderr
	}
	var out string
	err := run(ctx, what, argv, timeout, maxSecretOutput, func(b []byte) error {
		s := string(b)
		s = strings.TrimSuffix(s, "\n")
		s = strings.TrimSuffix(s, "\r")
		switch {
		case s == "":
			return errors.New("printed nothing")
		case strings.ContainsRune(s, 0):
			return errors.New("printed a NUL byte")
		case kind == "header" && strings.ContainsAny(s, "\r\n"):
			return errors.New("printed more than one line, which a header cannot hold")
		}
		out = s
		return nil
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

// Launch is what `gff-hab mcp launch` execs: the server's program, its argv, its folder and
// the environment with every value resolved.
type Launch struct {
	Path string   // the program, found on the environment's PATH
	Argv []string // argv[0] is the server's command as configured
	Dir  string   // "" keeps the launcher's folder
	Env  []string // KEY=value; holds resolved secrets
}

// Launch resolves a stdio server's values. base is the launcher's own environment
// (os.Environ()); the server's env values go over it.
func (r *Resolver) Launch(ctx context.Context, name string, base []string) (Launch, error) {
	s, ok := r.Config.Server(name)
	if !ok {
		return Launch{}, fmt.Errorf("mcp launch: no server %q in the registry", name)
	}
	if s.Transport != Stdio {
		return Launch{}, fmt.Errorf("mcp launch: server %s is http; harnesses reach it through gff-hab mcp header", name)
	}
	set := map[string]string{}
	for _, k := range sortedKeys(s.Env) {
		v, err := r.resolve(ctx, s.Name, "env", k, s.Env[k])
		if err != nil {
			return Launch{}, err
		}
		set[k] = v
	}
	env := make([]string, 0, len(base)+len(set))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if _, over := set[k]; !over {
			env = append(env, kv)
		}
	}
	for _, k := range sortedKeys(set) {
		env = append(env, k+"="+set[k])
	}
	path, err := lookPath(s.Command, s.Cwd, envValue(env, "PATH"))
	if err != nil {
		return Launch{}, fmt.Errorf("mcp launch: server %s: %v", name, err)
	}
	return Launch{Path: path, Argv: append([]string{s.Command}, s.Args...), Dir: s.Cwd, Env: env}, nil
}

// Headers resolves an http server's secret and command headers, the ones the rendered entry
// leaves to the header helper. Plain and env headers stay in the entry itself.
func (r *Resolver) Headers(ctx context.Context, name string) (map[string]string, error) {
	s, ok := r.Config.Server(name)
	if !ok {
		return nil, fmt.Errorf("mcp header: no server %q in the registry", name)
	}
	if s.Transport != HTTP {
		return nil, fmt.Errorf("mcp header: server %s is stdio; harnesses start it through gff-hab mcp launch", name)
	}
	out := map[string]string{}
	for _, k := range sortedKeys(s.Headers) {
		v := s.Headers[k]
		if !v.NeedsLaunch() {
			continue
		}
		val, err := r.resolve(ctx, s.Name, "header", k, v)
		if err != nil {
			return nil, err
		}
		out[k] = val
	}
	return out, nil
}

func envValue(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
			return v
		}
	}
	return ""
}

// lookPath finds a program the way a shell would, on the given PATH: a name with a slash is
// taken as it is, relative to dir.
func lookPath(prog, dir, path string) (string, error) {
	if strings.Contains(prog, "/") {
		p := prog
		if !filepath.IsAbs(p) && dir != "" {
			p = filepath.Join(dir, p)
		}
		if executable(p) {
			return p, nil
		}
		return "", fmt.Errorf("%s is not an executable file", prog)
	}
	for _, d := range filepath.SplitList(path) {
		if d == "" {
			d = "."
		}
		p := filepath.Join(d, prog)
		if !filepath.IsAbs(p) && dir != "" {
			p = filepath.Join(dir, p)
		}
		if executable(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found on PATH", prog)
}

func executable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

// launchEnvNames lists the variables a launcher entry must pass to the launcher under Codex,
// which gives a stdio server only a default set: each env reference's variable and each
// variable a provider the server uses reads.
func (c Config) launchEnvNames(s Server) []string {
	seen := map[string]bool{}
	for _, m := range []map[string]Value{s.Env, s.Headers} {
		for _, v := range m {
			if v.Env != "" {
				seen[v.Env] = true
			}
			if p, ok := c.provider(v.provider()); ok && v.Secret != "" {
				for _, e := range p.Env {
					seen[e] = true
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
