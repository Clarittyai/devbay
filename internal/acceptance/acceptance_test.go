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
	// home isolates the suite's state -- bays, ports, approvals, audit log --
	// from the developer's own. Without it the suite both depends on and
	// modifies whatever they have already done on this machine, which makes a
	// pass mean nothing and a failure impossible to reproduce.
	home string
	// dockerConfig points at the developer's real docker CLI configuration,
	// which is not state devbay owns and must not be isolated with the rest.
	dockerConfig string
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
	e := &env{t: t, bin: bin, repo: repo, home: t.TempDir()}
	// The docker CLI keeps its plugins under $HOME/.docker, and buildx is one
	// of them -- so isolating HOME without this takes BuildKit away and every
	// `build:` service fails for a reason that has nothing to do with devbay.
	if real, err := os.UserHomeDir(); err == nil {
		e.dockerConfig = filepath.Join(real, ".docker")
	}
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

// command prepares an invocation of devbay.
//
// Every call goes through here so no test can accidentally run against the
// developer's own state: one that did would create bays the suite's cleanup
// cannot see, and leave them on the machine for good.
func (e *env) command(args ...string) *exec.Cmd {
	cmd := exec.Command(e.bin, args...)
	cmd.Dir = e.repo
	cmd.Env = append(os.Environ(),
		"DEVBAY_NO_MODEL=1",
		"HOME="+e.home,
		"DOCKER_CONFIG="+e.dockerConfig)
	return cmd
}

// try invokes devbay and hands back whatever happened.
func (e *env) try(args ...string) (string, error) {
	e.t.Helper()
	cmd := e.command(args...)
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
	cmd := e.command("mcp")
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

// docker counts what this suite's project left behind.
//
// Scoped to the project rather than to everything devbay manages, because a
// developer running this on their own machine has their own bays up, and a
// suite that fails because of those is a suite they stop running.
func docker(t *testing.T, kind string) int {
	return dockerFor(t, kind, "app")
}

// dockerFor counts one project's resources.
func dockerFor(t *testing.T, kind, name string) int {
	t.Helper()
	project := "label=dev.devbay.project=" + name
	var args []string
	switch kind {
	case "containers":
		args = []string{"ps", "-aq", "--filter", project}
	case "volumes":
		args = []string{"volume", "ls", "-q", "--filter", project}
	case "networks":
		args = []string{"network", "ls", "-q", "--filter", project}
	case "images":
		args = []string{"images", "-q", "--filter", project}
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
	if _, err := os.Stat(filepath.Join(e.home, ".devbay", "worktrees", "app")); err == nil {
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

	cmd := e.command("new", "locked")
	cmd.Env = append(cmd.Env, "DEVBAY_EGRESS=1",
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

// TestEmulatorsRemoveTheNeedForCredentials is scenario P.
//
// The claim behind `externals:` is that a bay can exercise a third-party
// dependency with no credentials at all. It is worth its own scenario because
// it is the one that decides whether a developer has to put a real key on
// their machine to run a feature branch.
func TestEmulatorsRemoveTheNeedForCredentials(t *testing.T) {
	e := setup(t)

	// A repository that sends mail, and nothing else about it changes.
	if err := os.WriteFile(filepath.Join(e.repo, "devbay.yaml"), []byte(`version: 1
project: mailer
externals:
  mail: {emulate: mailpit}
services:
  web:
    image: nginx:alpine
    port: 80
    primary: true
    health: {http: /}
    env:
      SMTP_HOST: ${bay.mail.host}
      SMTP_PORT: ${bay.mail.ports.smtp}
tasks:
  unit: {run: ["true"], needs: []}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	e.git("add", "-A")
	e.git("-c", "user.email=a@t", "-c", "user.name=a", "commit", "-qm", "mailer")

	e.run("new", "mailbay")
	t.Cleanup(func() { _, _ = e.try("rm", "mailbay", "--force") })

	// The application was told where its mail goes, per bay.
	host, err := exec.Command("docker", "exec", "devbay-mailer-mailbay-web", "printenv", "SMTP_HOST").Output()
	if err != nil || strings.TrimSpace(string(host)) == "" {
		t.Fatalf("P: the application was not given the mail catcher's address: %v", err)
	}

	// Sending mail into it works, from inside the bay's own network.
	send := exec.Command("docker", "run", "--rm", "--network", "devbay-mailer-mailbay",
		"alpine:3.20", "sh", "-c",
		`printf "EHLO t\r\nMAIL FROM:<a@b>\r\nRCPT TO:<c@d>\r\nDATA\r\nSubject: acceptance\r\n\r\nbody\r\n.\r\nQUIT\r\n" `+
			`| nc devbay-mailer-mailbay-mail 1025`)
	if out, err := send.CombinedOutput(); err != nil || !strings.Contains(string(out), "250") {
		t.Fatalf("P: could not send mail to the bay's catcher: %v\n%s", err, out)
	}

	// And it is readable at the bay's own hostname, which is what makes it
	// usable rather than merely present.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, body := e.get("mail.mailbay.mailer.localhost", "/api/v1/messages"); strings.Contains(body, "acceptance") {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Error("P: the message never appeared in the bay's own mail catcher")
}

// TestAnUnapprovedCommandDoesNotRun is scenario Q.
//
// R2 lets a repository declare a command outside the allowlist -- bin/dev and
// friends -- on the condition that a human agrees to it once. The condition is
// the whole rule, so it gets a scenario: the question is not whether devbay
// prints a warning, it is whether the command runs.
func TestAnUnapprovedCommandDoesNotRun(t *testing.T) {
	e := setup(t)

	if err := os.MkdirAll(filepath.Join(e.repo, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Marks the filesystem when it runs, so "did it run" is answered by
	// evidence rather than by devbay's own account of itself.
	script := "#!/bin/sh\ntouch /tmp/devbay-acceptance-ran\nexec nginx -g 'daemon off;'\n"
	if err := os.WriteFile(filepath.Join(e.repo, "bin", "dev"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.repo, "devbay.yaml"), []byte(`version: 1
project: gated
services:
  web:
    image: nginx:alpine
    start: ["./bin/dev"]
    port: 80
    primary: true
    health: {http: /}
tasks:
  unit: {run: ["true"], needs: []}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	e.git("add", "-A")
	e.git("-c", "user.email=a@t", "-c", "user.name=a", "commit", "-qm", "gated")

	// The bay must refuse, and say what to do about it.
	out, err := e.try("new", "gated")
	if err == nil {
		_, _ = e.try("rm", "gated", "--force")
		t.Fatal("Q: a bay booted a command nobody had approved")
	}
	if !strings.Contains(out, "./bin/dev") || !strings.Contains(out, "devbay approve") {
		t.Errorf("Q: the refusal did not name the command and the way to allow it:\n%s", out)
	}

	// Nothing was left half-made by the refusal.
	if n := dockerFor(t, "containers", "gated"); n != 0 {
		t.Errorf("Q: a refused bay left %d containers behind", n)
	}

	// An agent cannot approve on the developer's behalf.
	cmd := e.command("approve")
	cmd.Stdin = strings.NewReader("y\ny\ny\n") // an agent answering for the human
	agentOut, _ := cmd.CombinedOutput()
	if !strings.Contains(string(agentOut), "human") {
		t.Errorf("Q: a non-terminal caller was able to approve:\n%s", agentOut)
	}
	if out, err := e.try("new", "gated"); err == nil {
		_, _ = e.try("rm", "gated", "--force")
		t.Fatalf("Q: the bay booted after a caller that was not a human approved it:\n%s", out)
	}

	// And with a human's approval on record, it runs.
	e.run("approve", "--yes")
	e.run("new", "gated")
	t.Cleanup(func() { _, _ = e.try("rm", "gated", "--force") })
	if code, _ := e.get("gated.gated.localhost", "/"); code != 200 {
		t.Errorf("Q: an approved command did not serve; got %d", code)
	}
}

// TestSeedingIsPaidForOnce is scenario R.
//
// The claim is that the second bay of a project does not re-run the migration
// suite. It is the difference between opening a bay to look at a branch and
// deciding not to bother, so it is checked by observation -- the seeding step
// must not run, and the data must be there anyway.
func TestSeedingIsPaidForOnce(t *testing.T) {
	e := setup(t)

	if err := os.MkdirAll(filepath.Join(e.repo, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.repo, "db", "001_init.sql"),
		[]byte("CREATE TABLE seeded (id serial primary key);\n"+
			"INSERT INTO seeded SELECT generate_series(1,1000);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.repo, "devbay.yaml"), []byte(`version: 1
project: seeded
services:
  db:
    image: postgres:16
    port: 5432
    fork: image
    seed: {after: [migrate], sources: [db]}
    # tcp, not pg_isready: the official entrypoint runs initdb against a
    # temporary server listening on the unix socket only, so a socket probe
    # reports ready while every TCP client still gets connection refused.
    health: {tcp: 5432}
    env:
      POSTGRES_PASSWORD: devbay
      POSTGRES_DB: app
  migrate:
    kind: oneshot
    image: postgres:16
    needs: [db]
    run: ["sh", "-c", "psql \"$DB\" -v ON_ERROR_STOP=1 -f /workspace/db/001_init.sql"]
    env:
      DB: postgres://postgres:devbay@db:5432/app
  web:
    image: nginx:alpine
    port: 80
    primary: true
    needs: [migrate]
    health: {http: /}
tasks:
  unit: {run: ["true"], needs: []}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	e.git("add", "-A")
	e.git("-c", "user.email=a@t", "-c", "user.name=a", "commit", "-qm", "seeded")

	// `sh -c` is exactly what R2 gates, so this repository needs a decision
	// before any of it runs -- which is itself worth exercising here.
	e.run("approve", "--yes")

	first := e.run("new", "sfirst")
	t.Cleanup(func() { _, _ = e.try("rm", "sfirst", "--force") })
	if !strings.Contains(first, "captured") {
		t.Fatalf("R: the first bay seeded but captured nothing to reuse:\n%s", first)
	}

	second := e.run("new", "ssecond")
	t.Cleanup(func() { _, _ = e.try("rm", "ssecond", "--force") })
	if !strings.Contains(second, "skipped") {
		t.Errorf("R: the second bay re-ran the seeding steps:\n%s", second)
	}

	// The saving is worthless if the data is not there.
	for _, bay := range []string{"sfirst", "ssecond"} {
		out, err := exec.Command("docker", "exec", "devbay-seeded-"+bay+"-db",
			"psql", "-U", "postgres", "-d", "app", "-tAc", "select count(*) from seeded").Output()
		if err != nil || strings.TrimSpace(string(out)) != "1000" {
			t.Errorf("R: bay %s has %q rows of seeded data, want 1000 (%v)", bay, strings.TrimSpace(string(out)), err)
		}
	}

	// And the bays are still independent: a shared template must not become a
	// shared database.
	if out, err := exec.Command("docker", "exec", "devbay-seeded-ssecond-db",
		"psql", "-U", "postgres", "-d", "app", "-c", "insert into seeded (id) values (99999)").CombinedOutput(); err != nil {
		t.Fatalf("writing to the second bay: %v\n%s", err, out)
	}
	out, _ := exec.Command("docker", "exec", "devbay-seeded-sfirst-db",
		"psql", "-U", "postgres", "-d", "app", "-tAc", "select count(*) from seeded").Output()
	if strings.TrimSpace(string(out)) != "1000" {
		t.Errorf("R: a write in one bay changed another bay's data (%q)", strings.TrimSpace(string(out)))
	}
}

// TestBaysStayWithinABudget is scenario S.
//
// Five bays is the number the design was sized against, and nothing enforces
// it without a daemon -- so devbay enforces it at the only moment it is
// running, which is when a new bay is created. The claim is that the budget
// holds and that the bay the developer is looking at is never the one stopped.
func TestBaysStayWithinABudget(t *testing.T) {
	e := setup(t)

	if err := os.WriteFile(filepath.Join(e.repo, "devbay.yaml"), []byte(`version: 1
project: budget
services:
  web:
    image: nginx:alpine
    port: 80
    primary: true
    health: {http: /}
tasks:
  unit: {run: ["true"], needs: []}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	e.git("add", "-A")
	e.git("-c", "user.email=a@t", "-c", "user.name=a", "commit", "-qm", "budget")

	budget := func(args ...string) string {
		cmd := e.command(args...)
		cmd.Env = append(cmd.Env, "DEVBAY_MAX_BAYS=2")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("devbay %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	for _, name := range []string{"b1", "b2"} {
		budget("new", name)
		t.Cleanup(func() { _, _ = e.try("rm", name, "--force") })
	}
	// The oldest is the one the developer is using, so it must survive.
	budget("focus", "b1")

	budget("new", "b3")
	t.Cleanup(func() { _, _ = e.try("rm", "b3", "--force") })

	// The state is the third field of the header line `<bay> <alias> <state>
	// <branch>`, which is what `devbay status` leads with.
	state := func(bay string) string {
		for _, line := range strings.Split(e.run("status", bay), "\n") {
			if f := strings.Fields(line); len(f) >= 3 && f[0] == bay {
				return f[2]
			}
		}
		return "unknown"
	}
	if got := state("b1"); got == "cold" {
		t.Error("S: the focused bay was stopped to make room, which is the one bay that must not be")
	}
	if got := state("b2"); got != "cold" {
		t.Errorf("S: nothing was cooled for the third bay; b2 is %s, so the budget does not hold", got)
	}
	if got := state("b3"); got == "cold" {
		t.Errorf("S: the new bay did not come up (%s)", got)
	}

	// Cooling keeps the bay: it is a resting state, not a removal.
	e.run("thaw", "b2")
	if code, _ := e.get("b2.budget.localhost", "/"); code != 200 {
		t.Errorf("S: a bay cooled by the budget did not come back; got %d", code)
	}
}
