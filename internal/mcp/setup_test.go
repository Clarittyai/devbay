package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoAt builds a server pointed at a directory, the way one is when it is
// started in a repository that has no manifest and therefore no manager.
func repoAt(t *testing.T, dir string) *Server {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	s := &Server{index: map[string]*Tool{}}
	s.registerSetup()
	return s
}

func result(t *testing.T, s *Server, tool string, args string) map[string]any {
	t.Helper()
	h := s.index[tool]
	if h == nil {
		t.Fatalf("no tool %q", tool)
	}
	out, err := h.handler(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("%s returned %T, want an object", tool, out)
	}
	return m
}

const composeFixture = `services:
  db:
    image: postgres:16-alpine
    environment:
      POSTGRES_PASSWORD: pw
  web:
    build: .
    ports: ["8080:8080"]
    depends_on: [db]
`

func repoWithCompose(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(composeFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The first question an agent has about a repository it has not seen.
func TestRepoStatusSaysWhatIsMissing(t *testing.T) {
	dir := repoWithCompose(t)
	s := repoAt(t, dir)

	out := result(t, s, "repo_status", `{}`)
	if out["has_manifest"] != false || out["ready"] != false {
		t.Errorf("a repository with no devbay.yaml reported ready=%v has_manifest=%v", out["ready"], out["has_manifest"])
	}
	if next, _ := out["next"].(string); !strings.Contains(next, "repo_init") {
		t.Errorf("next = %q; it has to name the tool that fixes this", next)
	}
}

// The proposal is a proposal: looking at it must not change the repository.
func TestRepoInitWritesNothingUnlessAsked(t *testing.T) {
	dir := repoWithCompose(t)
	s := repoAt(t, dir)

	out := result(t, s, "repo_init", `{}`)
	if out["written"] != false {
		t.Error("repo_init wrote the file without being asked")
	}
	if _, err := os.Stat(filepath.Join(dir, "devbay.yaml")); !os.IsNotExist(err) {
		t.Error("devbay.yaml exists after a read-only call")
	}
	if yaml, _ := out["yaml"].(string); !strings.Contains(yaml, "version: 1") {
		t.Error("the proposal itself was not returned, so there is nothing to review")
	}

	out = result(t, s, "repo_init", `{"write":true}`)
	if out["written"] != true {
		t.Fatal("write=true did not write")
	}
	if _, err := os.Stat(filepath.Join(dir, "devbay.yaml")); err != nil {
		t.Fatalf("devbay.yaml missing after write=true: %v", err)
	}
	// A bay is a fresh checkout, so a manifest that is written but not
	// committed is not in it. Saying so here saves the failure later.
	if note, _ := out["note"].(string); !strings.Contains(note, "commit") {
		t.Errorf("note = %q; it should say the manifest has to be committed", note)
	}
}

// Overwriting a file someone wrote is not a thing to do by default.
func TestRepoInitRefusesToReplaceWithoutForce(t *testing.T) {
	dir := repoWithCompose(t)
	s := repoAt(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "devbay.yaml"), []byte("version: 1\nproject: mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := s.index["repo_init"].handler(context.Background(), json.RawMessage(`{"write":true}`)); err == nil {
		t.Fatal("replaced an existing devbay.yaml without force")
	}
	body, _ := os.ReadFile(filepath.Join(dir, "devbay.yaml"))
	if !strings.Contains(string(body), "project: mine") {
		t.Error("the existing manifest was overwritten anyway")
	}
}

// The gaps are the agent's work list, and they have to arrive as data.
func TestRepoInitReturnsItsGapsAsData(t *testing.T) {
	dir := repoWithCompose(t)
	s := repoAt(t, dir)

	out := result(t, s, "repo_init", `{}`)
	gaps, ok := out["undecided"].([]string)
	if !ok || len(gaps) == 0 {
		t.Fatalf("undecided = %#v; a proposal devbay could not fully decide must say what is left", out["undecided"])
	}
	for _, g := range gaps {
		if strings.HasPrefix(strings.TrimSpace(g), "#") {
			t.Errorf("gap %q is still a YAML comment rather than a statement", g)
		}
	}
}

// Findings have to be objects. A rule and a path are what an agent edits by.
func TestManifestValidateReturnsLocatedFindings(t *testing.T) {
	s := repoAt(t, t.TempDir())

	// No health probe (R5), and a command outside the allowlist (R2).
	bad := `{"yaml":"version: 1\nproject: p\nservices:\n  web:\n    image: nginx:1\n    port: 80\n    start: [sh, -c, run]\ntasks: {}\n"}`
	out := result(t, s, "manifest_validate", bad)

	if out["valid"] != false {
		t.Error("a manifest with no health probe validated")
	}
	found := map[string]bool{}
	var approvalArgv bool
	for _, d := range out["diagnostics"].([]map[string]any) {
		if rule, _ := d["rule"].(string); rule != "" {
			found[rule] = true
		}
		if sev, _ := d["severity"].(string); sev != "error" && sev != "warn" && sev != "approval" {
			t.Errorf("severity = %q, which is not a name; an int conversion yields a rune", sev)
		}
		if _, ok := d["argv"]; ok {
			approvalArgv = true
			if who, _ := d["approval"].(string); !strings.Contains(who, "human") {
				t.Error("an approval finding does not say a human has to grant it")
			}
		}
	}
	if !found["R5"] {
		t.Error("no R5 finding for a service with no health probe")
	}
	if !approvalArgv {
		t.Error("the R2 finding did not carry the argv a human is being asked to approve")
	}
}

// A manifest that does not parse is a finding about the manifest, not a
// failure of the call: an agent wants to know what is wrong with it.
func TestManifestValidateReportsAParseFailureAsAFinding(t *testing.T) {
	s := repoAt(t, t.TempDir())
	out := result(t, s, "manifest_validate", `{"yaml":"version: 1\nservices: [this is not a map]\n"}`)
	if out["parses"] != false || out["valid"] != false {
		t.Errorf("parses=%v valid=%v for unparseable YAML", out["parses"], out["valid"])
	}
	if msg, _ := out["parse_error"].(string); msg == "" {
		t.Error("no parse_error, so the agent cannot tell what is wrong")
	}
}
