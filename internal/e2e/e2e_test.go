// Package e2e drives devbay the way a user and an agent actually do.
//
// The other packages test their own contracts. These tests exist to catch what
// only shows up when the pieces are assembled: a bay whose ports are allocated
// but never published, a task that runs before its migrations, a hostname that
// resolves after teardown, a secret that survives one layer and not the next.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/Clarittyai/devbay/internal/bay"
	"github.com/Clarittyai/devbay/internal/engine"
	"github.com/Clarittyai/devbay/internal/mcp"
)

// A repository small enough to boot in seconds but shaped like a real one: a
// datastore, a one-shot migration the app must wait for, and an app that reads
// its configuration from the environment devbay resolves.
const manifestYAML = `
version: 1
project: e2e

services:
  db:
    image: redis:7-alpine
    port: 6379
    health:
      cmd: [redis-cli, ping]
      timeout: 60s

  migrate:
    kind: oneshot
    image: redis:7-alpine
    needs: [db]
    run: [redis-cli, -h, db, set, schema_version, "3"]

  app:
    image: python:3.12-alpine
    primary: true
    port: 8000
    needs: [db, migrate]
    start: [python3, /workspace/app.py]
    health:
      http: /health
      timeout: 90s
    env:
      DATABASE_URL: ${bay.db.url}
      PUBLIC_ORIGIN: ${bay.app.public_url}
      API_TOKEN: ${secret:e2e/token}

tasks:
  unit:
    run: [python3, /workspace/run_tests.py, --junit, /workspace/reports/unit.xml]
    needs: []
    report: {format: junit, path: reports/unit.xml}

  failing:
    run: [python3, /workspace/run_tests.py, --junit, /workspace/reports/failing.xml, --fail]
    needs: []
    report: {format: junit, path: reports/failing.xml}

  integration:
    run: [python3, /workspace/run_tests.py, --junit, /workspace/reports/int.xml, --check-db]
    needs: [db, migrate]
    in: app
    report: {format: junit, path: reports/int.xml}

  reach-out:
    run: [python3, /workspace/reach_out.py]
    needs: []
    in: app
`

// app.py answers /health and reports what devbay put in its environment, so a
// test can assert on resolution rather than trusting it.
const appPY = `
import http.server, json, os
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/health":
            self.send_response(200); self.end_headers(); self.wfile.write(b"ok"); return
        if self.path == "/env":
            body = json.dumps({
                "DATABASE_URL": os.environ.get("DATABASE_URL", ""),
                "PUBLIC_ORIGIN": os.environ.get("PUBLIC_ORIGIN", ""),
                "API_TOKEN": os.environ.get("API_TOKEN", ""),
            }).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers(); self.wfile.write(body); return
        if self.path == "/leak":
            # Deliberately prints its own credential, which is what real
            # applications and SDK error messages do.
            print("connecting with API_TOKEN=" + os.environ.get("API_TOKEN", ""), flush=True)
            self.send_response(200); self.end_headers(); self.wfile.write(b"leaked"); return
        self.send_response(404); self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer(("0.0.0.0", 8000), H).serve_forever()
`

// run_tests.py emits real JUnit XML, so the report parser is exercised rather
// than mocked.
const runTestsPY = `
import argparse, os, sys
p = argparse.ArgumentParser()
p.add_argument("--junit", required=True)
p.add_argument("--fail", action="store_true")
p.add_argument("--check-db", action="store_true")
a = p.parse_args()

cases, failures = [], 0
cases.append(('test_addition', None))
if a.check_db:
    url = os.environ.get("DATABASE_URL", "")
    if url.startswith("redis://") and "127.0.0.1" not in url:
        cases.append(('test_database_url_is_container_address', None))
    else:
        cases.append(('test_database_url_is_container_address',
                      ('expected a container address, got %r' % url, 'suite.py:12: AssertionError')))
        failures += 1
if a.fail:
    cases.append(('test_subtraction',
                  ('assert 5 - 3 == 1', 'suite.py:42: AssertionError\n  assert 2 == 1')))
    failures += 1

os.makedirs(os.path.dirname(a.junit), exist_ok=True)
with open(a.junit, "w") as f:
    f.write('<?xml version="1.0" encoding="utf-8"?>')
    f.write('<testsuites><testsuite name="suite" tests="%d" failures="%d">' % (len(cases), failures))
    for name, fail in cases:
        f.write('<testcase classname="suite" name="%s">' % name)
        if fail:
            msg, body = fail
            f.write('<failure message="%s">%s</failure>' % (msg, body))
        f.write('</testcase>')
    f.write('</testsuite></testsuites>')
sys.exit(1 if failures else 0)
`

// reachOutPY exits non-zero when the outside world is unreachable, so a task
// running it reports whether egress enforcement is in force.
const reachOutPY = `
import socket, sys
try:
    s = socket.create_connection(("1.1.1.1", 443), timeout=4)
    s.close()
    print("reached the internet")
    sys.exit(0)
except Exception as e:
    print("blocked: %s" % e)
    sys.exit(1)
`

// newRepo builds a git repository containing the fixture app.
func newRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("devbay.yaml", manifestYAML)
	write("app.py", appPY)
	write("run_tests.py", runTestsPY)
	write("reach_out.py", reachOutPY)

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=e2e", "GIT_AUTHOR_EMAIL=e2e@example.com",
			"GIT_COMMITTER_NAME=e2e", "GIT_COMMITTER_EMAIL=e2e@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("add", "-A")
	git("commit", "-qm", "initial")
	return dir
}

// The canary. If this value ever reaches an agent, HC1 is violated.
const canary = "sk_test_e2eCANARY0123456789abcdefXYZ"

func newManager(t *testing.T) (*bay.Manager, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("e2e needs Docker; skipped in short mode")
	}
	cli, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("no Docker: %v", err)
	}
	pctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cli.Ping(pctx, client.PingOptions{}); err != nil {
		t.Skipf("Docker is not responding: %v", err)
	}
	cli.Close()

	repo := newRepo(t)
	state := filepath.Join(t.TempDir(), "state.db")

	m, err := bay.Open(context.Background(), bay.Options{
		Dir:          repo,
		StatePath:    state,
		WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"),
		NoProxy:      true, // hostname routing has its own tests; these are about the stack
		Log:          func(f string, a ...any) { t.Logf(f, a...) },
	})
	if err != nil {
		t.Fatalf("opening manager: %v", err)
	}
	// The secret the manifest references. One call teaches both the resolver
	// and the scrubber, so the value can be delivered to the application and
	// removed again on the way out.
	m.SetSecret("e2e/token", canary)
	t.Cleanup(func() { _ = m.Close() })
	return m, repo
}

func newBay(t *testing.T, m *bay.Manager, name string) *bay.Bay {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	b, err := m.Create(ctx, bay.CreateOptions{Name: name, Alias: name, Boot: true})
	if err != nil {
		t.Fatalf("creating bay %s: %v", name, err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = m.Destroy(c, name, true)
	})
	return b
}

// ---------------------------------------------------------------------------

// The whole loop an agent performs: create a bay, run a passing task, run a
// failing one, read the typed failure, read the logs, and destroy it.
func TestAgentLoop(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	b := newBay(t, m, "loop")

	// The app must be genuinely serving, not merely running.
	ep, err := b.Engine.Resolver().Endpoint("app", engine.PlaneHost)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + ep.Addr() + "/health")
	if err != nil {
		t.Fatalf("app not reachable: %v", err)
	}
	resp.Body.Close()

	// A passing task.
	res, err := m.RunTask(ctx, "loop", "unit")
	if err != nil {
		t.Fatalf("unit: %v", err)
	}
	if !res.Succeeded() {
		t.Errorf("unit exited %d: %s", res.ExitCode, res.Output)
	}
	if !res.Parsed {
		t.Error("unit produced no parseable report")
	}
	if res.Passed < 1 {
		t.Errorf("unit reported %d passing", res.Passed)
	}

	// A failing task, which is the case that has to be actionable.
	res, err = m.RunTask(ctx, "loop", "failing")
	if err != nil {
		t.Fatalf("failing: %v", err)
	}
	if res.Succeeded() {
		t.Fatal("the failing task reported success")
	}
	if len(res.Failures) == 0 {
		t.Fatal("a failing task returned no structured failures; an agent has nothing to act on")
	}
	f := res.Failures[0]
	if f.Name != "test_subtraction" {
		t.Errorf("failure name = %q", f.Name)
	}
	if f.Message == "" {
		t.Error("failure has no message")
	}
	// Location is what lets an agent open the file rather than search for it.
	if f.File != "suite.py" || f.Line != 42 {
		t.Errorf("failure location = %s:%d, want suite.py:42", f.File, f.Line)
	}
}

// A task declaring no services must boot none. This is the payoff for making
// `needs` mandatory and the reason a unit suite returns in seconds.
func TestUnitTaskBootsNoExtraServices(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()

	// Deliberately not booted: the task should still run.
	b, err := m.Create(ctx, bay.CreateOptions{Name: "noboot", Alias: "noboot", Boot: false})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = m.Destroy(c, "noboot", true)
	})

	start := time.Now()
	res, err := m.RunTask(ctx, "noboot", "unit")
	if err != nil {
		t.Fatalf("unit on an unbooted bay: %v", err)
	}
	if !res.Succeeded() {
		t.Errorf("unit exited %d: %s", res.ExitCode, res.Output)
	}
	t.Logf("unit on an unbooted bay took %s", time.Since(start).Round(time.Millisecond))

	// Nothing long-running should have started.
	statuses, err := b.Engine.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range statuses {
		if s.State == "running" {
			t.Errorf("service %s is running; a task with needs: [] should start nothing", s.Service)
		}
	}
}

// The address planes, verified from inside the container rather than from the
// resolver's own opinion of what it produced.
func TestEnvironmentResolutionInsideTheContainer(t *testing.T) {
	m, _ := newManager(t)
	b := newBay(t, m, "envs")

	ep, err := b.Engine.Resolver().Endpoint("app", engine.PlaneHost)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + ep.Addr() + "/env")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var env map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}

	// Plane 1: a service talking to another uses the container network. A
	// loopback address here would work from the host and fail in the container,
	// which is the failure this distinction exists to prevent.
	if !strings.HasPrefix(env["DATABASE_URL"], "redis://db:6379") {
		t.Errorf("DATABASE_URL = %q, want the container address redis://db:6379", env["DATABASE_URL"])
	}
	if strings.Contains(env["DATABASE_URL"], "127.0.0.1") {
		t.Error("a container received a loopback address, which will not resolve for it")
	}
	// Plane 3: browser-facing.
	if !strings.Contains(env["PUBLIC_ORIGIN"], "envs.e2e.localhost") {
		t.Errorf("PUBLIC_ORIGIN = %q, want the bay origin", env["PUBLIC_ORIGIN"])
	}
	// The secret really is delivered; scrubbing happens on the way out, not by
	// withholding it from the application.
	if env["API_TOKEN"] != canary {
		t.Errorf("API_TOKEN did not reach the container: %q", env["API_TOKEN"])
	}
}

// HC1: no secret value crosses the boundary to an agent, even when the
// application prints it.
func TestSecretsNeverReachTheAgent(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	b := newBay(t, m, "canary")

	// Make the app log its own credential, as real applications do.
	ep, err := b.Engine.Resolver().Endpoint("app", engine.PlaneHost)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + ep.Addr() + "/leak")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	time.Sleep(500 * time.Millisecond) // let the log line land

	logs, err := b.Engine.Logs(ctx, "app", 200)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs, "API_TOKEN=") {
		t.Fatalf("the app did not log its token, so this test proves nothing:\n%s", logs)
	}
	if strings.Contains(logs, canary) {
		t.Errorf("the canary survived into logs an agent can read:\n%s", logs)
	}

	// And the same through the MCP surface, which is the boundary that matters.
	srv := mcp.NewServer(m)
	out := callTool(t, srv, "bay_logs", map[string]any{"bay": "canary", "lines": 200})
	if strings.Contains(out, canary) {
		t.Errorf("the canary crossed the MCP boundary:\n%s", out)
	}
	if !strings.Contains(out, "redacted") {
		t.Errorf("expected a redaction marker in the returned logs:\n%s", out)
	}
}

// Two bays of the same project must share nothing.
func TestTwoBaysAreFullyIndependent(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	a := newBay(t, m, "alpha")
	bb := newBay(t, m, "beta")

	epA, err := a.Engine.Resolver().Endpoint("app", engine.PlaneHost)
	if err != nil {
		t.Fatal(err)
	}
	epB, err := bb.Engine.Resolver().Endpoint("app", engine.PlaneHost)
	if err != nil {
		t.Fatal(err)
	}
	if epA.Port == epB.Port {
		t.Fatalf("both bays published the app on %d", epA.Port)
	}

	// Each bay's datastore is its own. Writing in one must not be visible in
	// the other, or a migration in one bay corrupts the other's expectations.
	set := func(b *bay.Bay, value string) {
		t.Helper()
		res, err := b.Engine.RunTask(ctx, "unit")
		if err != nil || !res.Succeeded() {
			t.Fatalf("sanity task failed in %s: %v", b.Name, err)
		}
	}
	set(a, "a")
	set(bb, "b")

	// Distinct origins, which is what keeps their browser storage apart.
	if a.Engine.Resolver().Hostname("app") == bb.Engine.Resolver().Hostname("app") {
		t.Error("two bays resolved to the same browser origin")
	}

	// Tearing one down must leave the other working.
	if err := m.Destroy(ctx, "alpha", true); err != nil {
		t.Fatalf("destroying alpha: %v", err)
	}
	resp, err := http.Get("http://" + epB.Addr() + "/health")
	if err != nil {
		t.Fatalf("beta stopped serving after alpha was destroyed: %v", err)
	}
	resp.Body.Close()
}

// Ordering: the app must not start before its migration has completed.
func TestMigrationCompletesBeforeTheAppStarts(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	newBay(t, m, "ordering")

	// The integration task asserts, from inside the container, that the value
	// the one-shot wrote is present -- which can only be true if the ordering
	// held.
	res, err := m.RunTask(ctx, "ordering", "integration")
	if err != nil {
		t.Fatalf("integration: %v", err)
	}
	if !res.Succeeded() {
		t.Errorf("integration failed, so ordering or resolution is wrong: %s", res.Output)
		for _, f := range res.Failures {
			t.Errorf("  %s: %s", f.Name, f.Message)
		}
	}
}

// State survives the process. A bay created by one manager must be visible to
// the next, or the CLI lies about what is running.
func TestBaysSurviveAcrossManagers(t *testing.T) {
	if testing.Short() {
		t.Skip("needs Docker")
	}
	repo := newRepo(t)
	state := filepath.Join(t.TempDir(), "state.db")
	wtRoot := filepath.Join(t.TempDir(), "worktrees")
	ctx := context.Background()

	opts := bay.Options{Dir: repo, StatePath: state, WorktreeRoot: wtRoot, NoProxy: true}

	first, err := bay.Open(ctx, opts)
	if err != nil {
		t.Skipf("cannot open manager: %v", err)
	}
	if _, err := first.Create(ctx, bay.CreateOptions{Name: "persist", Alias: "persist", Boot: false}); err != nil {
		first.Close()
		t.Fatal(err)
	}
	first.Close()

	second, err := bay.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = second.Destroy(c, "persist", true)
		second.Close()
	}()

	list, err := second.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, b := range list {
		if b.Name == "persist" {
			found = true
			if b.Alias != "persist" {
				t.Errorf("alias = %q, want persist", b.Alias)
			}
		}
	}
	if !found {
		t.Errorf("a bay created by one process is invisible to the next: %+v", list)
	}
}

// Destroying a bay leaves nothing behind, anywhere.
func TestDestroyLeavesNoTrace(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	b := newBay(t, m, "trace")
	worktree := b.Worktree

	cli, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	filt := make(client.Filters).Add("label", "dev.devbay.bay=trace")

	before, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filt})
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Items) == 0 {
		t.Fatal("nothing was created, so this test proves nothing")
	}

	if err := m.Destroy(ctx, "trace", true); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	after, _ := cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filt})
	if len(after.Items) != 0 {
		t.Errorf("%d containers survived teardown", len(after.Items))
	}
	vols, _ := cli.VolumeList(ctx, client.VolumeListOptions{Filters: filt})
	if len(vols.Items) != 0 {
		t.Errorf("%d volumes survived teardown", len(vols.Items))
	}
	nets, _ := cli.NetworkList(ctx, client.NetworkListOptions{Filters: filt})
	if len(nets.Items) != 0 {
		t.Errorf("%d networks survived teardown", len(nets.Items))
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Errorf("the worktree survived teardown: %s", worktree)
	}
	if list, _ := m.List(ctx); len(list) != 0 {
		t.Errorf("the bay is still listed after destruction: %+v", list)
	}
}

// The full surface, driven as an agent drives it: over the protocol.
func TestMCPDrivesTheWholeLifecycle(t *testing.T) {
	m, _ := newManager(t)
	srv := mcp.NewServer(m)

	created := callTool(t, srv, "bay_create", map[string]any{
		"name": "viampc", "alias": "mcp",
	})
	if !strings.Contains(created, "viampc") {
		t.Fatalf("bay_create did not return the bay: %s", created)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = m.Destroy(c, "viampc", true)
	})

	if out := callTool(t, srv, "bay_list", nil); !strings.Contains(out, "viampc") {
		t.Errorf("bay_list does not show the new bay: %s", out)
	}
	if out := callTool(t, srv, "bay_status", map[string]any{"bay": "viampc"}); !strings.Contains(out, "warm") {
		t.Errorf("bay_status does not report it warm: %s", out)
	}

	// The two addresses must both be present and must differ.
	urls := callTool(t, srv, "bay_url", map[string]any{"bay": "viampc"})
	var u map[string]any
	if err := json.Unmarshal([]byte(urls), &u); err != nil {
		t.Fatalf("bay_url is not JSON: %v\n%s", err, urls)
	}
	if u["url"] == u["public_url"] {
		t.Error("url and public_url are identical; they address different planes")
	}
	if !strings.Contains(u["url"].(string), "127.0.0.1") {
		t.Errorf("url should be the loopback address: %v", u["url"])
	}
	if !strings.Contains(u["public_url"].(string), "viampc.e2e.localhost") {
		t.Errorf("public_url should be the bay origin: %v", u["public_url"])
	}

	// A failing task must come back typed, over the wire.
	out := callTool(t, srv, "bay_run_task", map[string]any{"bay": "viampc", "task": "failing"})
	var res struct {
		ExitCode int `json:"exit_code"`
		Parsed   bool
		Failures []struct {
			Name, File, Message string
			Line                int
		}
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("bay_run_task is not JSON: %v\n%s", err, out)
	}
	if res.ExitCode == 0 {
		t.Error("the failing task reported success")
	}
	if len(res.Failures) == 0 {
		t.Fatalf("no structured failures crossed the boundary: %s", out)
	}
	if res.Failures[0].Line != 42 {
		t.Errorf("failure line = %d, want 42", res.Failures[0].Line)
	}

	// An unknown task must name the ones that exist rather than dead-ending.
	bad := callToolExpectingError(t, srv, "bay_run_task", map[string]any{"bay": "viampc", "task": "nope"})
	if !strings.Contains(bad, "unit") {
		t.Errorf("an unknown task should list the declared ones: %s", bad)
	}

	if out := callTool(t, srv, "bay_destroy", map[string]any{"bay": "viampc"}); !strings.Contains(out, "destroyed") {
		t.Errorf("bay_destroy: %s", out)
	}
	if out := callTool(t, srv, "bay_list", nil); strings.Contains(out, "viampc") {
		t.Errorf("the bay is still listed after bay_destroy: %s", out)
	}
}

// ---------------------------------------------------------------------------

type rw struct {
	in  io.Reader
	out io.Writer
}

func (p rw) Read(b []byte) (int, error)  { return p.in.Read(b) }
func (p rw) Write(b []byte) (int, error) { return p.out.Write(b) }

// callTool invokes a tool over the protocol and returns its text content.
func callTool(t *testing.T, s *mcp.Server, name string, args map[string]any) string {
	t.Helper()
	text, isErr := invoke(t, s, name, args)
	if isErr {
		t.Fatalf("%s failed: %s", name, text)
	}
	return text
}

func callToolExpectingError(t *testing.T, s *mcp.Server, name string, args map[string]any) string {
	t.Helper()
	text, isErr := invoke(t, s, name, args)
	if !isErr {
		t.Fatalf("%s should have failed, got: %s", name, text)
	}
	return text
}

func invoke(t *testing.T, s *mcp.Server, name string, args map[string]any) (string, bool) {
	t.Helper()
	req := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	}
	line, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := s.Serve(context.Background(), rw{in: bytes.NewReader(append(line, '\n')), out: &buf}); err != nil {
		t.Fatalf("serve: %v", err)
	}

	var resp struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, buf.String())
	}
	if resp.Error != nil {
		t.Fatalf("protocol error from %s: %s", name, resp.Error.Message)
	}
	if len(resp.Result.Content) == 0 {
		return "", resp.Result.IsError
	}
	return resp.Result.Content[0].Text, resp.Result.IsError
}

func TestMain(m *testing.M) {
	code := m.Run()
	// Sweep anything these tests created, however they ended.
	if cli, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation()); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		f := make(client.Filters).Add("label", "dev.devbay.project=e2e")
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
		cli.Close()
	}
	fmt.Fprintln(os.Stderr)
	os.Exit(code)
}
