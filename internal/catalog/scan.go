package catalog

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/gitstatus"
	"github.com/noahkiss/flightdeck/internal/structure"
)

const (
	// scanDepth is how deep below a project root the scan looks for repositories (mesh.md 5.1).
	scanDepth = 3
	// maxFolders bounds the folders one scan reads.
	maxFolders = 500
	// folderTimeout bounds the git reads of one folder.
	folderTimeout = 10 * time.Second
	// summaryMax bounds the summary line.
	summaryMax = 200
)

// Folder is what a scan read from one repository folder.
type Folder struct {
	Remote     string // normalized (structure.NormalizeRemote)
	RootCommit string // only when there is no remote
	Summary    string
	State      api.CatalogFolder
}

// FindRepos walks each root to scanDepth and returns the repository folders it finds, in walk
// order. It follows no symbolic links, skips hidden folders below a root, and does not look
// inside a repository.
func FindRepos(roots []string) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if len(out) >= maxFolders {
			return
		}
		if isRepo(dir) {
			if !seen[dir] {
				seen[dir] = true
				out = append(out, dir)
			}
			return
		}
		if depth >= scanDepth {
			return
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range ents {
			if !e.IsDir() || e.Type()&os.ModeSymlink != 0 || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			walk(filepath.Join(dir, e.Name()), depth+1)
		}
	}
	for _, r := range roots {
		if fi, err := os.Lstat(r); err != nil || !fi.IsDir() {
			continue
		}
		walk(r, 0)
	}
	return out
}

// isRepo reports whether dir is the top of a git working tree: it holds .git, a folder or a
// file (a worktree or a submodule).
func isRepo(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// ReadFolder reads one repository folder: its identity, as gff-hab files a project (the remote
// of origin, else of the first remote, else the root commit), its state and its summary. Every
// git command is a read, in gitstatus's clean environment. ok is false when dir is not a
// repository or has neither a remote nor a commit.
func ReadFolder(ctx context.Context, dir string) (Folder, bool) {
	ctx, cancel := context.WithTimeout(ctx, folderTimeout)
	defer cancel()
	var r gitstatus.Reader
	st, err := r.Read(ctx, dir)
	if err != nil || len(st.Watch) == 0 {
		return Folder{}, false
	}
	f := Folder{State: api.CatalogFolder{Path: dir, Branch: st.State.Branch, Head: st.State.Head}}
	remote, err := r.Run(ctx, dir, "remote", "get-url", "origin")
	if err != nil {
		remote = ""
		if list, err := r.Run(ctx, dir, "remote"); err == nil && list != "" {
			first, _, _ := strings.Cut(list, "\n")
			remote, _ = r.Run(ctx, dir, "remote", "get-url", first)
		}
	}
	f.Remote = structure.NormalizeRemote(remote)
	if f.Remote == "" && st.State.Head != "" {
		if roots, err := r.Run(ctx, dir, "rev-list", "--max-parents=0", "HEAD"); err == nil && roots != "" {
			list := strings.Fields(roots)
			slices.Sort(list)
			f.RootCommit = list[0]
		}
	}
	if f.Remote == "" && f.RootCommit == "" {
		return Folder{}, false
	}
	if out, err := r.Run(ctx, dir, "status", "--porcelain", "--untracked-files=normal"); err == nil {
		dirty := out != ""
		f.State.Dirty = &dirty
	}
	if st.State.Head != "" {
		if t, err := r.Run(ctx, dir, "log", "-1", "--format=%cI", "HEAD"); err == nil {
			if pt, err := time.Parse(time.RFC3339, t); err == nil {
				f.State.LastCommit = pt.UTC().Format(time.RFC3339)
			}
		}
	}
	f.Summary = summary(dir)
	return f, true
}

// summary is the first non-empty line of AGENTS.md, else of a README, with heading marks
// trimmed.
func summary(dir string) string {
	for _, name := range []string{"AGENTS.md", "README.md", "README", "README.txt"} {
		if s := firstLine(filepath.Join(dir, name)); s != "" {
			return s
		}
	}
	return ""
}

func firstLine(path string) string {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 16<<10))
	for sc.Scan() {
		s := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(sc.Text()), "#"))
		if s == "" {
			continue
		}
		if len(s) > summaryMax {
			s = strings.ToValidUTF8(s[:summaryMax], "") // drop a rune the cut split
		}
		return s
	}
	return ""
}
