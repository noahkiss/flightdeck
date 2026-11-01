package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/content"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// contentFile holds the shown references in the state directory (app.md 4.7).
const contentFile = "content.json"

// attachmentsFile indexes the files the owner sent (app.md 4.4).
const attachmentsFile = "attachments.json"

// conversation returns a session's chat messages and the folder its transcript records, for
// rich content. A session with no transcript (no agent yet, a harness with no reader) has none.
func (c *chatService) conversation(ctx context.Context, id string) ([]api.ChatMessage, string, error) {
	r, err := c.reader(id)
	if err != nil {
		var ce *control.Error
		if errors.As(err, &ce) && ce.Data.Kind == api.ErrorKindUnavailable {
			return nil, "", nil
		}
		return nil, "", err
	}
	if r == nil {
		return nil, "", nil
	}
	// A source that cannot list its whole conversation (opencode's) derives nothing.
	all, ok := r.(conversationSource)
	if !ok {
		return nil, "", nil
	}
	msgs, err := all.All(ctx)
	return msgs, all.Cwd(), err
}

// conversationSource is a chat source that returns its whole conversation and the folder its
// transcript records: the Claude Code and Codex readers.
type conversationSource interface {
	All(ctx context.Context) ([]api.ChatMessage, error)
	Cwd() string
}

// projectDirOf returns the most recently used folder of a session's project, or "".
func projectDirOf(store *structure.Store) func(session string) string {
	return func(session string) string {
		s, ok := store.Session(session)
		if !ok || s.Project == "" {
			return ""
		}
		for _, pd := range store.ProjectDirs() {
			if pd.ID == s.Project && len(pd.Dirs) > 0 {
				return pd.Dirs[0]
			}
		}
		return ""
	}
}

// paneSessionOf maps a pane's GFF_HAB token to its session.
func paneSessionOf(tokens *hook.Tokens, store *structure.Store) func(token string) (string, bool) {
	return func(token string) (string, bool) {
		pane, ok := tokens.Pane(token)
		if !ok {
			return "", false
		}
		s, ok := store.SessionOfPane(pane)
		return s.ID, ok
	}
}

// newContent opens the shown references in dir and returns the content service.
func newContent(dir string, cfg content.Config) (*content.Service, error) {
	st, err := content.OpenStore(filepath.Join(dir, contentFile))
	var bad *content.CorruptError
	if errors.As(err, &bad) {
		cfg.Logf("content: %v", err) // a store of references never stops the hab
	} else if err != nil {
		return nil, err
	}
	cfg.Store = st
	// References of sessions whose record is gone go at start.
	if err := st.Prune(func(id string) bool { _, ok := cfg.Session(id); return ok }); err != nil {
		cfg.Logf("content: %v", err)
	}
	return content.New(cfg), nil
}

// showCmd stores a reference to a file for a session to show: gff-hab show [flags] <path>.
// With no -session it names the session by the pane it runs in ($GFF_HAB).
func showCmd(args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	sock := socketFlag(fs)
	via := fs.String("via", "", "run this command and speak over its stdio, e.g. \"ssh <host> gff-hab connect\"")
	session := fs.String("session", "", "the session to show it in (default: the session of this pane, from $GFF_HAB)")
	kind := fs.String("kind", "", "image, markdown, pdf, html, text, diff or file (default: from the file name)")
	title := fs.String("title", "", "a title to show instead of the file name")
	timeout := fs.Duration("timeout", 10*time.Second, "give up after this long")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("show needs one path")
	}
	path, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		return err
	}
	p := api.ContentShowParams{Session: *session, Path: path, Kind: api.ContentKind(*kind), Title: *title}
	if p.Session == "" {
		hookSock, token, err := hook.ParseAddr(os.Getenv(hook.Env))
		if err != nil {
			return errors.New("show needs -session outside a pane gff-hab started ($GFF_HAB is not set)")
		}
		p.PaneToken = token
		if *sock == "" && *via == "" && os.Getenv("GFF_HAB_SOCKET") == "" {
			// The control socket sits beside the hook socket unless the hab was told otherwise.
			if c := filepath.Join(filepath.Dir(hookSock), "hab.sock"); exists(c) {
				*sock = c
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	c, err := open(*sock, *via)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.Hello(ctx, showClient, "", ""); err != nil {
		return err
	}
	res, err := api.Client{Caller: c}.ContentShow(ctx, p)
	if err != nil {
		return err
	}
	return printJSON(res)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

var showClient = api.ClientInfo{Name: "gff-hab show", Version: build, Kind: api.ClientKindCli}
