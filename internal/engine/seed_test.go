package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Clarittyai/devbay/internal/manifest"
)

func seedEngine(t *testing.T, worktree string) *Engine {
	t.Helper()
	return &Engine{
		worktree: worktree,
		m: &manifest.Manifest{
			Project: "p",
			Services: map[string]*manifest.Service{
				"db": {
					Image: "postgres:16",
					Fork:  manifest.ForkImage,
					Seed:  &manifest.Seed{After: []string{"migrate"}, Sources: []string{"db/migrations"}},
				},
				"migrate": {Image: "node:22", Run: manifest.Argv{"npm", "run", "migrate"}},
			},
		},
	}
}

func writeMigrations(t *testing.T, dir string, bodies map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "db", "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range bodies {
		if err := os.WriteFile(filepath.Join(dir, "db", "migrations", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The template must be reused across bays, which means the hash cannot depend
// on anything that differs between two checkouts of the same commit.
func TestSeedHashIsStableAcrossCheckouts(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	files := map[string]string{"001_init.sql": "create table t (id int);", "002_more.sql": "alter table t add c int;"}
	writeMigrations(t, a, files)
	writeMigrations(t, b, files)

	ha, err := seedEngine(t, a).seedHash("db", seedEngine(t, a).m.Services["db"])
	if err != nil {
		t.Fatal(err)
	}
	hb, err := seedEngine(t, b).seedHash("db", seedEngine(t, b).m.Services["db"])
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Errorf("two checkouts of the same migrations hashed differently (%s vs %s), so no bay would ever hit the cache", ha, hb)
	}
}

func TestSeedHashChangesWithAMigration(t *testing.T) {
	dir := t.TempDir()
	writeMigrations(t, dir, map[string]string{"001_init.sql": "create table t (id int);"})
	e := seedEngine(t, dir)
	before, err := e.seedHash("db", e.m.Services["db"])
	if err != nil {
		t.Fatal(err)
	}

	writeMigrations(t, dir, map[string]string{"002_new.sql": "create table u (id int);"})
	after, err := e.seedHash("db", e.m.Services["db"])
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Error("adding a migration did not change the hash, so every bay would restore last week's schema")
	}
}

// Renaming a migration changes what runs, even if the bytes are identical.
func TestSeedHashCoversNamesNotJustContents(t *testing.T) {
	dir := t.TempDir()
	writeMigrations(t, dir, map[string]string{"001_a.sql": "create table t (id int);"})
	e := seedEngine(t, dir)
	before, _ := e.seedHash("db", e.m.Services["db"])

	os.Remove(filepath.Join(dir, "db", "migrations", "001_a.sql"))
	writeMigrations(t, dir, map[string]string{"002_a.sql": "create table t (id int);"})
	after, _ := e.seedHash("db", e.m.Services["db"])
	if before == after {
		t.Error("renaming a migration left the hash unchanged")
	}
}

func TestSeedHashChangesWithTheSeedCommand(t *testing.T) {
	dir := t.TempDir()
	writeMigrations(t, dir, map[string]string{"001.sql": "create table t (id int);"})
	e := seedEngine(t, dir)
	before, _ := e.seedHash("db", e.m.Services["db"])
	e.m.Services["migrate"].Run = manifest.Argv{"npm", "run", "migrate:reset"}
	after, _ := e.seedHash("db", e.m.Services["db"])
	if before == after {
		t.Error("changing what seeding runs did not invalidate the template")
	}
}

// Sources that match nothing mean the globs are wrong, and a template keyed on
// nothing would never go stale.
func TestSeedSourcesMustMatchSomething(t *testing.T) {
	dir := t.TempDir()
	e := seedEngine(t, dir)
	if _, err := e.seedHash("db", e.m.Services["db"]); err == nil {
		t.Error("sources matching no files were accepted")
	}
}

func TestSeedSourcesStayInTheWorktree(t *testing.T) {
	e := seedEngine(t, t.TempDir())
	for _, bad := range []string{"/etc", "../outside", "db/../../etc"} {
		if _, err := e.seedSources([]string{bad}); err == nil {
			t.Errorf("%q was accepted as a seed source", bad)
		}
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"/var/lib/postgresql/data": "var-lib-postgresql-data",
		"/data":                    "data",
		"/":                        "root",
	} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}
