package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Clarittyai/devbay/internal/watcher"
)

// cmdWatch reacts to edits until interrupted.
//
// A foreground command rather than a daemon, which is the same shape as
// `docker compose watch` and for the same reason: the process that watches is
// the process the developer can see, stop, and read the output of. devbay has
// no daemon, and adding one so that a file watcher could run in the background
// would be a large amount of machinery to avoid one terminal tab.
func cmdWatch(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: devbay watch <bay>")
	}
	m, err := open(ctx, false)
	if err != nil {
		return err
	}
	defer m.Close()

	b, ok := m.Get(args[0])
	if !ok {
		return fmt.Errorf("no bay named %q", args[0])
	}

	w, err := watcher.New(b.Worktree, b.Manifest, b.Engine,
		func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) })
	if err != nil {
		return err
	}
	if len(w.Services()) == 0 {
		return fmt.Errorf("no service in %s declares a `watch:` list, so there is nothing to watch.\n"+
			"      Add one to the service whose code you edit, for example:\n"+
			"        watch: [\"src/**\", \"package.json\"]", b.Manifest.Path)
	}

	fmt.Printf("%s %s — watching %s\n", green("watch"), bold(b.Name),
		strings.Join(w.Services(), ", "))
	fmt.Println(dim("  edits on this machine are applied inside the containers; ^C to stop"))

	// The context is already wired to SIGINT in main, so ^C ends the watch
	// cleanly rather than leaving a half-applied reload behind.
	return w.Run(ctx)
}
