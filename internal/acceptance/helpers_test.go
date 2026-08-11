//go:build acceptance

package acceptance

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// readManifest returns the generated devbay.yaml.
func (e *env) readManifest() string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.repo, "devbay.yaml"))
	if err != nil {
		e.t.Fatalf("B: init wrote no manifest: %v", err)
	}
	return string(b)
}

// addTasks writes the tasks a developer adds, since init will not invent them.
func (e *env) addTasks() {
	e.t.Helper()
	p := filepath.Join(e.repo, "devbay.yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		e.t.Fatal(err)
	}
	s := strings.Replace(string(b),
		"      http: /\n      timeout: 60s\n    watch:\n      - api/**",
		"      http: /healthz\n      timeout: 60s\n    watch:\n      - api/**", 1)
	// A real secret reference, so the leak check has something to leak. A
	// canary that is never delivered proves nothing about scrubbing.
	s = strings.Replace(s, "    env:\n      DATABASE_URL:",
		"    env:\n      CANARY: ${secret:acceptance/canary}\n      DATABASE_URL:", 1)
	s = strings.Replace(s, "tasks: {}", `tasks:
  unit:
    run: [node, --test, --test-reporter=junit, --test-reporter-destination=reports/junit.xml, api/server.test.js]
    needs: []
    report: {format: junit, path: reports/junit.xml}
  integration:
    run: [node, -e, "fetch(process.env.API+'/tasks').then(r=>r.json()).then(d=>{if(!Array.isArray(d.tasks))throw new Error('bad shape')})"]
    needs: [api]
    env: {API: "${bay.api.url}"}
`, 1)
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// worktree is where a bay's checkout lives.
func (e *env) worktree(bay string) string {
	e.t.Helper()
	for _, line := range strings.Split(e.run("status", bay), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "worktree" {
			return f[1]
		}
	}
	e.t.Fatalf("could not find the worktree of bay %q", bay)
	return ""
}

// breakATest makes one assertion fail, the way a change under development does.
func (e *env) breakATest(bay string) {
	e.t.Helper()
	p := filepath.Join(e.worktree(bay), "api", "server.test.js")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		e.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("\ntest('deliberately broken', () => {\n  assert.strictEqual(2, 3);\n});\n"); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) fixTheTest(bay string) {
	e.t.Helper()
	p := filepath.Join(e.worktree(bay), "api", "server.test.js")
	b, err := os.ReadFile(p)
	if err != nil {
		e.t.Fatal(err)
	}
	s := strings.Replace(string(b),
		"\ntest('deliberately broken', () => {\n  assert.strictEqual(2, 3);\n});\n", "", 1)
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// editAndWatch changes a file in the bay's worktree and checks the change is
// served -- which is the whole claim behind `watch:`.
func (e *env) editAndWatch(bay string) {
	e.t.Helper()
	wt := e.worktree(bay)

	cmd := exec.Command(e.bin, "watch", bay)
	cmd.Dir = e.repo
	if err := cmd.Start(); err != nil {
		e.t.Fatalf("H: devbay watch would not start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	time.Sleep(3 * time.Second) // let the tree be registered

	p := filepath.Join(wt, "web", "server.js")
	b, err := os.ReadFile(p)
	if err != nil {
		e.t.Fatal(err)
	}
	const marker = "watch-applied-marker"
	s := strings.Replace(string(b), "<h1>taskboard", "<h1>"+marker, 1)
	if s == string(b) {
		e.t.Fatal("H: the example changed shape; the edit had no effect on the file")
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		e.t.Fatal(err)
	}

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if _, body := e.get(bay+".app.localhost", "/"); strings.Contains(body, marker) {
			return
		}
		time.Sleep(2 * time.Second)
	}
	e.t.Error("H: an edit in the bay's worktree never reached the container")
}

// commitInBay does what a developer does before throwing the bay away.
func (e *env) commitInBay(bay string) {
	e.t.Helper()
	wt := e.worktree(bay)
	for _, args := range [][]string{
		{"add", "-A"},
		{"-c", "user.email=a@t", "-c", "user.name=a", "commit", "-qm", "work done in the bay"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = wt
		if out, err := cmd.CombinedOutput(); err != nil && !strings.Contains(string(out), "nothing to commit") {
			e.t.Fatalf("committing in the bay: %v\n%s", err, out)
		}
	}
}

// assertNoSecretLeak plants a credential and checks it never comes back out.
//
// The value is handed to the bay the way a developer's secret manager would,
// so it genuinely reaches the container -- a test that never sets the secret
// proves nothing about scrubbing.
func (e *env) assertNoSecretLeak(bay string) {
	e.t.Helper()

	// It has to have reached the container, or the rest of this proves only
	// that a string nobody ever set is absent.
	out, err := exec.Command("docker", "exec", "devbay-app-"+bay+"-api", "printenv", "CANARY").Output()
	if err != nil || !strings.Contains(string(out), canary) {
		e.t.Fatalf("N: the secret never reached the container, so the leak check would prove nothing (got %q, %v)",
			strings.TrimSpace(string(out)), err)
	}
	// And the application prints it, which is how a credential usually escapes.
	e.get("api."+bay+".app.localhost", "/leak")

	logs, _ := e.try("logs", bay, "api", "-n", "200")
	status := e.mcp("bay_status", map[string]any{"bay": bay})
	blob, _ := os.ReadFile(filepath.Join(e.repo, "devbay.yaml"))

	for what, text := range map[string]string{
		"the logs devbay returns": logs,
		"an MCP response":         fmt.Sprint(status),
		"the manifest on disk":    string(blob),
	} {
		if strings.Contains(text, canary) {
			e.t.Errorf("N: a credential appeared in %s", what)
		}
	}
}

// canary is the credential planted for scenario N. Shaped like a real one, so
// the shape-based scrubber would catch it even if the value-based one did not.
const canary = "sk_live_ACCEPTANCE_CANARY_9f2ad41c"

// runWithSecret invokes devbay with the canary available to the broker, the way
// a developer's secret manager would supply it.
func (e *env) runWithSecret(args ...string) string {
	e.t.Helper()
	cmd := exec.Command(e.bin, args...)
	cmd.Dir = e.repo
	cmd.Env = append(os.Environ(),
		"DEVBAY_NO_MODEL=1",
		"DEVBAY_SECRET_ACCEPTANCE_CANARY="+canary)
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("devbay %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
