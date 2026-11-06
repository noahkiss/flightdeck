package lbsvc

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/logbook"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// Projects (logbook.md 3, 3.3): identities, first sight, remote changes, and the private flag
// on agents' reads (12.1).

// projectsHint says who changes a project's identities and privacy.
const projectsHint = "gff-hab files a project when a session starts in its folder, and only the user joins projects or marks one private (gff lb project, run outside a pane)"

// Identity is the identity of a repository (3.1): its remote in the catalog's normalised form
// (structure.NormalizeRemote, mesh.md 5.1), else root:<hash> of its root commit, else "".
func Identity(remote, rootCommit string) string {
	if r := structure.NormalizeRemote(remote); r != "" {
		return r
	}
	if rootCommit != "" {
		return "root:" + rootCommit
	}
	return ""
}

// ProjectName is the name a project filed at first sight gets: the repository's name, the last
// part of its remote, else the folder's name.
func ProjectName(identity, dir string) string {
	name := ""
	if identity != "" && !strings.HasPrefix(identity, "root:") {
		name = path.Base(identity)
	}
	if (name == "" || name == "." || name == "/") && dir != "" {
		name = filepath.Base(dir)
	}
	name = strings.Join(strings.Fields(name), " ")
	if name == "" || name == "." || name == "/" {
		name = "project"
	}
	if utf8.RuneCountInString(name) > logbook.MaxTitle {
		name = string([]rune(name)[:logbook.MaxTitle])
	}
	return name
}

// Sighting is a folder this hab works in, with the identity it has now.
type Sighting struct {
	Dir      string // the repository's top folder; "" when unknown
	Identity string
	Name     string // the name a new project gets; "" is ProjectName's
	Host     string // this hab's host id, the writer of a new project or identity
}

// Sight files a folder under its project and returns the project's canonical id (3.1):
//
//   - an identity a project holds: that project, whatever the folder was filed under before;
//   - a folder filed before under a project whose identity was another (a remote change): the
//     new identity joins that project's set, and its old records stay with it;
//   - else first sight: a new project with the identity and the repository's name.
//
// Two hosts that file one identity while apart make two projects; the fold joins them (5.6).
// The writes are the hab's own, not an agent's or a person's.
func (s *Service) Sight(ctx context.Context, sg Sighting) (string, error) {
	if sg.Identity == "" {
		return "", errors.New("logbook: a sighting needs an identity")
	}
	s.sightMu.Lock()
	defer s.sightMu.Unlock()
	id, ok, err := s.st.ProjectByIdentity(ctx, sg.Identity)
	if err != nil {
		return "", err
	}
	var steps []logbook.Step
	if !ok && sg.Dir != "" {
		f, known, err := s.st.FolderProject(ctx, sg.Dir)
		if err != nil {
			return "", err
		}
		if known && f.Identity != sg.Identity {
			if id, err = s.st.Canonical(ctx, f.Project); err != nil {
				return "", err
			}
			if r, err := s.st.Get(ctx, id); err == nil && r.Type == logbook.TypeProject {
				ok = true
				steps = []logbook.Step{{Kind: logbook.KindAdd, Record: id, Field: "identities", Value: sg.Identity}}
				s.cfg.Logf("logbook: the folder %s has a new identity %s: added to project %s", sg.Dir, sg.Identity, id)
			}
		}
	}
	if !ok {
		name := sg.Name
		if name == "" {
			name = ProjectName(sg.Identity, sg.Dir)
		}
		id = logbook.NewID(logbook.TypeProject)
		steps = []logbook.Step{
			{Kind: logbook.KindCreate, Record: id, Type: logbook.TypeProject, Fields: map[string]any{"title": name}},
			{Kind: logbook.KindAdd, Record: id, Field: "identities", Value: sg.Identity},
		}
	}
	if len(steps) > 0 {
		who := &logbook.Actor{Kind: "hab", Host: sg.Host}
		if _, err := s.st.Write(ctx, logbook.Batch{Actor: who, Steps: steps}); err != nil {
			return "", fmt.Errorf("logbook: file project for %s: %w", sg.Identity, err)
		}
		s.wrote(ctx)
		if id, err = s.st.Canonical(ctx, id); err != nil {
			return "", err
		}
	}
	if sg.Dir != "" {
		if err := s.st.NoteFolder(ctx, sg.Dir, id, sg.Identity); err != nil {
			return "", err
		}
	}
	return id, nil
}

// Known reports whether dir was filed under a project on this replica.
func (s *Service) Known(ctx context.Context, dir string) bool {
	_, ok, err := s.st.FolderProject(ctx, dir)
	return err == nil && ok
}

// Lookup returns the canonical id of the project that holds identity, without filing anything.
func (s *Service) Lookup(ctx context.Context, identity string) (string, bool) {
	id, ok, err := s.st.ProjectByIdentity(ctx, identity)
	return id, ok && err == nil
}

// Hide lists the projects an agent working in project (any of its ids, or "" for none) may not
// read: every private project but its own (12.1).
func (s *Service) Hide(ctx context.Context, project string) ([]string, error) {
	private, err := s.st.PrivateProjects(ctx)
	if err != nil || len(private) == 0 {
		return nil, err
	}
	if project != "" {
		c, err := s.st.Canonical(ctx, project)
		if err != nil {
			return nil, err
		}
		private = slices.DeleteFunc(private, func(p string) bool { return p == c })
	}
	return private, nil
}

// hidden answers ErrNotFound for a record in a hidden project, or a hidden project itself: an
// agent outside a private project reads it as if this replica did not hold it.
func (s *Service) hidden(ctx context.Context, r *logbook.Record, hide []string) error {
	if len(hide) == 0 {
		return nil
	}
	p := r.Project
	if r.Type == logbook.TypeProject {
		c, err := s.st.Canonical(ctx, r.ID)
		if err != nil {
			return err
		}
		p = c
	}
	if p != "" && slices.Contains(hide, p) {
		return logbook.ErrNotFound
	}
	return nil
}

// touchesHidden refuses a change to a record in a hidden project as not found, as a read of it.
func (s *Service) touchesHidden(ctx context.Context, ids []string, hide []string) error {
	if len(hide) == 0 {
		return nil
	}
	for _, id := range ids {
		r, err := s.st.Get(ctx, id)
		if errors.Is(err, logbook.ErrNotFound) {
			continue // the store refuses it with its reason
		}
		if err == nil {
			err = s.hidden(ctx, r, hide)
		}
		if err != nil {
			return apiErr(err)
		}
	}
	return nil
}

// writesHidden refuses a pane's write that touches a hidden project: a change to one of its
// records, or a create or set that files a record under it.
func (s *Service) writesHidden(ctx context.Context, changes []api.LogbookChange, hide []string) error {
	if len(hide) == 0 {
		return nil
	}
	var ids []string
	for _, c := range changes {
		project := ""
		switch c.Op {
		case api.LogbookChangeOpCreate:
			project = c.Project
		case api.LogbookChangeOpSet:
			if c.Field == "project" {
				project = str(c.Value)
			}
		}
		if c.Op != api.LogbookChangeOpCreate && c.ID != "" {
			ids = append(ids, c.ID)
		}
		if project == "" {
			continue
		}
		cp, err := s.st.Canonical(ctx, project)
		if err != nil {
			return err
		}
		if slices.Contains(hide, cp) {
			return apiErr(logbook.ErrNotFound)
		}
	}
	return s.touchesHidden(ctx, ids, hide)
}
