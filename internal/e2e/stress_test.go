package e2e

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Clarittyai/devbay/internal/bay"
	"github.com/Clarittyai/devbay/internal/engine"
)

// The headline case: several agents working at once.
//
// Every other test creates bays one at a time, which means the concurrency
// this tool exists for was the least-exercised path in it. Ports, the network,
// the image pull and the state database are all shared, and each is a place
// where two simultaneous creations can collide.
func TestFiveBaysInParallel(t *testing.T) {
	m, _ := newManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	const n = 5
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("par%d", i)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		for _, name := range names {
			_ = m.Destroy(c, name, true)
		}
	})

	start := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = m.Create(ctx, bay.CreateOptions{Name: name, Alias: name, Boot: true})
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("%s failed to come up: %v", names[i], err)
		}
	}
	t.Logf("%d bays booted concurrently in %s", n, time.Since(start).Round(time.Millisecond))

	// Every bay must have its own ports, and every one must actually serve.
	seen := map[int]string{}
	var total int64
	for _, name := range names {
		b, ok := m.Get(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		for _, key := range engine.PortKeys(b.Manifest) {
			ep, err := b.Engine.Resolver().Endpoint(svcOf(key), engine.PlaneHost)
			if err != nil {
				continue
			}
			if other, dup := seen[ep.Port]; dup {
				t.Errorf("port %d handed to both %s and %s", ep.Port, other, name)
			}
			seen[ep.Port] = name
		}

		ep, err := b.Engine.Resolver().Endpoint("app", engine.PlaneHost)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		resp, err := http.Get("http://" + ep.Addr() + "/health")
		if err != nil {
			t.Errorf("%s is not serving: %v", name, err)
			continue
		}
		resp.Body.Close()

		if mem, err := b.Engine.Memory(ctx); err == nil {
			total += int64(mem)
		}
	}

	mib := float64(total) / (1 << 20)
	t.Logf("%d bays resident: %.0f MiB inside the VM (%.0f MiB each)", n, mib, mib/n)
	// The design target is five bays on a 16 GiB machine. The fixture is
	// small, so this is a ceiling check rather than a benchmark of real apps:
	// it catches a per-bay leak, not a heavy application.
	if mib > 4096 {
		t.Errorf("five bays used %.0f MiB, which is past any plausible budget", mib)
	}

	// And they must come down cleanly, concurrently, too.
	var dwg sync.WaitGroup
	derrs := make([]error, n)
	for i, name := range names {
		dwg.Add(1)
		go func() {
			defer dwg.Done()
			derrs[i] = m.Destroy(ctx, name, true)
		}()
	}
	dwg.Wait()
	for i, err := range derrs {
		if err != nil {
			t.Errorf("%s failed to tear down: %v", names[i], err)
		}
	}
	if list, _ := m.List(ctx); len(list) != 0 {
		t.Errorf("%d bays survived a concurrent teardown", len(list))
	}
}

// Tasks running at the same time in different bays must not interfere. This is
// the normal state of affairs with several agents, and it exercises the
// throwaway-container path concurrently.
func TestConcurrentTasksAcrossBays(t *testing.T) {
	m, _ := newManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	const n = 4
	for i := 0; i < n; i++ {
		newBay(t, m, fmt.Sprintf("task%d", i))
	}

	var wg sync.WaitGroup
	results := make([]*engine.TaskResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Alternate so both a passing and a failing task run concurrently.
			task := "unit"
			if i%2 == 1 {
				task = "failing"
			}
			results[i], errs[i] = m.RunTask(ctx, fmt.Sprintf("task%d", i), task)
		}()
	}
	wg.Wait()

	for i := range results {
		if errs[i] != nil {
			t.Fatalf("task%d: %v", i, errs[i])
		}
		wantPass := i%2 == 0
		if results[i].Succeeded() != wantPass {
			t.Errorf("task%d: succeeded=%v, want %v", i, results[i].Succeeded(), wantPass)
		}
		// A concurrent run must not pick up another bay's report file.
		if !wantPass && len(results[i].Failures) == 0 {
			t.Errorf("task%d failed with no structured failures", i)
		}
		if wantPass && len(results[i].Failures) != 0 {
			t.Errorf("task%d passed but reported failures: %+v", i, results[i].Failures)
		}
	}
}

// Running the same task repeatedly must give the same answer. A report file
// left behind by a previous run, or a container reused in a dirty state, would
// show up here as drift.
func TestRepeatedRunsAreStable(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	newBay(t, m, "repeat")

	var first *engine.TaskResult
	for i := 0; i < 4; i++ {
		res, err := m.RunTask(ctx, "repeat", "failing")
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if first == nil {
			first = res
			continue
		}
		if res.ExitCode != first.ExitCode {
			t.Errorf("run %d exit %d, first run %d", i, res.ExitCode, first.ExitCode)
		}
		if res.Failed != first.Failed || res.Passed != first.Passed {
			t.Errorf("run %d counts %d/%d, first run %d/%d",
				i, res.Passed, res.Failed, first.Passed, first.Failed)
		}
		if len(res.Failures) != len(first.Failures) {
			t.Errorf("run %d reported %d failures, first run %d",
				i, len(res.Failures), len(first.Failures))
		}
	}
}

// A bay booted twice must not be booted twice. Up is called by every task run,
// so this path runs constantly and used to collide on container names.
func TestRepeatedBootIsIdempotent(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	b := newBay(t, m, "idem")

	before, err := b.Engine.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := engine.BootPlan(b.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := b.Engine.Up(ctx, plan); err != nil {
			t.Fatalf("boot %d: %v", i, err)
		}
	}
	after, err := b.Engine.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("container count changed from %d to %d across repeated boots", len(before), len(after))
	}

	// The one-shot must not have run again: re-applying a migration is at
	// best wasted work and at worst destructive.
	var oneshots int
	for _, s := range after {
		if s.Service == "migrate" {
			oneshots++
		}
	}
	if oneshots != 1 {
		t.Errorf("found %d migrate containers, want exactly 1", oneshots)
	}
}

// The north star: how long from asking for a result to holding one.
func TestTimings(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()

	coldStart := time.Now()
	b := newBay(t, m, "timing")
	bootTime := time.Since(coldStart)

	unitStart := time.Now()
	if _, err := m.RunTask(ctx, "timing", "unit"); err != nil {
		t.Fatal(err)
	}
	unitTime := time.Since(unitStart)

	intStart := time.Now()
	if _, err := m.RunTask(ctx, "timing", "integration"); err != nil {
		t.Fatal(err)
	}
	intTime := time.Since(intStart)

	freezeStart := time.Now()
	if err := b.Engine.Freeze(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Engine.Thaw(ctx); err != nil {
		t.Fatal(err)
	}
	thawTime := time.Since(freezeStart)

	t.Logf("cold boot          %s", bootTime.Round(time.Millisecond))
	t.Logf("unit task (warm)   %s", unitTime.Round(time.Millisecond))
	t.Logf("integration (warm) %s", intTime.Round(time.Millisecond))
	t.Logf("freeze+thaw        %s", thawTime.Round(time.Millisecond))

	// These are the design targets. They are generous, because the point is to
	// catch a regression of the kind that turns 200 ms into 20 s, not to
	// benchmark this particular machine.
	if unitTime > 10*time.Second {
		t.Errorf("a unit task took %s; the target is under 10s", unitTime)
	}
	if intTime > 5*time.Second {
		t.Errorf("a warm integration task took %s; the target is under 5s", intTime)
	}
	if bootTime > 60*time.Second {
		t.Errorf("cold boot took %s; the target is under 60s", bootTime)
	}
	if thawTime > 2*time.Second {
		t.Errorf("freeze and thaw took %s; resume is meant to be instant", thawTime)
	}
}

// svcOf strips a named-port suffix from a port key.
func svcOf(key string) string {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i]
		}
	}
	return key
}
