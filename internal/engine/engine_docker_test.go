package engine

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/Clarittyai/devbay/internal/manifest"
	"github.com/Clarittyai/devbay/internal/proxy"
	"github.com/Clarittyai/devbay/internal/testutil"
)

// These tests need a live Docker daemon. They are the only ones that do, and
// they are the ones that matter most: a boot plan that is correct on paper and
// a teardown that is complete on paper prove nothing about either.
//
// Images are deliberately small and already-common so a first run is not a
// multi-gigabyte download.
const testManifest = `
version: 1
project: devbaytest

services:
  cache:
    image: redis:7-alpine
    port: 6379
    health:
      cmd: [redis-cli, ping]
      timeout: 60s

  # A second redis, probed by its startup line instead of a command. This is
  # the probe that exists for processes with no port and nothing to ask.
  logprobe:
    image: redis:7-alpine
    port: 6379
    health:
      log: "Ready to accept connections"
      timeout: 60s

  seed:
    kind: oneshot
    image: busybox:latest
    needs: [cache]
    run: ["true"]

  web:
    image: nginx:alpine
    primary: true
    port: 80
    needs: [cache, seed]
    volumes: [cachedir]
    health:
      http: /
      timeout: 60s

tasks:
  nothing: {run: ["true"], needs: []}
  witheverything: {run: ["true"], needs: [web]}
`

func dockerOrSkip(t *testing.T) *client.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping Docker integration test in short mode")
	}
	cli, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("no Docker client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
		t.Skipf("Docker daemon is not responding: %v", err)
	}
	return cli
}

func testEngine(t *testing.T, bay string) (*Engine, *manifest.Manifest) {
	return testEngineWith(t, bay, nil)
}

// testEngineWith builds an engine, optionally behind a proxy.
func testEngineWith(t *testing.T, bay string, p *proxy.Proxy) (*Engine, *manifest.Manifest) {
	t.Helper()
	cli := dockerOrSkip(t)

	m, err := manifest.Parse([]byte(testManifest))
	if err != nil {
		t.Fatalf("parsing the test manifest: %v", err)
	}
	if r := manifest.Validate(m); !r.OK() {
		t.Fatalf("test manifest is invalid: %v", r.Err())
	}

	// Registered after the directory exists and before the engine can write
	// into it, so ownership is restored ahead of t.TempDir()'s own removal --
	// cleanups run last-registered-first. Without this, a volume mounted at a
	// path inside the bind mount leaves a root-owned mountpoint behind on
	// Linux and the temp directory cannot be deleted.
	worktree := t.TempDir()
	testutil.ReclaimOnCleanup(t, cli, worktree)

	e, err := New(context.Background(), Options{
		Manifest: m,
		Bay:      bay,
		Worktree: worktree,
		Proxy:    p,
		Log:      func(f string, a ...any) { t.Logf(f, a...) },
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	t.Cleanup(func() {
		// Teardown always runs, even when the test failed part-way, so a
		// failing test does not leave containers behind for the next one.
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		_ = e.Down(ctx)
		_ = e.Close()
	})
	return e, m
}

// count returns how many Docker objects carry this bay's labels.
func count(t *testing.T, e *Engine) (containers, volumes, networks int) {
	t.Helper()
	ctx := context.Background()

	cs, err := e.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: e.filter()})
	if err != nil {
		t.Fatalf("listing containers: %v", err)
	}
	vs, err := e.cli.VolumeList(ctx, client.VolumeListOptions{Filters: e.filter()})
	if err != nil {
		t.Fatalf("listing volumes: %v", err)
	}
	ns, err := e.cli.NetworkList(ctx, client.NetworkListOptions{Filters: e.filter()})
	if err != nil {
		t.Fatalf("listing networks: %v", err)
	}
	return len(cs.Items), len(vs.Items), len(ns.Items)
}

// The walking skeleton: a bay boots, every probe form passes, and the primary
// service actually serves traffic on the port devbay reports.
func TestBayBootsAndServes(t *testing.T) {
	e, m := testEngine(t, "boot")

	plan, err := BootPlan(m)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	start := time.Now()
	if err := e.Up(ctx, plan); err != nil {
		t.Fatalf("up: %v", err)
	}
	t.Logf("bay booted in %s", time.Since(start).Round(time.Millisecond))

	// Every long-running service is running; the oneshot has exited.
	status, err := e.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, s := range status {
		states[s.Service] = s.State
	}
	for _, svc := range []string{"cache", "logprobe", "web"} {
		if states[svc] != "running" {
			t.Errorf("%s state = %q, want running", svc, states[svc])
		}
	}
	if states["seed"] == "running" {
		t.Error("a oneshot should have exited, not still be running")
	}

	// The address devbay hands back must actually work. Probing proved the
	// engine could reach it; this proves the reported number is the same one.
	ep, err := e.Resolver().Endpoint("web", PlaneHost)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + ep.Addr() + "/")
	if err != nil {
		t.Fatalf("GET %s: %v", ep.Addr(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("GET / = %s", resp.Status)
	}

	// Published ports are bound to loopback only. Binding 0.0.0.0 would put
	// every bay, and any credential it holds, on the local network.
	ins, err := e.cli.ContainerInspect(ctx, e.containerName("web"), client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for port, bindings := range ins.Container.NetworkSettings.Ports {
		for _, b := range bindings {
			if b.HostIP != loopback {
				t.Errorf("port %s published on %s, want 127.0.0.1 only", port, b.HostIP)
			}
		}
	}
}

// HC6: teardown must fully reverse creation. A leaked volume silently changes
// the behaviour of the next bay, which is why this is a first-class test
// rather than a cleanup routine.
func TestTeardownLeavesNothing(t *testing.T) {
	e, m := testEngine(t, "teardown")

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	plan, err := BootPlan(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Up(ctx, plan); err != nil {
		t.Fatalf("up: %v", err)
	}

	c, v, n := count(t, e)
	if c == 0 || v == 0 || n == 0 {
		t.Fatalf("nothing to tear down: %d containers, %d volumes, %d networks", c, v, n)
	}
	t.Logf("before teardown: %d containers, %d volumes, %d networks", c, v, n)

	if err := e.Down(ctx); err != nil {
		t.Fatalf("down: %v", err)
	}

	c, v, n = count(t, e)
	if c != 0 || v != 0 || n != 0 {
		t.Errorf("teardown left orphans: %d containers, %d volumes, %d networks", c, v, n)
	}

	// Teardown runs after crashes and partial boots, so it has to be safe to
	// repeat.
	if err := e.Down(ctx); err != nil {
		t.Errorf("second teardown should be a no-op, got %v", err)
	}
}

// A task that needs nothing must boot nothing. This is the payoff for making
// `needs` mandatory, and the reason a unit suite can return in seconds.
func TestTaskNeedingNothingBootsNothing(t *testing.T) {
	e, m := testEngine(t, "empty")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	plan, err := TaskPlan(m, "nothing")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Up(ctx, plan); err != nil {
		t.Fatalf("up: %v", err)
	}

	if c, v, n := count(t, e); c != 0 || v != 0 || n != 0 {
		t.Errorf("a task with needs: [] created %d containers, %d volumes, %d networks; want none", c, v, n)
	}
}

// Two bays of the same project must not share anything. If they did, the
// isolation would be decorative and one bay's migration would corrupt the
// other's expectations.
func TestTwoBaysAreIsolated(t *testing.T) {
	a, m := testEngine(t, "alpha")
	b, _ := testEngine(t, "beta")

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	plan, err := TaskPlan(m, "witheverything")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Up(ctx, plan); err != nil {
		t.Fatalf("alpha up: %v", err)
	}
	if err := b.Up(ctx, plan); err != nil {
		t.Fatalf("beta up: %v", err)
	}

	// Distinct published ports, so both are reachable at once.
	epA, err := a.Resolver().Endpoint("web", PlaneHost)
	if err != nil {
		t.Fatal(err)
	}
	epB, err := b.Resolver().Endpoint("web", PlaneHost)
	if err != nil {
		t.Fatal(err)
	}
	if epA.Port == epB.Port {
		t.Fatalf("both bays published web on %d", epA.Port)
	}
	for _, ep := range []Endpoint{epA, epB} {
		resp, err := http.Get("http://" + ep.Addr() + "/")
		if err != nil {
			t.Errorf("GET %s: %v", ep.Addr(), err)
			continue
		}
		resp.Body.Close()
	}

	// Distinct origins, which is what keeps their cookie jars apart.
	if a.Resolver().Hostname("web") == b.Resolver().Hostname("web") {
		t.Error("two bays resolved to the same browser origin")
	}

	// Tearing one down must not disturb the other.
	if err := a.Down(ctx); err != nil {
		t.Fatalf("alpha down: %v", err)
	}
	if c, _, _ := count(t, a); c != 0 {
		t.Errorf("alpha left %d containers", c)
	}
	if c, _, n := count(t, b); c == 0 || n == 0 {
		t.Error("tearing down alpha destroyed beta")
	}
	resp, err := http.Get("http://" + epB.Addr() + "/")
	if err != nil {
		t.Errorf("beta stopped serving after alpha was torn down: %v", err)
	} else {
		resp.Body.Close()
	}
}

// A service that never becomes healthy must fail with the container's own
// output attached. Without the logs, an agent gets "did not become healthy"
// and has nothing to act on.
func TestUnhealthyServiceFailsWithLogs(t *testing.T) {
	dockerOrSkip(t)

	src := `
version: 1
project: devbaytest
services:
  broken:
    image: busybox:latest
    primary: true
    port: 8080
    start: [echo, "this process exits immediately"]
    health:
      http: /
      timeout: 5s
tasks:
  nothing: {run: ["true"], needs: []}
`
	m, err := manifest.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if r := manifest.Validate(m); !r.OK() {
		t.Fatal(r.Err())
	}

	e, err := New(context.Background(), Options{
		Manifest: m, Bay: "broken", Worktree: brokenWorktree(t),
		Log: func(f string, a ...any) { t.Logf(f, a...) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = e.Down(ctx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	plan, err := BootPlan(m)
	if err != nil {
		t.Fatal(err)
	}
	err = e.Up(ctx, plan)
	if err == nil {
		t.Fatal("a service that exits immediately should not report healthy")
	}
	// Detecting the exit rather than waiting out the timeout is the point:
	// the daemon already knows the container is gone.
	if !strings.Contains(err.Error(), "exited") {
		t.Errorf("error should say the container exited, got: %v", err)
	}
	if !strings.Contains(err.Error(), "this process exits immediately") {
		t.Errorf("error should carry the container's own output, got: %v", err)
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	// Leaving stray objects behind would poison a later run, so sweep anything
	// this package's tests created regardless of how they ended.
	if cli, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation()); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		f := make(client.Filters).Add("label", LabelProject+"=devbaytest")
		if cs, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: f}); err == nil {
			for _, c := range cs.Items {
				_, _ = cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
			}
		}
		if vs, err := cli.VolumeList(ctx, client.VolumeListOptions{Filters: f}); err == nil {
			for _, v := range vs.Items {
				_, _ = cli.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{Force: true})
			}
		}
		if ns, err := cli.NetworkList(ctx, client.NetworkListOptions{Filters: f}); err == nil {
			for _, n := range ns.Items {
				_, _ = cli.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{})
			}
		}
		cancel()
		_ = cli.Close()
	}
	fmt.Fprintln(os.Stderr)
	os.Exit(code)
}

// brokenWorktree is the temp directory for a bay that is expected to fail
// boot. It still gets ownership restored: the failure happens after containers
// have started, so they have already had the chance to write into it.
func brokenWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if cli, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation()); err == nil {
		testutil.ReclaimOnCleanup(t, cli, dir)
	}
	return dir
}

// Every container is told which bay it is in, so an application can name its
// own bay in a title, a log line, or a banner. Five identical-looking tabs is
// the problem per-bay origins solve for the browser; this is what lets the
// application help.
func TestContainersKnowWhichBayTheyAreIn(t *testing.T) {
	e, m := testEngine(t, "identity")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	plan, err := BootPlan(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Up(ctx, plan); err != nil {
		t.Fatal(err)
	}

	ins, err := e.cli.ContainerInspect(ctx, e.containerName("web"), client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, kv := range ins.Container.Config.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			got[k] = v
		}
	}
	for k, want := range map[string]string{
		"DEVBAY_BAY":     "identity",
		"DEVBAY_PROJECT": "devbaytest",
		"DEVBAY_SERVICE": "web",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}
