//go:build acceptance

// Package acceptance drives the real devbay binary the way a developer does.
//
// It answers "does devbay do the job", which is a different question from "do
// the units pass" -- and one the unit and integration suites cannot answer,
// because they exercise packages rather than the tool. Every check here is
// something a developer could observe for themselves: a hostname that answers,
// a task that finishes, a container that is not running.
//
// The scenarios and their pass criteria are written down in
// docs/ACCEPTANCE.md; the failure messages name the claim rather than the
// assertion, so a red run says what stopped being true.
package acceptance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// env is one repository, one devbay binary, and the bays created from it.
type env struct {
	t    *testing.T
	bin  string // the devbay binary under test
	repo string // a checkout of examples/taskboard
}

func setup(t *testing.T) *env {
	t.Helper()

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "devbay")
	build := exec.Command("go", "build", "-o", bin, "./cmd/devbay")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building devbay: %v\n%s", err, out)
	}

	// A copy of the shipped example, so the suite exercises the same thing the
	// README tells people to try.
	repo := filepath.Join(t.TempDir(), "app")
	if out, err := exec.Command("cp", "-r", filepath.Join(root, "examples", "taskboard"), repo).CombinedOutput(); err != nil {
		t.Fatalf("copying the example: %v\n%s", err, out)
	}
	e := &env{t: t, bin: bin, repo: repo}
	e.git("init", "-q")
	e.git("add", "-A")
	e.git("-c", "user.email=a@t", "-c", "user.name=a", "commit", "-qm", "taskboard")
	return e
}

func (e *env) git(args ...string) string {
	e.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = e.repo
	out, err := cmd.CombinedOutput()
	if err != nil && !strings.Contains(string(out), "nothing to commit") {
		e.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// run invokes devbay and returns its combined output, failing on error.
func (e *env) run(args ...string) string {
	e.t.Helper()
	out, err := e.try(args...)
	if err != nil {
		e.t.Fatalf("devbay %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// try invokes devbay and hands back whatever happened.
func (e *env) try(args ...string) (string, error) {
	e.t.Helper()
	cmd := exec.Command(e.bin, args...)
	cmd.Dir = e.repo
	cmd.Env = append(os.Environ(), "DEVBAY_NO_MODEL=1")
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}

// mcp sends one tool call over the agent interface and returns the payload.
func (e *env) mcp(tool string, args map[string]any) map[string]any {
	e.t.Helper()
	req, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	cmd := exec.Command(e.bin, "mcp")
	cmd.Dir = e.repo
	cmd.Stdin = bytes.NewReader(append(req, '\n'))
	out, err := cmd.Output()
	if err != nil {
		e.t.Fatalf("I: the agent interface failed on %s: %v", tool, err)
	}
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	line, _, _ := strings.Cut(string(out), "\n")
	if err := json.Unmarshal([]byte(line), &envelope); err != nil {
		e.t.Fatalf("I: the agent interface returned something unreadable: %v\n%s", err, line)
	}
	if len(envelope.Result.Content) == 0 {
		e.t.Fatalf("I: %s returned no content", tool)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), &payload); err != nil {
		e.t.Fatalf("I: %s did not return JSON: %v", tool, err)
	}
	return payload
}

// get fetches a bay hostname through the proxy, the way a browser would.
func (e *env) get(host, path string) (int, string) {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1"+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Host = host
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	return resp.StatusCode, string(body[:n])
}

func (e *env) post(host, path string) {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1"+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Host = host
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// docker counts resources devbay says it owns.
func docker(t *testing.T, kind string) int {
	t.Helper()
	var args []string
	switch kind {
	case "containers":
		args = []string{"ps", "-aq", "--filter", "label=dev.devbay.managed"}
	case "volumes":
		args = []string{"volume", "ls", "-q", "--filter", "label=dev.devbay.managed"}
	case "networks":
		args = []string{"network", "ls", "-q", "--filter", "label=dev.devbay.managed"}
	case "images":
		args = []string{"images", "-q", "--filter", "label=dev.devbay.managed=1"}
	}
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		t.Fatalf("listing %s: %v", kind, err)
	}
	return len(strings.Fields(string(out)))
}

// runningServices names the services of a bay that are up.
func runningServices(t *testing.T, bay string) map[string]bool {
	t.Helper()
	out, err := exec.Command("docker", "ps",
		"--filter", "label=dev.devbay.bay="+bay,
		"--format", "{{.Label \"dev.devbay.service\"}}").Output()
	if err != nil {
		t.Fatalf("listing containers: %v", err)
	}
	up := map[string]bool{}
	for _, name := range strings.Fields(string(out)) {
		up[name] = true
	}
	return up
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// running reports how many of this bay's containers are up.
func running(t *testing.T, bay string) int {
	t.Helper()
	out, err := exec.Command("docker", "ps", "-q",
		"--filter", "label=dev.devbay.bay="+bay).Output()
	if err != nil {
		t.Fatalf("listing containers: %v", err)
	}
	return len(strings.Fields(string(out)))
}

// TestDevbayDoesTheJob is the whole suite. One test, because the scenarios
// share a repository and build on each other exactly as a working day does --
// splitting them would mean booting the same stack a dozen times to prove
// things that only matter in sequence.
func TestDevbayDoesTheJob(t *testing.T) {
	e := setup(t)
	t.Cleanup(func() {
		for _, bay := range []string{"one", "two"} {
			_, _ = e.try("rm", bay, "--force")
		}
	})

	// ---------------------------------------------------------------- A
	if out := e.run("doctor"); strings.Contains(out, "blocking problem") &&
		!strings.Contains(out, "no blocking problems") {
		t.Fatalf("A: doctor reports a blocking problem on this machine\n%s", out)
	}

	// ---------------------------------------------------------------- B
	e.run("init")
	manifest := e.readManifest()
	for _, bad := range []string{"localhost:", "127.0.0.1:", "@db:", "@cache:", "//api:"} {
		if strings.Contains(manifest, bad) {
			t.Errorf("B: the generated manifest still contains the fixed address %q; "+
				"a second bay would point at the first", bad)
		}
	}
	e.addTasks()
	if out, err := e.try("validate", "."); err != nil {
		t.Fatalf("B: the generated manifest does not validate\n%s", out)
	}
	e.git("add", "-A")
	e.git("-c", "user.email=a@t", "-c", "user.name=a", "commit", "-qm", "devbay.yaml")

	// ---------------------------------------------------------------- C
	e.runWithSecret("new", "one")
	if code, _ := e.get("one.app.localhost", "/"); code != 200 {
		t.Fatalf("C: the primary service does not answer on its own hostname (got %d)", code)
	}

	// ---------------------------------------------------------------- D
	e.runWithSecret("new", "two")
	for _, bay := range []string{"one", "two"} {
		if code, _ := e.get(bay+".app.localhost", "/"); code != 200 {
			t.Errorf("D: %s stopped serving once a second bay existed (got %d)", bay, code)
		}
	}
	e.post("api.one.app.localhost", "/tasks?title=only-in-one")
	time.Sleep(time.Second)
	if _, body := e.get("api.two.app.localhost", "/tasks"); strings.Contains(body, "only-in-one") {
		t.Errorf("D: data written in bay one is visible in bay two: %s", body)
	}
	if _, body := e.get("api.one.app.localhost", "/healthz"); !strings.Contains(body, `"bay":"one"`) {
		t.Errorf("D: a container cannot name its own bay: %s", body)
	}

	// ---------------------------------------------------------------- E
	before := running(t, "two")
	start := time.Now()
	out := e.runWithSecret("run", "two", "unit")
	took := time.Since(start)
	if after := running(t, "two"); after > before {
		t.Errorf("E: a `needs: []` task started %d container(s); it must boot nothing", after-before)
	}
	if took > 5*time.Second {
		t.Errorf("E: a `needs: []` task took %s; the fast path is the point of it", took.Round(time.Millisecond))
	}
	if !strings.Contains(out, "pass") {
		t.Errorf("E: the unit task did not pass\n%s", out)
	}

	// ---------------------------------------------------------------- F
	e.breakATest("two")
	failure := e.mcp("bay_run_task", map[string]any{"bay": "two", "task": "unit"})
	fails, _ := failure["failures"].([]any)
	if len(fails) == 0 {
		t.Errorf("F: a failing test produced no structured failure: %v", failure)
	} else {
		f := fails[0].(map[string]any)
		for _, key := range []string{"name", "file", "line"} {
			if v, ok := f[key]; !ok || v == "" || v == float64(0) {
				t.Errorf("F: the failure carries no %s, so nobody can navigate to it: %v", key, f)
			}
		}
		if msg, _ := f["message"].(string); !strings.Contains(msg, "strictly equal") {
			t.Errorf("F: the assertion text was lost: %q", msg)
		}
	}
	e.fixTheTest("two")

	// ---------------------------------------------------------------- G
	e.runWithSecret("cool", "two")
	e.runWithSecret("run", "two", "integration")
	up := runningServices(t, "two")
	// api declares needs: [cache, db], so those come with it. web asked for
	// nothing and nothing asked for web, so it must stay down -- that is the
	// whole claim: a task materialises its subgraph, not the stack.
	for _, want := range []string{"api", "cache", "db"} {
		if !up[want] {
			t.Errorf("G: `needs: [api]` did not start %q, which api depends on; running: %v", want, keys(up))
		}
	}
	if up["web"] {
		t.Errorf("G: `needs: [api]` started web, which nothing asked for; running: %v", keys(up))
	}

	// ---------------------------------------------------------------- H
	e.editAndWatch("one")

	// ---------------------------------------------------------------- I
	status := e.mcp("bay_status", map[string]any{"bay": "one"})
	if status["state"] != "warm" && status["state"] != "hot" {
		t.Errorf("I: the agent sees state %v while the bay is serving", status["state"])
	}
	urls := e.mcp("bay_url", map[string]any{"bay": "one"})
	if !strings.Contains(fmt.Sprint(urls["public_url"]), "one.app.localhost") {
		t.Errorf("I: the agent was given a browser URL of %v", urls["public_url"])
	}

	// ---------------------------------------------------------------- J
	wtBefore := e.worktree("one")
	e.runWithSecret("cool", "one")
	if n := running(t, "one"); n != 0 {
		t.Errorf("J: cooling left %d container(s) running, so it frees nothing", n)
	}
	e.runWithSecret("thaw", "one")
	if code, _ := e.get("one.app.localhost", "/"); code != 200 {
		t.Errorf("J: the bay did not come back after thawing (got %d)", code)
	}
	// What devbay promises across a resting state is the bay: its worktree,
	// its ports and its hostname. Whether an application's data survives is
	// the application's business -- this example's cache declares no volume,
	// so losing its contents on stop is redis behaving correctly, and
	// asserting otherwise would be testing redis.
	if got := e.worktree("one"); got != wtBefore {
		t.Errorf("J: the worktree moved across a cool/thaw cycle: %s then %s", wtBefore, got)
	}
	if _, err := os.Stat(filepath.Join(wtBefore, "devbay.yaml")); err != nil {
		t.Errorf("J: the bay's checkout did not survive a cool/thaw cycle: %v", err)
	}

	// ---------------------------------------------------------------- K
	_ = exec.Command("docker", "rm", "-f", "devbay-proxy").Run()
	e.runWithSecret("ls") // any command should heal it
	time.Sleep(2 * time.Second)
	if code, _ := e.get("one.app.localhost", "/"); code != 200 {
		t.Errorf("K: the hostname did not come back after the proxy was destroyed (got %d)", code)
	}

	// ---------------------------------------------------------------- L
	e.commitInBay("one")
	e.run("rm", "one", "--force")
	if !strings.Contains(e.git("branch", "--list", "one"), "one") {
		t.Error("L: a branch carrying commits was deleted with the bay")
	}

	// ---------------------------------------------------------------- N
	e.assertNoSecretLeak("two")

	// ---------------------------------------------------------------- M
	e.run("rm", "two", "--force")
	for _, kind := range []string{"containers", "volumes", "networks", "images"} {
		if n := docker(t, kind); n != 0 {
			t.Errorf("M: teardown left %d %s behind", n, kind)
		}
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".devbay", "worktrees", "app")); err == nil {
		t.Error("M: teardown left the project's worktree directory behind")
	}
}

// TestUndeclaredEgressIsBlocked is scenario O, kept separate because it needs
// the enforcement flag and a privileged sidecar per service.
func TestUndeclaredEgressIsBlocked(t *testing.T) {
	e := setup(t)
	e.run("init")
	e.addTasks()
	e.git("add", "-A")
	e.git("-c", "user.email=a@t", "-c", "user.name=a", "commit", "-qm", "devbay.yaml")

	cmd := exec.Command(e.bin, "new", "locked")
	cmd.Dir = e.repo
	cmd.Env = append(os.Environ(), "DEVBAY_EGRESS=1", "DEVBAY_NO_MODEL=1",
		"DEVBAY_SECRET_ACCEPTANCE_CANARY="+canary)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("O: a bay with egress enforcement did not boot\n%s", out)
	}
	t.Cleanup(func() { _, _ = e.try("rm", "locked", "--force") })

	reach := func(addr string) bool {
		out, _ := exec.Command("docker", "exec", "devbay-app-locked-api",
			"node", "-e",
			`const n=require('net');const s=n.createConnection({host:process.argv[1],port:+process.argv[2]});`+
				`s.setTimeout(4000);s.on('connect',()=>{console.log('reached');process.exit(0)});`+
				`s.on('error',()=>process.exit(1));s.on('timeout',()=>process.exit(1));`,
			strings.Split(addr, ":")[0], strings.Split(addr, ":")[1]).CombinedOutput()
		return strings.Contains(string(out), "reached")
	}

	if reach("1.1.1.1:443") {
		t.Error("O: a service declaring no egress reached the internet")
	}
	if !reach("devbay-app-locked-cache:6379") {
		t.Error("O: a service cannot reach its own bay's peers, so the policy is too tight to use")
	}
}
