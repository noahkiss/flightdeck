package logbook

import (
	"context"
	"database/sql"
	"errors"
	"slices"
)

// Projects (logbook.md 3, stage 5): reads over the folded project records, and the folders
// table, which remembers which project each folder on this host was filed under. The table is
// this replica's own, like the keys: sync never sends it, and a rebuild keeps it. It lets a
// remote change find its project (3.1): the folder's new identity matches no project, but the
// folder was filed under one.

const foldersSchema = `
CREATE TABLE IF NOT EXISTS folders (dir TEXT PRIMARY KEY, project TEXT NOT NULL, identity TEXT NOT NULL, at INTEGER NOT NULL);
`

// ProjectByIdentity returns the canonical id (5.6) of the project that holds identity, deleted
// or not. ok is false when no project holds it.
func (s *Store) ProjectByIdentity(ctx context.Context, identity string) (id string, ok bool, err error) {
	if identity == "" {
		return "", false, nil
	}
	err = s.db.QueryRowContext(ctx, `SELECT r.id FROM members m JOIN records r ON r.root = m.root
		WHERE m.field = 'identities' AND m.key = ? AND r.type = ? ORDER BY r.created_wall, r.created_counter, r.created_origin LIMIT 1`,
		memberKey("identities", jsonString(identity)), TypeProject).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	id, err = s.canonicalProject(ctx, id)
	return id, err == nil, err
}

// Canonical returns the id a project shows as: itself, or the older project it is joined with
// (5.6). An id that names no project comes back unchanged.
func (s *Store) Canonical(ctx context.Context, id string) (string, error) {
	return s.canonicalProject(ctx, id)
}

// JoinedWith lists every project joined with id, id included, by their own ids, sorted. A
// project joined with no other lists only itself.
func (s *Store) JoinedWith(ctx context.Context, id string) ([]string, error) {
	c, err := s.canonicalProject(ctx, id)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT project FROM aliases WHERE canonical = ? ORDER BY project`, c)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !slices.Contains(out, id) {
		out = append(out, id)
		slices.Sort(out)
	}
	return out, nil
}

// PrivateProjects lists the canonical ids of the private projects (12.1): a project is private
// when it, or any project joined with it, is marked private. Deleted projects count.
func (s *Store) PrivateProjects(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT coalesce(a.canonical, r.id) FROM records r
		JOIN fields f ON f.root = r.root AND f.field = 'private'
			AND (CASE WHEN json_valid(f.value) THEN json_extract(f.value, '$') END) = 1
		LEFT JOIN aliases a ON a.project = r.id WHERE r.type = ?`, TypeProject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out, rows.Err()
}

// Folder is what the folders table holds for one folder: the project it was filed under, by
// the id the filing used, and the identity it had then.
type Folder struct {
	Project  string
	Identity string
}

// FolderProject reads what the folders table holds for dir. ok is false for a folder never
// filed.
func (s *Store) FolderProject(ctx context.Context, dir string) (f Folder, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT project, identity FROM folders WHERE dir = ?`, dir).Scan(&f.Project, &f.Identity)
	if errors.Is(err, sql.ErrNoRows) {
		return Folder{}, false, nil
	}
	return f, err == nil, err
}

// NoteFolder records that dir, with identity, is filed under project.
func (s *Store) NoteFolder(ctx context.Context, dir, project, identity string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO folders(dir, project, identity, at) VALUES(?, ?, ?, ?)
		ON CONFLICT(dir) DO UPDATE SET project = excluded.project, identity = excluded.identity, at = excluded.at`,
		dir, project, identity, s.now().UnixMilli())
	return err
}
