package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// State is how much of a bay is resident.
//
// The names come from the original design, but two of the cost claims attached
// to them were wrong and have been corrected here, because building a
// scheduler on the wrong ones would produce a tool that promises to reclaim
// memory and does not.
type State string

const (
	// StateCold means the containers exist but are stopped, or do not exist at
	// all. Volumes survive, so a cold bay keeps its data.
	//
	// This is the ONLY state that returns memory. Whether the host sees it
	// back is a separate question: on Apple's Virtualization.framework the
	// Linux VM does not return freed guest memory to macOS, so reclamation
	// there needs OrbStack or Docker's own VMM.
	StateCold State = "cold"

	// StateFrozen means the containers are paused with the cgroup freezer.
	//
	// Freezing stops scheduling, not allocation. CPU drops to zero and resume
	// is near-instant with no state lost, which is genuinely useful -- but the
	// memory stays exactly where it was. Anything that needs memory back must
	// go to cold instead.
	StateFrozen State = "frozen"

	// StateWarm means running and reachable at its own hostname.
	StateWarm State = "warm"

	// StateHot means running and additionally holding the project's canonical
	// hostname, so links and configs that hardcode it land here.
	StateHot State = "hot"

	// StateMixed means the containers disagree, usually mid-transition or
	// after a partial failure. Reported rather than papered over.
	StateMixed State = "mixed"
)

// ReclaimsMemory reports whether entering this state frees the memory a bay
// was using. Only one state does, and a scheduler that assumes otherwise will
// pause bays forever while the machine keeps swapping.
func (s State) ReclaimsMemory() bool { return s == StateCold }

// State reports the bay's current state.
func (e *Engine) State(ctx context.Context) (State, error) {
	list, err := e.longRunning(ctx)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return StateCold, nil
	}

	var running, paused, stopped int
	for _, c := range list {
		switch {
		case c.State == "paused":
			paused++
		case c.State == "running":
			running++
		default:
			stopped++
		}
	}
	switch {
	case paused > 0 && running == 0 && stopped == 0:
		return StateFrozen, nil
	case running > 0 && paused == 0 && stopped == 0:
		if e.focused {
			return StateHot, nil
		}
		return StateWarm, nil
	case stopped > 0 && running == 0 && paused == 0:
		return StateCold, nil
	}
	return StateMixed, nil
}

// Freeze pauses every container in the bay.
//
// Use this to stop a bay consuming CPU while keeping resume instant. Do not
// use it to relieve memory pressure: see StateFrozen.
func (e *Engine) Freeze(ctx context.Context) error {
	return e.each(ctx, "freezing", func(id string) error {
		_, err := e.cli.ContainerPause(ctx, id, client.ContainerPauseOptions{})
		if err != nil && isNotModified(err) {
			return nil // already paused
		}
		return err
	})
}

// Thaw resumes a frozen bay. Nothing is lost, so no health probe is needed --
// the processes never stopped, they were only descheduled.
func (e *Engine) Thaw(ctx context.Context) error {
	return e.each(ctx, "thawing", func(id string) error {
		_, err := e.cli.ContainerUnpause(ctx, id, client.ContainerUnpauseOptions{})
		if err != nil && isNotModified(err) {
			return nil
		}
		return err
	})
}

// Resume brings a bay back from whichever resting state it is in.
//
// "Thaw" is what a developer types to get a bay working again, and they should
// not have to know whether it was paused or stopped to type the right thing.
// Unpausing a stopped bay silently did nothing: the command reported the bay's
// state, which was still cold, and `devbay cool` was a one-way door out of a
// working bay -- the opposite of what it is documented to be, since cooling is
// exactly what a machine under memory pressure should do.
func (e *Engine) Resume(ctx context.Context) error {
	st, err := e.State(ctx)
	if err != nil {
		return err
	}
	if st != StateCold {
		return e.Thaw(ctx)
	}
	plan, err := BootPlan(e.m)
	if err != nil {
		return err
	}
	return e.Warm(ctx, plan)
}

// Cool stops the bay's containers, keeping volumes.
//
// This is the state transition a scheduler under memory pressure should make.
// A frozen bay costs the same memory as a running one.
func (e *Engine) Cool(ctx context.Context) error {
	// A paused container cannot be stopped, so thaw first. Without this a
	// frozen bay would be un-coolable, which is precisely the bay a scheduler
	// most wants to reclaim.
	if st, err := e.State(ctx); err == nil && st == StateFrozen {
		if err := e.Thaw(ctx); err != nil {
			return err
		}
	}
	timeout := 10
	if err := e.each(ctx, "stopping", func(id string) error {
		_, err := e.cli.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &timeout})
		if err != nil && isNotModified(err) {
			return nil
		}
		return err
	}); err != nil {
		return err
	}
	if err := e.settle(ctx, StateCold); err != nil {
		return err
	}
	// The hostname stops resolving too. Left in place it pointed at a stopped
	// container and the browser got a bare 502, which reads as the application
	// having crashed rather than as the bay having been put away on purpose.
	// devbay's own 404 says what actually happened and what to run.
	if e.prox != nil {
		if err := e.prox.ClearRoutes(ctx, e.m.Project, e.bay); err != nil {
			e.Log("  could not withdraw routes: %v", err)
		}
	}
	return nil
}

// settle waits until the daemon's own view of the bay matches want.
//
// ContainerStop returns once the process is gone, but the container list is
// not updated atomically with it: on a loaded machine a State() read taken
// immediately afterwards can still report the container as running. A `devbay
// cool` that returns while `devbay ls` still says warm is a tool contradicting
// itself, and a scheduler that acts on the stale answer evicts the wrong bay.
//
// Bounded, and it reports which services are still up rather than hanging: a
// container that genuinely refuses to stop is a real failure and must look
// like one.
func (e *Engine) settle(ctx context.Context, want State) error {
	deadline := time.Now().Add(20 * time.Second)
	for {
		st, err := e.State(ctx)
		if err != nil {
			return err
		}
		if st == want {
			return nil
		}
		if time.Now().After(deadline) {
			list, _ := e.longRunning(ctx)
			var stuck []string
			for _, c := range list {
				if c.State != "exited" && c.State != "created" {
					stuck = append(stuck, fmt.Sprintf("%s (%s)", c.Labels[LabelService], c.State))
				}
			}
			return fmt.Errorf("the bay is %s rather than %s: %s", st, want, strings.Join(stuck, ", "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Warm restarts a cold bay and waits for it to be healthy again.
//
// Unlike Thaw this does need probing: the processes really did exit, and a
// container that starts is not the same thing as a service that works.
func (e *Engine) Warm(ctx context.Context, plan *Plan) error {
	if err := e.each(ctx, "starting", func(id string) error {
		_, err := e.cli.ContainerStart(ctx, id, client.ContainerStartOptions{})
		if err != nil && isNotModified(err) {
			return nil
		}
		return err
	}); err != nil {
		return err
	}

	for _, step := range plan.Steps {
		if step.Oneshot {
			continue // already ran; re-running a migration on warm would be wrong
		}
		s := e.m.Services[step.Service]
		id, err := e.containerID(ctx, step.Service)
		if err != nil {
			return err
		}
		if err := e.recordPorts(ctx, step.Service, id); err != nil {
			return fmt.Errorf("%s: %w", step.Service, err)
		}
		if err := e.waitHealthy(ctx, step.Service, id, s); err != nil {
			return fmt.Errorf("%s: %w", step.Service, err)
		}
	}
	// The hostname comes back with the bay. Cooling withdrew it, and a bay that
	// is running again but unreachable at the address the developer bookmarked
	// is not a bay that came back.
	return e.Republish(ctx)
}

// Focus marks this bay as the holder of the project's canonical hostname.
//
// Named hostnames cover most work, but some things cannot be talked out of a
// fixed address: an OAuth redirect URI the provider will not wildcard, a mobile
// simulator, a native app config. Focus is what serves those.
func (e *Engine) Focus(ctx context.Context, focused bool) error {
	e.focused = focused
	return e.publishRoutes(ctx)
}

// Focused reports whether this bay holds the canonical hostname.
func (e *Engine) Focused() bool { return e.focused }

// Memory reports the resident memory of the bay's containers, in bytes.
//
// Measured inside the VM, because on macOS the host-side figure is the
// virtual machine's own footprint and says almost nothing about any individual
// bay. A scheduler budgeting against the host number would be budgeting
// against noise.
func (e *Engine) Memory(ctx context.Context) (uint64, error) {
	list, err := e.longRunning(ctx)
	if err != nil {
		return 0, err
	}

	var (
		mu    sync.Mutex
		total uint64
		errs  []error
		wg    sync.WaitGroup
	)
	for _, c := range list {
		if c.State != "running" && c.State != "paused" {
			continue
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			res, err := e.cli.ContainerStats(ctx, id, client.ContainerStatsOptions{})
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			defer res.Body.Close()

			var stats container.StatsResponse
			if err := json.NewDecoder(res.Body).Decode(&stats); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			// Page cache is charged to the container but is reclaimable under
			// pressure, so counting it would overstate what freezing or
			// stopping a bay would actually give back.
			usage := stats.MemoryStats.Usage
			if cache, ok := stats.MemoryStats.Stats["inactive_file"]; ok && cache < usage {
				usage -= cache
			}
			mu.Lock()
			total += usage
			mu.Unlock()
		}(c.ID)
	}
	wg.Wait()
	return total, errors.Join(errs...)
}

// longRunning lists the bay's containers, excluding one-shots, which have
// exited by design and would otherwise drag the state to mixed forever.
func (e *Engine) longRunning(ctx context.Context) ([]container.Summary, error) {
	list, err := e.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: e.filter()})
	if err != nil {
		return nil, err
	}
	out := make([]container.Summary, 0, len(list.Items))
	for _, c := range list.Items {
		svc := c.Labels[LabelService]
		if s, ok := e.m.Services[svc]; ok && s.IsOneshot() {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func (e *Engine) each(ctx context.Context, what string, fn func(id string) error) error {
	list, err := e.longRunning(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range list {
		if err := fn(c.ID); err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", what, c.Labels[LabelService], err))
		}
	}
	return errors.Join(errs...)
}

// isNotModified reports the "already in that state" response, which makes
// every transition idempotent rather than an error to special-case at each
// call site.
func isNotModified(err error) bool {
	if err == nil {
		return false
	}
	if cerrdefs.IsNotModified(err) || cerrdefs.IsConflict(err) {
		return true
	}
	// Not every daemon version classifies these, so the wording is a fallback
	// rather than the primary check.
	msg := err.Error()
	for _, s := range []string{"not paused", "already paused", "is not running", "already started"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
