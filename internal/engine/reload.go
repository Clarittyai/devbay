package engine

import (
	"context"
	"fmt"

	"github.com/moby/moby/client"

	"github.com/Clarittyai/devbay/internal/manifest"
)

// RestartService restarts one service and waits for it to be healthy again.
//
// The action behind `watch_action: restart`, which is what a process that
// reads its configuration or its code once at startup needs when a file
// changes. Health is re-probed because a restart is exactly when a change
// breaks a service, and reporting "restarted" for a container that came back
// and immediately died would be worse than saying nothing.
func (e *Engine) RestartService(ctx context.Context, name string) error {
	s, ok := e.m.Services[name]
	if !ok {
		return fmt.Errorf("engine: unknown service %q", name)
	}
	id, err := e.containerID(ctx, name)
	if err != nil {
		return err
	}
	timeout := 10
	if _, err := e.cli.ContainerRestart(ctx, id, client.ContainerRestartOptions{Timeout: &timeout}); err != nil {
		return fmt.Errorf("engine: restarting %s: %w", name, err)
	}
	if err := e.recordPorts(ctx, name, id); err != nil {
		return err
	}
	return e.waitHealthy(ctx, name, id, s)
}

// RebuildService rebuilds a service's image and replaces its container.
//
// The action behind `watch_action: rebuild`, for a service whose code is baked
// into the image rather than read from the mount -- a compiled binary, an
// asset bundle. Restarting one of those reruns the old build.
func (e *Engine) RebuildService(ctx context.Context, name string) error {
	s, ok := e.m.Services[name]
	if !ok {
		return fmt.Errorf("engine: unknown service %q", name)
	}
	if s.Build == nil {
		// Nothing to rebuild, so this is a restart. Saying so beats failing:
		// the developer asked for the freshest possible state and that is what
		// a restart gives them for a pulled image.
		return e.RestartService(ctx, name)
	}

	// The image is content-addressed, so this is a no-op when nothing that
	// goes into it changed -- a watch on a path outside the build context
	// costs a hash walk rather than a build.
	if err := e.ensureImage(ctx, name, s); err != nil {
		return err
	}

	id, err := e.containerID(ctx, name)
	if err == nil {
		if err := e.remove(ctx, id); err != nil {
			return fmt.Errorf("engine: replacing %s: %w", name, err)
		}
	}
	return e.bring(ctx, Step{Service: name, Oneshot: s.IsOneshot()})
}

// Reload applies the action a service declares for a changed file.
func (e *Engine) Reload(ctx context.Context, name string) error {
	s, ok := e.m.Services[name]
	if !ok {
		return fmt.Errorf("engine: unknown service %q", name)
	}
	switch s.WatchAction {
	case manifest.WatchRebuild:
		return e.RebuildService(ctx, name)
	case manifest.WatchSync:
		// The worktree is bind-mounted, so the file is already there; a
		// process that watches its own files has seen it. Nothing to do is the
		// honest answer, and doing something anyway -- restarting a dev server
		// that had already hot-reloaded -- would throw away the state that
		// made hot reload worth having.
		return nil
	default:
		return e.RestartService(ctx, name)
	}
}
