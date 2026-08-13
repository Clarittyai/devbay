package bay

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// store persists bay records.
//
// Without this a bay would exist only in the memory of whichever process
// created it, so `devbay new` followed by `devbay ls` would report nothing --
// while the containers, the port block and the worktree all still existed.
// State that outlives the process is not a daemon feature; it is what makes
// the CLI honest about what is running.
type store struct {
	db *sql.DB
}

type record struct {
	Name      string
	Alias     string
	Branch    string
	Worktree  string
	Project   string
	Adopted   bool
	Focused   bool
	CreatedAt int64
}

func openStore(path string) (*store, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, ".devbay", "state.db")
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS bays (
			name       TEXT PRIMARY KEY,
			alias      TEXT NOT NULL,
			branch     TEXT NOT NULL,
			worktree   TEXT NOT NULL,
			project    TEXT NOT NULL,
			adopted    INTEGER NOT NULL DEFAULT 0,
			focused    INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		);
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("bay: preparing store: %w", err)
	}
	return &store{db: db}, nil
}

func (s *store) Close() error { return s.db.Close() }

func (s *store) Save(ctx context.Context, r record) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO bays (name, alias, branch, worktree, project, adopted, focused, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			alias = excluded.alias, branch = excluded.branch,
			worktree = excluded.worktree, project = excluded.project,
			adopted = excluded.adopted, focused = excluded.focused`,
		r.Name, r.Alias, r.Branch, r.Worktree, r.Project,
		boolToInt(r.Adopted), boolToInt(r.Focused), time.Now().Unix())
	return err
}

func (s *store) SetFocus(ctx context.Context, name string, focused bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE bays SET focused = ? WHERE name = ?`, boolToInt(focused), name)
	return err
}

func (s *store) Delete(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM bays WHERE name = ?`, name)
	return err
}

// List returns records for one project, oldest first.
func (s *store) List(ctx context.Context, project string) ([]record, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, alias, branch, worktree, project, adopted, focused, created_at
		FROM bays WHERE project = ? ORDER BY created_at`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []record
	for rows.Next() {
		var r record
		var adopted, focused int
		if err := rows.Scan(&r.Name, &r.Alias, &r.Branch, &r.Worktree, &r.Project,
			&adopted, &focused, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Adopted, r.Focused = adopted != 0, focused != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// worktrees returns the worktree path of every bay on the machine, by name.
//
// Not filtered by project: the question it answers is whether a directory is
// still in use, and a directory does not belong to a project.
func (s *store) worktrees(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, worktree FROM bays`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var name, worktree string
		if err := rows.Scan(&name, &worktree); err != nil {
			return nil, err
		}
		out[name] = worktree
	}
	return out, rows.Err()
}

func (s *store) Get(ctx context.Context, name string) (record, bool, error) {
	var r record
	var adopted, focused int
	err := s.db.QueryRowContext(ctx, `
		SELECT name, alias, branch, worktree, project, adopted, focused, created_at
		FROM bays WHERE name = ?`, name).
		Scan(&r.Name, &r.Alias, &r.Branch, &r.Worktree, &r.Project, &adopted, &focused, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return record{}, false, nil
	}
	if err != nil {
		return record{}, false, err
	}
	r.Adopted, r.Focused = adopted != 0, focused != 0
	return r, true, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
