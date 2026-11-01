package worktree

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// IncludeFile names the ignored files to copy into a new worktree, in gitignore syntax. It is read
// from the source checkout.
const IncludeFile = ".worktreeinclude"

// ConfigFile is the project file at the repository root. It is read from the worktree, so a new
// worktree uses the version committed on its start point.
const ConfigFile = "flightdeck.json"

// markFile, in a linked worktree's private git folder, records that gff-hab made the worktree and
// whether its scripts may run. Git ignores files it does not know there, and removes the folder
// with the worktree.
const markFile = "gff-worktree.json"

// copyIncludes copies each file in src that git ignores and that a pattern in src's
// .worktreeinclude matches to the same place in dst. A file dst already has is skipped. Regular
// files keep their mode; symbolic links are copied as links; folders (nested repositories) are
// skipped. No .worktreeinclude: nothing is copied.
func (g Git) copyIncludes(ctx context.Context, src, dst string) ([]string, error) {
	inc := filepath.Join(src, IncludeFile)
	if _, err := os.Stat(inc); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	// Untracked files the include patterns match, then those of them git ignores anyway.
	out, err := g.runIn(ctx, src, nil, "ls-files", "-z", "--others", "--ignored", "--exclude-from="+inc)
	if err != nil {
		return nil, err
	}
	var cand []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" && !strings.HasSuffix(p, "/") {
			cand = append(cand, p)
		}
	}
	if len(cand) == 0 {
		return nil, nil
	}
	in := []byte(strings.Join(cand, "\x00") + "\x00")
	out, err = g.runIn(ctx, src, in, "check-ignore", "-z", "--stdin")
	if err != nil && exitCode(err) != 1 { // 1: none of them is ignored
		return nil, err
	}
	// Every write goes through a Root of the new worktree: a folder the checkout holds as a
	// symbolic link to somewhere outside it (committed on a foreign branch, say) is refused, so
	// the user's ignored files never land outside the worktree.
	root, err := os.OpenRoot(dst)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	b := &copyBudget{ctx: ctx, files: MaxIncludeFiles, bytes: MaxIncludeBytes}
	var copied []string
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return copied, err
		}
		ok, err := copyOne(b, filepath.Join(src, rel), root, rel)
		if err != nil {
			return copied, fmt.Errorf("%s: %w", rel, err)
		}
		if ok {
			copied = append(copied, rel)
		}
	}
	return copied, nil
}

// The bounds on what .worktreeinclude may copy into one worktree. Past either, creation fails
// and the worktree is removed again: dependency folders belong in the setup script.
var (
	MaxIncludeFiles       = 10000
	MaxIncludeBytes int64 = 1 << 30
)

// copyBudget counts what copyOne may still copy, and carries the deadline.
type copyBudget struct {
	ctx   context.Context
	files int
	bytes int64
}

// Read implements the deadline and the byte bound for the copy of one file.
type budgetReader struct {
	b *copyBudget
	r io.Reader
}

func (br budgetReader) Read(p []byte) (int, error) {
	if err := br.b.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := br.r.Read(p)
	br.b.bytes -= int64(n)
	if br.b.bytes < 0 {
		return n, fmt.Errorf("the included files pass %d bytes", MaxIncludeBytes)
	}
	return n, err
}

// copyOne copies one file or symbolic link from the source checkout to rel in the worktree
// root. It reports false when it skipped the file: the worktree has one there already.
func copyOne(b *copyBudget, from string, root *os.Root, rel string) (bool, error) {
	fi, err := os.Lstat(from)
	if err != nil {
		return false, err
	}
	if _, err := root.Lstat(rel); err == nil {
		return false, nil // the worktree has it: keep its contents
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err // a parent that leaves the worktree, among others
	}
	if !fi.Mode().IsRegular() && fi.Mode()&fs.ModeSymlink == 0 {
		return false, nil
	}
	if b.files--; b.files < 0 {
		return false, fmt.Errorf("the included files pass %d files", MaxIncludeFiles)
	}
	if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return false, err
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(from)
		if err != nil {
			return false, err
		}
		return true, root.Symlink(target, rel)
	case fi.Mode().IsRegular():
		in, err := os.Open(from)
		if err != nil {
			return false, err
		}
		defer in.Close()
		out, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
		if err != nil {
			return false, err
		}
		if _, err := io.Copy(out, budgetReader{b, in}); err != nil {
			out.Close()
			return false, err
		}
		return true, out.Close()
	}
	return false, nil
}

// Config is the worktree part of a project's flightdeck.json.
type Config struct {
	Setup    string // shell script run once in a new worktree; "" for none
	Teardown string // shell script run in a worktree before it is removed; "" for none
}

// Digest identifies the pair of scripts, so an approval covers these scripts and no others.
func (c Config) Digest() string {
	h := sha256.Sum256([]byte(c.Setup + "\x00" + c.Teardown))
	return hex.EncodeToString(h[:])
}

// Allows reports whether the mark lets cfg's scripts run: approved, for exactly these scripts.
func (m Mark) Allows(cfg Config) bool {
	return m.Approved && m.Digest == cfg.Digest()
}

// script is a shell script, or a list of commands run one after another.
type script string

func (s *script) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*s = script(one)
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return errors.New("must be a string or a list of strings")
	}
	*s = script(strings.Join(list, "\n"))
	return nil
}

// ReadConfig reads flightdeck.json at the top of the worktree dir. No file is the zero Config.
// Unknown keys are ignored, so the file can grow.
func ReadConfig(dir string) (Config, error) {
	b, err := os.ReadFile(filepath.Join(dir, ConfigFile))
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var f struct {
		Worktree struct {
			Setup    script `json:"setup"`
			Teardown script `json:"teardown"`
		} `json:"worktree"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return Config{}, fmt.Errorf("%s: %w", ConfigFile, err)
	}
	return Config{Setup: strings.TrimSpace(string(f.Worktree.Setup)), Teardown: strings.TrimSpace(string(f.Worktree.Teardown))}, nil
}

// Mark is what gff-hab records about a worktree it made.
type Mark struct {
	Source   string    `json:"source"`             // top folder of the source checkout
	Approved bool      `json:"approved"`           // the scripts with Digest may run
	Digest   string    `json:"digest,omitempty"`   // Config.Digest of the scripts approved
	Created  time.Time `json:"created"`            // when gff-hab made it
	Foreign  string    `json:"foreign,omitempty"`  // why setup waited for a yes
	SetupRun time.Time `json:"setup_run,omitzero"` // when setup last started
}

// ReadMark reads the mark of the linked worktree at path. ok is false when there is none: the
// worktree was not made by gff-hab.
func (g Git) ReadMark(ctx context.Context, path string) (m Mark, ok bool, err error) {
	dir, err := g.GitDir(ctx, path)
	if err != nil {
		return Mark{}, false, err
	}
	b, err := os.ReadFile(filepath.Join(dir, markFile))
	if errors.Is(err, fs.ErrNotExist) {
		return Mark{}, false, nil
	}
	if err != nil {
		return Mark{}, false, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return Mark{}, false, fmt.Errorf("%s: %w", markFile, err)
	}
	return m, true, nil
}

// WriteMark writes the mark of the linked worktree at path, whole, through a rename.
func (g Git) WriteMark(ctx context.Context, path string, m Mark) error {
	dir, err := g.GitDir(ctx, path)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, markFile+".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, markFile))
}

// Env is the environment a worktree's scripts get on top of the hab's.
func Env(source, path, branch string) []string {
	return []string{
		"GFF_SOURCE_CHECKOUT=" + source,
		"GFF_WORKTREE=" + path,
		"GFF_BRANCH=" + branch,
	}
}

// ScriptArgv is the argv that runs a setup or teardown script: sh, stopping at the first command
// that fails.
func ScriptArgv(s string) []string {
	return []string{"/bin/sh", "-c", "set -e\n" + s}
}

// maxTail bounds the output RunScript keeps.
const maxTail = 4 << 10

// tailBuffer keeps the last maxTail bytes written to it.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > maxTail {
		t.b = append(t.b[:0], t.b[len(t.b)-maxTail:]...)
	}
	return len(p), nil
}

// RunScript runs a script in dir with env added to the process environment, until it ends or ctx
// does. It returns the exit code (-1 when it did not run to an exit) and the last 4 KiB of its
// output, stdout and stderr together.
func RunScript(ctx context.Context, dir, s string, env []string) (int, string, error) {
	argv := ScriptArgv(s)
	cmd := execCommand(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var tail tailBuffer
	cmd.Stdout, cmd.Stderr = &tail, &tail
	cmd.Stdin = bytes.NewReader(nil)
	cmd.WaitDelay = time.Second
	ownGroup(cmd) // at the deadline the script's children go with it
	err := cmd.Run()
	out := string(tail.b)
	if err == nil {
		return 0, out, nil
	}
	if ctx.Err() != nil {
		return -1, out, fmt.Errorf("stopped: %w", ctx.Err())
	}
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		return cmd.ProcessState.ExitCode(), out, nil
	}
	return -1, out, err
}
