// Package approve records the commands a human has agreed to run.
//
// R2 permits an argv[0] outside the default allowlist -- `bin/dev`,
// `./scripts/setup.sh` -- because refusing them outright means people fork the
// project, and because the script is committed, reviewable repository content
// rather than a model-derived shell string. What makes that safe is the second
// half of the rule: a human sees the exact argv once and agrees to it.
//
// Until this package existed, devbay printed the approval and ran the command
// anyway. That is the worst of both designs -- the developer is trained to
// scroll past a warning, and the rule provides no boundary at all. So the
// approval now blocks, and because blocking on every single run would be
// intolerable, it is remembered.
package approve

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the record of what has been approved.
type Store struct {
	db *sql.DB
}

// Record is one approved command.
type Record struct {
	// Key identifies the approval; see Key.
	Key string
	// Project scopes it. Approving `bin/dev` for one repository must not
	// approve a file of the same name in another, because the thing being
	// approved is that repository's script, not the string.
	Project string
	// At is where in the manifest it appeared, kept for display only -- moving
	// a command from install: to start: does not change what it does.
	At   string
	Argv []string
	// When and By record who took the decision, so an audit answers "who let
	// this run" rather than only "it was allowed".
	When time.Time
	By   string
}

// Key is the identity of an approval: the project and the exact argv.
//
// Hashing the whole array rather than argv[0] is the point of the rule. A
// developer who approves [bin/dev] has approved that script, not every future
// invocation of it with arguments they never saw -- so [bin/dev, --seed-prod]
// is a different command and asks again.
func Key(project string, argv []string) string {
	h := sha256.New()
	h.Write([]byte(project))
	h.Write([]byte{0})
	// JSON rather than a join: a separator can be smuggled inside an argument,
	// and two different argv arrays that hash the same would let an approval
	// be transplanted onto a command nobody read.
	b, _ := json.Marshal(argv)
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// Open prepares the store. An empty path uses ~/.devbay/state.db, the same
// file the rest of devbay's state lives in.
func Open(path string) (*Store, error) {
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
		CREATE TABLE IF NOT EXISTS approvals (
			key      TEXT PRIMARY KEY,
			project  TEXT NOT NULL,
			at       TEXT NOT NULL,
			argv     TEXT NOT NULL,
			approved INTEGER NOT NULL,
			by       TEXT NOT NULL
		);
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("approve: preparing store: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Granted reports whether this exact command has been approved.
func (s *Store) Granted(ctx context.Context, project string, argv []string) bool {
	if s == nil || len(argv) == 0 {
		return false
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM approvals WHERE key = ?`, Key(project, argv)).Scan(&n)
	return err == nil && n > 0
}

// Grant records an approval.
func (s *Store) Grant(ctx context.Context, r Record) error {
	blob, err := json.Marshal(r.Argv)
	if err != nil {
		return err
	}
	if r.When.IsZero() {
		r.When = time.Now()
	}
	if r.By == "" {
		r.By = currentUser()
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO approvals (key, project, at, argv, approved, by)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			at = excluded.at, approved = excluded.approved, by = excluded.by`,
		Key(r.Project, r.Argv), r.Project, r.At, string(blob), r.When.Unix(), r.By)
	return err
}

// Revoke removes an approval by key. Withdrawing is as necessary as granting:
// an approval that cannot be taken back is a decision a developer will avoid
// making.
func (s *Store) Revoke(ctx context.Context, key string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM approvals WHERE key = ?`, key)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// List returns everything approved for a project, oldest first.
func (s *Store) List(ctx context.Context, project string) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT key, project, at, argv, approved, by
		FROM approvals WHERE project = ? ORDER BY approved`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var r Record
		var blob string
		var when int64
		if err := rows.Scan(&r.Key, &r.Project, &r.At, &blob, &when, &r.By); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(blob), &r.Argv); err != nil {
			return nil, err
		}
		r.When = time.Unix(when, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

func currentUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return fmt.Sprintf("uid:%d", os.Getuid())
}
