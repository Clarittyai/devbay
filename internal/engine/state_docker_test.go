package engine

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Clarittyai/devbay/internal/proxy"
)

// The four states, and the transitions between them.
func TestStateMachine(t *testing.T) {
	e, m := testEngine(t, "states")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	plan, err := BootPlan(m)
	if err != nil {
		t.Fatal(err)
	}

	// Cold before anything exists.
	if st, err := e.State(ctx); err != nil || st != StateCold {
		t.Fatalf("state before boot = %v (%v), want cold", st, err)
	}

	if err := e.Up(ctx, plan); err != nil {
		t.Fatalf("up: %v", err)
	}
	if st, _ := e.State(ctx); st != StateWarm {
		t.Errorf("state after boot = %v, want warm", st)
	}

	// Freeze, then thaw. The processes are descheduled, not stopped, so no
	// probing is needed on the way back.
	if err := e.Freeze(ctx); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if st, _ := e.State(ctx); st != StateFrozen {
		t.Errorf("state after freeze = %v, want frozen", st)
	}

	start := time.Now()
	if err := e.Thaw(ctx); err != nil {
		t.Fatalf("thaw: %v", err)
	}
	thawTime := time.Since(start)
	if st, _ := e.State(ctx); st != StateWarm {
		t.Errorf("state after thaw = %v, want warm", st)
	}
	t.Logf("frozen -> warm in %s", thawTime.Round(time.Millisecond))
	if thawTime > time.Second {
		t.Errorf("thaw took %s; the whole point of freezing is that resume is instant", thawTime)
	}

	// A service must actually work after a thaw, not merely report running.
	ep, err := e.Resolver().Endpoint("web", PlaneHost)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + ep.Addr() + "/")
	if err != nil {
		t.Errorf("web unreachable after thaw: %v", err)
	} else {
		resp.Body.Close()
	}

	// Cool, then warm again. This time the processes really exited, so Warm
	// re-probes.
	if err := e.Cool(ctx); err != nil {
		t.Fatalf("cool: %v", err)
	}
	if st, _ := e.State(ctx); st != StateCold {
		t.Errorf("state after cool = %v, want cold", st)
	}
	if err := e.Warm(ctx, plan); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if st, _ := e.State(ctx); st != StateWarm {
		t.Errorf("state after warm = %v, want warm", st)
	}
}

// A frozen bay must still be coolable. Docker refuses to stop a paused
// container, and the frozen bay is exactly the one a scheduler under memory
// pressure most wants to reclaim -- so this path cannot be left broken.
func TestFrozenBayCanStillBeCooled(t *testing.T) {
	e, m := testEngine(t, "frozencool")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	plan, err := BootPlan(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Up(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := e.Freeze(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Cool(ctx); err != nil {
		t.Fatalf("cooling a frozen bay: %v", err)
	}
	if st, _ := e.State(ctx); st != StateCold {
		t.Errorf("state = %v, want cold", st)
	}
}

// Transitions are idempotent, because teardown and scheduling both re-issue
// them after crashes and partial failures.
func TestTransitionsAreIdempotent(t *testing.T) {
	e, m := testEngine(t, "idem")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	plan, err := BootPlan(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Up(ctx, plan); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := e.Freeze(ctx); err != nil {
			t.Errorf("freeze %d: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := e.Thaw(ctx); err != nil {
			t.Errorf("thaw %d: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := e.Cool(ctx); err != nil {
			t.Errorf("cool %d: %v", i, err)
		}
	}
}

// The correction that matters most. The original design treated freezing as
// the way to relieve memory pressure; the cgroup freezer stops scheduling, not
// allocation. This measures the difference rather than asserting it, so the
// claim in the docs stays honest as Docker changes underneath.
func TestFreezingDoesNotReclaimMemoryButCoolingDoes(t *testing.T) {
	e, m := testEngine(t, "memory")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	plan, err := BootPlan(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Up(ctx, plan); err != nil {
		t.Fatal(err)
	}

	running, err := e.Memory(ctx)
	if err != nil {
		t.Fatalf("measuring running memory: %v", err)
	}
	if running == 0 {
		t.Fatal("a running bay reported zero memory; the measurement is broken")
	}

	if err := e.Freeze(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // let stats settle
	frozen, err := e.Memory(ctx)
	if err != nil {
		t.Fatalf("measuring frozen memory: %v", err)
	}

	if err := e.Cool(ctx); err != nil {
		t.Fatal(err)
	}
	cold, err := e.Memory(ctx)
	if err != nil {
		t.Fatalf("measuring cold memory: %v", err)
	}

	mb := func(b uint64) float64 { return float64(b) / (1 << 20) }
	t.Logf("running %.1f MiB | frozen %.1f MiB | cold %.1f MiB", mb(running), mb(frozen), mb(cold))

	// Freezing should leave memory essentially untouched. A generous margin,
	// because the point is the order of magnitude, not the exact number.
	if frozen < running/2 {
		t.Errorf("frozen memory (%.1f MiB) is far below running (%.1f MiB); "+
			"if the freezer now reclaims memory, StateFrozen's documentation is out of date",
			mb(frozen), mb(running))
	}
	// Cooling must genuinely release it.
	if cold != 0 {
		t.Errorf("cold bay still reports %.1f MiB; stopping should release it", mb(cold))
	}
	if !StateCold.ReclaimsMemory() || StateFrozen.ReclaimsMemory() {
		t.Error("ReclaimsMemory disagrees with what was just measured")
	}
}

// With the proxy wired in, a bay is reachable by hostname and the focused bay
// additionally answers on the project's canonical name.
func TestRoutesFollowTheBay(t *testing.T) {
	cli := dockerOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	p := proxy.New(cli, func(f string, a ...any) { t.Logf(f, a...) })
	if err := p.Ensure(ctx, 18081, 12020); err != nil {
		t.Fatalf("proxy: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = p.Stop(c)
	})

	e, m := testEngineWith(t, "routed", p)
	plan, err := BootPlan(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Up(ctx, plan); err != nil {
		t.Fatalf("up: %v", err)
	}

	// routed reports whether a hostname reaches a bay. The proxy answers an
	// unrouted host with an explicit 404 carrying X-Devbay: no-such-bay, so
	// "reached a service" and "fell through" are distinguishable rather than
	// both looking like 200.
	routed := func(host string) (bool, int) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:18081/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			return false, 0
		}
		defer resp.Body.Close()
		if resp.Header.Get("X-Devbay") == "no-such-bay" {
			return false, resp.StatusCode
		}
		return resp.StatusCode == 200, resp.StatusCode
	}

	// The primary service claims the bare bay hostname.
	if ok, code := routed("routed.devbaytest.localhost"); !ok {
		t.Errorf("bay hostname did not reach the bay (status %d)", code)
	}
	// An unknown bay gets an explicit answer, not a blank page.
	if ok, code := routed("nosuchbay.devbaytest.localhost"); ok || code != 404 {
		t.Errorf("unknown bay = routed %v, status %d; want an explicit 404", ok, code)
	}

	// The canonical project hostname only answers for the focused bay.
	if ok, _ := routed("devbaytest.localhost"); ok {
		t.Error("an unfocused bay answered on the canonical hostname")
	}
	if err := e.Focus(ctx, true); err != nil {
		t.Fatalf("focus: %v", err)
	}
	if ok, code := routed("devbaytest.localhost"); !ok {
		t.Errorf("focused bay did not claim the canonical hostname (status %d)", code)
	}
	if st, _ := e.State(ctx); st != StateHot {
		t.Errorf("focused bay state = %v, want hot", st)
	}

	// Teardown must withdraw the routes, or a hostname keeps resolving to a
	// container that no longer exists.
	if err := e.Down(ctx); err != nil {
		t.Fatalf("down: %v", err)
	}
	if ok, _ := routed("routed.devbaytest.localhost"); ok {
		t.Error("bay hostname still reaches a service after teardown")
	}
}
