// Package watcher turns file changes on the host into container actions.
//
// The watching happens on the host, deliberately and unavoidably. The FUSE and
// virtiofs inotify patches were never merged, so a file edited on the host does
// not reliably produce an inotify event inside a container: Vite, chokidar,
// webpack and nodemon all miss changes made from the editor. The usual
// workaround is to make every watcher poll, which costs real CPU per watcher
// per service per bay -- five bays of a JavaScript monorepo is a fan spinning
// for nothing.
//
// So devbay watches with the platform's own mechanism, on the side of the
// mount where it works, and tells the container what to do. That is what makes
// `watch:` a real field rather than a decorative one.
package watcher

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/Clarittyai/devbay/internal/manifest"
)

// Reloader applies the action a changed file calls for.
type Reloader interface {
	Reload(ctx context.Context, service string) error
}

// ReloaderFunc adapts a function to Reloader.
type ReloaderFunc func(context.Context, string) error

func (f ReloaderFunc) Reload(ctx context.Context, service string) error { return f(ctx, service) }

// settle is how long changes are collected before acting.
//
// Editors do not write a file once. They write a temporary file, rename it,
// touch the directory, and sometimes write again; a formatter on save adds
// more. Acting on the first event restarts a service two or three times per
// keystroke-batch, and the last restart is the only one that mattered.
const settle = 250 * time.Millisecond

// skipDirs never contain anything worth reacting to, and two of them --
// .git and node_modules -- generate enough events on their own to keep a
// watcher permanently busy.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, ".venv": true, "__pycache__": true,
	".next": true, "dist": true, "target": true, ".devbay": true,
}

// Watcher reacts to changes under one worktree.
type Watcher struct {
	root string
	m    *manifest.Manifest
	rel  Reloader

	// Log receives one line per action. Never nil after New.
	Log func(format string, args ...any)

	fsw *fsnotify.Watcher

	mu      sync.Mutex
	pending map[string]bool // services waiting for the settle window
}

// New builds a watcher for a worktree.
func New(root string, m *manifest.Manifest, rel Reloader, log func(string, ...any)) (*Watcher, error) {
	if m == nil || rel == nil {
		return nil, errors.New("watcher: a manifest and a reloader are required")
	}
	if log == nil {
		log = func(string, ...any) {}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return &Watcher{root: abs, m: m, rel: rel, Log: log, pending: map[string]bool{}}, nil
}

// Services lists the services that declare a watch, sorted.
func (w *Watcher) Services() []string {
	var out []string
	for name, s := range w.m.Services {
		if len(s.Watch) > 0 {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Match reports which services care about a path, relative to the worktree.
//
// Exported because it is the part worth testing directly: a glob that matches
// nothing is a watch that silently never fires, which is the failure mode this
// whole package exists to remove.
func (w *Watcher) Match(rel string) []string {
	rel = filepath.ToSlash(strings.TrimPrefix(rel, "/"))
	var out []string
	for _, name := range sortedServices(w.m) {
		for _, pattern := range w.m.Services[name].Watch {
			if matches(filepath.ToSlash(pattern), rel) {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

// matches implements the glob dialect people actually write.
//
// path.Match alone is not enough: it treats ** as a single segment, so
// "src/**" would match src/main.go and not src/api/main.go -- which is never
// what someone means by src/**. A prefix like "src/" and a bare directory name
// are accepted for the same reason.
func matches(pattern, rel string) bool {
	switch {
	case pattern == "":
		return false
	case pattern == "." || pattern == "**" || pattern == "./**":
		return true
	}
	pattern = strings.TrimPrefix(pattern, "./")

	// "src/**" and "src/" mean everything below src.
	if base, ok := strings.CutSuffix(pattern, "/**"); ok {
		return rel == base || strings.HasPrefix(rel, base+"/")
	}
	if base, ok := strings.CutSuffix(pattern, "/"); ok {
		return strings.HasPrefix(rel, base+"/")
	}

	// "src/**/*.go": try each split point, so any number of intervening
	// directories satisfies the **.
	if before, after, ok := strings.Cut(pattern, "/**/"); ok {
		if !strings.HasPrefix(rel, before+"/") {
			return false
		}
		tail := strings.TrimPrefix(rel, before+"/")
		for {
			if ok, _ := filepath.Match(after, tail); ok {
				return true
			}
			_, rest, found := strings.Cut(tail, "/")
			if !found {
				return false
			}
			tail = rest
		}
	}

	if ok, _ := filepath.Match(pattern, rel); ok {
		return true
	}
	// A bare name with no separator matches at any depth: "package.json"
	// should fire for packages/api/package.json, which is what someone
	// watching a monorepo means.
	if !strings.Contains(pattern, "/") {
		if ok, _ := filepath.Match(pattern, filepath.Base(rel)); ok {
			return true
		}
	}
	return false
}

func sortedServices(m *manifest.Manifest) []string {
	out := make([]string, 0, len(m.Services))
	for name := range m.Services {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Run watches until the context is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	if len(w.Services()) == 0 {
		return errors.New("no service declares a `watch:` list, so there is nothing to watch")
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("watcher: %w", err)
	}
	w.fsw = fsw
	defer fsw.Close()

	// fsnotify watches directories, not trees, so every directory is
	// registered and new ones are registered as they appear. A build that
	// creates a directory and writes into it would otherwise be invisible.
	if err := w.addTree(w.root); err != nil {
		return err
	}

	w.Log("watching %s for %s", w.root, strings.Join(w.Services(), ", "))

	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	armed := false

	for {
		select {
		case <-ctx.Done():
			return nil

		case ev, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			if ev.Op&fsnotify.Chmod != 0 && ev.Op&^fsnotify.Chmod == 0 {
				continue // touching permissions is not a change to react to
			}
			if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
				if ev.Op&fsnotify.Create != 0 {
					_ = w.addTree(ev.Name)
				}
				continue
			}
			rel, err := filepath.Rel(w.root, ev.Name)
			if err != nil || strings.HasPrefix(rel, "..") || skipPath(rel) {
				continue
			}
			services := w.Match(rel)
			if len(services) == 0 {
				continue
			}
			w.mu.Lock()
			for _, s := range services {
				w.pending[s] = true
			}
			w.mu.Unlock()

			// Restarted rather than extended, so a long editing session does
			// not defer the reload indefinitely; the window is short enough
			// that this is the behaviour people expect.
			if armed && !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(settle)
			armed = true

		case <-timer.C:
			armed = false
			w.flush(ctx)

		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			w.Log("watch error: %v", err)
		}
	}
}

// flush applies one round of pending reloads.
func (w *Watcher) flush(ctx context.Context) {
	w.mu.Lock()
	names := make([]string, 0, len(w.pending))
	for s := range w.pending {
		names = append(names, s)
	}
	w.pending = map[string]bool{}
	w.mu.Unlock()
	sort.Strings(names)

	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		if err := w.rel.Reload(ctx, name); err != nil {
			// Reported and survived. A change that breaks a service is the
			// normal case while editing, and a watcher that exits on the first
			// broken save is a watcher nobody leaves running.
			w.Log("%s: %v", name, err)
			continue
		}
		if action := w.m.Services[name].WatchAction; action == manifest.WatchSync {
			w.Log("%s: files are already visible through the mount", name)
		} else {
			w.Log("%s: %s in %s", name, action, time.Since(start).Round(time.Millisecond))
		}
	}
}

// addTree registers a directory and everything under it.
func (w *Watcher) addTree(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // a directory that vanished mid-walk is not an error
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && skipDirs[d.Name()] {
			return fs.SkipDir
		}
		if err := w.fsw.Add(path); err != nil {
			// One unwatchable directory must not take the whole watch down;
			// a symlink loop or a permission hole is not worth failing over.
			w.Log("cannot watch %s: %v", path, err)
		}
		return nil
	})
}

// skipPath reports whether a relative path lies inside a skipped directory.
func skipPath(rel string) bool {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if skipDirs[part] {
			return true
		}
	}
	// Editor scratch files: writing one is not a change to the file it shadows.
	base := filepath.Base(rel)
	return strings.HasSuffix(base, "~") || strings.HasPrefix(base, ".#") ||
		strings.HasSuffix(base, ".swp") || strings.HasSuffix(base, ".tmp")
}
