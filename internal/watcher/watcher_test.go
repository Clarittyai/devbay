package watcher

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Clarittyai/devbay/internal/manifest"
)

func testManifest(t *testing.T, body string) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The glob dialect people actually write. A pattern that matches nothing is a
// watch that silently never fires, which is the exact failure this package
// exists to remove -- so the dialect is tested rather than assumed.
func TestGlobsMatchWhatPeopleMean(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		// The most common form, and the one path.Match gets wrong: ** is a
		// single segment there, so src/** would miss anything nested.
		{"src/**", "src/main.go", true},
		{"src/**", "src/api/handlers/user.go", true},
		{"src/**", "cmd/main.go", false},
		{"src/", "src/a/b.ts", true},

		{"src/**/*.go", "src/main.go", true},
		{"src/**/*.go", "src/api/user.go", true},
		{"src/**/*.go", "src/api/user.ts", false},

		{"*.go", "main.go", true},
		{"*.go", "cmd/main.go", true}, // a bare name matches at any depth

		{"package.json", "package.json", true},
		{"package.json", "packages/api/package.json", true},
		{"package.json", "package.json.bak", false},

		{"go.mod", "go.sum", false},
		{"**", "anything/at/all", true},
	} {
		w := &Watcher{m: testManifest(t, `
version: 1
project: p
services:
  web:
    image: node:22
    port: 3000
    health: {http: /}
    watch: ["`+tc.pattern+`"]
tasks:
  unit: {run: [true], needs: []}
`)}
		got := len(w.Match(tc.path)) == 1
		if got != tc.want {
			t.Errorf("pattern %q against %q = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

// A change can concern more than one service in a monorepo, and every one of
// them has to hear about it.
func TestOneChangeCanReachSeveralServices(t *testing.T) {
	w := &Watcher{m: testManifest(t, `
version: 1
project: p
services:
  api:
    image: node:22
    port: 3000
    health: {http: /}
    watch: ["shared/**", "api/**"]
  web:
    image: node:22
    port: 3001
    health: {http: /}
    watch: ["shared/**", "web/**"]
tasks:
  unit: {run: [true], needs: []}
`)}
	if got := w.Match("shared/types.ts"); len(got) != 2 {
		t.Errorf("shared change reached %v, want both services", got)
	}
	if got := w.Match("web/page.tsx"); len(got) != 1 || got[0] != "web" {
		t.Errorf("web change reached %v, want only web", got)
	}
}

// Noise directories are skipped. .git alone produces enough events during a
// commit to restart every service repeatedly for no reason.
func TestNoiseIsIgnored(t *testing.T) {
	for _, p := range []string{
		".git/index", "node_modules/react/index.js", "src/.#main.go",
		"src/main.go~", "src/main.go.swp", "dist/bundle.js", "__pycache__/x.pyc",
	} {
		if !skipPath(p) {
			t.Errorf("%q should be ignored", p)
		}
	}
	for _, p := range []string{"src/main.go", "package.json", "a/b/c.ts"} {
		if skipPath(p) {
			t.Errorf("%q should not be ignored", p)
		}
	}
}

// ---------------------------------------------------------------------------
// against the real filesystem
// ---------------------------------------------------------------------------

type recorder struct {
	mu   sync.Mutex
	seen []string
	fire chan struct{}
}

func (r *recorder) Reload(_ context.Context, service string) error {
	r.mu.Lock()
	r.seen = append(r.seen, service)
	r.mu.Unlock()
	select {
	case r.fire <- struct{}{}:
	default:
	}
	return nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

const watchManifest = `
version: 1
project: p
services:
  web:
    image: node:22
    port: 3000
    health: {http: /}
    watch: ["src/**"]
tasks:
  unit: {run: [true], needs: []}
`

// The claim the package makes: an edit on the host reaches the container,
// without anything polling inside it.
func TestAHostEditTriggersTheService(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{fire: make(chan struct{}, 8)}
	w, err := New(root, testManifest(t, watchManifest), rec, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := w.Run(ctx); err != nil {
			t.Errorf("run: %v", err)
		}
	}()
	time.Sleep(300 * time.Millisecond) // let the tree be registered

	if err := os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rec.fire:
	case <-time.After(10 * time.Second):
		t.Fatal("an edit under a watched path did not reach the service")
	}
}

// A file outside every watch list must not restart anything: a service that
// restarts on unrelated edits is worse than one that never restarts, because
// it throws away working state at random.
func TestAnUnwatchedFileChangesNothing(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"src", "docs"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rec := &recorder{fire: make(chan struct{}, 8)}
	w, err := New(root, testManifest(t, watchManifest), rec, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(root, "docs", "readme.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rec.fire:
		t.Error("an edit outside every watch list restarted a service")
	case <-time.After(1500 * time.Millisecond):
	}
}

// Editors write a file several times per save -- temp file, rename, touch,
// sometimes a formatter afterwards. Acting on each one restarts a service
// repeatedly when only the last write mattered.
func TestABurstOfWritesActsOnce(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{fire: make(chan struct{}, 32)}
	w, err := New(root, testManifest(t, watchManifest), rec, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	for i := 0; i < 12; i++ {
		if err := os.WriteFile(filepath.Join(root, "src", "main.go"),
			[]byte("package main // "+string(rune('a'+i))), 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(15 * time.Millisecond)
	}
	select {
	case <-rec.fire:
	case <-time.After(10 * time.Second):
		t.Fatal("the burst produced no reload at all")
	}
	time.Sleep(settle + 500*time.Millisecond)

	if n := rec.count(); n > 2 {
		t.Errorf("twelve writes produced %d reloads; they should be collapsed", n)
	}
}

// A directory created after the watch started has to be watched too -- a build
// that creates a directory and writes into it would otherwise be invisible.
func TestDirectoriesCreatedLaterAreWatched(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{fire: make(chan struct{}, 8)}
	w, err := New(root, testManifest(t, watchManifest), rec, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	sub := filepath.Join(root, "src", "api")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(sub, "user.go"), []byte("package api"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rec.fire:
	case <-time.After(10 * time.Second):
		t.Fatal("an edit in a directory created after the watch started was missed")
	}
}

// A manifest with no watches is an error rather than a process that sits there
// doing nothing while the developer waits for it to work.
func TestNothingToWatchIsAnError(t *testing.T) {
	w, err := New(t.TempDir(), testManifest(t, `
version: 1
project: p
services:
  web: {image: node:22, port: 3000, health: {http: /}}
tasks:
  unit: {run: [true], needs: []}
`), ReloaderFunc(func(context.Context, string) error { return nil }), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Run(context.Background()); err == nil {
		t.Error("watching a manifest with no watch lists should say so")
	}
}
