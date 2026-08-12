package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These files belong to the developer. devbay edits one entry in them and must
// leave everything else exactly as it found it, including the parts it has no
// opinion about.

func TestJSONInstallKeepsTheOtherServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	before := `{
  "mcpServers": {
    "postgres": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-postgres"]}
  },
  "somethingDevbayHasNeverHeardOf": {"keep": "me"}
}`
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Install(Clients[1], path, "/usr/local/bin/devbay"); err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	body, _ := os.ReadFile(path)
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("devbay wrote invalid JSON: %v", err)
	}
	servers := doc["mcpServers"].(map[string]any)
	if _, ok := servers["postgres"]; !ok {
		t.Error("another server was dropped; the file is not devbay's to rewrite")
	}
	if _, ok := doc["somethingDevbayHasNeverHeardOf"]; !ok {
		t.Error("a top-level key devbay does not understand was dropped")
	}
	entry := servers["devbay"].(map[string]any)
	if entry["command"] != "/usr/local/bin/devbay" {
		t.Errorf("command = %v, want the absolute path", entry["command"])
	}
}

func TestJSONInstallIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")

	first, err := Install(Clients[0], path, "/usr/local/bin/devbay")
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || !first.Changed {
		t.Fatalf("the first install did nothing: %+v", first)
	}

	second, err := Install(Clients[0], path, "/usr/local/bin/devbay")
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed {
		t.Error("running it twice rewrote the file, so every install would show up as a diff")
	}
}

// Moving the binary has to be repairable by running the command again.
func TestJSONInstallUpdatesAMovedBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	if _, err := Install(Clients[0], path, "/old/devbay"); err != nil {
		t.Fatal(err)
	}
	res, err := Install(Clients[0], path, "/new/devbay")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("a moved binary was not written")
	}
	if !strings.Contains(res.Before, "/old/devbay") {
		t.Errorf("the replaced entry was not reported: %q", res.Before)
	}
}

// A config devbay cannot parse is a config it must not overwrite.
func TestJSONInstallRefusesToRewriteBrokenJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	broken := `{"mcpServers": {`
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(Clients[0], path, "/usr/local/bin/devbay"); err == nil {
		t.Fatal("devbay overwrote a file it could not parse")
	}
	body, _ := os.ReadFile(path)
	if string(body) != broken {
		t.Error("the unparseable file was modified anyway")
	}
}

// The Codex config is TOML a person wrote, with comments and an order they
// chose. Both survive, because a round trip through a parser loses them.
func TestCodexInstallKeepsCommentsAndOtherTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	before := `# my codex settings
model = "o3"

[mcp_servers.github]
command = "npx"

[tui]
theme = "dark"
`
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(Clients[2], path, "/usr/local/bin/devbay"); err != nil {
		t.Fatal(err)
	}

	after, _ := os.ReadFile(path)
	got := string(after)
	for _, keep := range []string{"# my codex settings", `model = "o3"`, "[mcp_servers.github]", "[tui]", `theme = "dark"`} {
		if !strings.Contains(got, keep) {
			t.Errorf("%q was lost", keep)
		}
	}
	if !strings.Contains(got, "[mcp_servers.devbay]") {
		t.Error("devbay was not added")
	}
}

func TestCodexInstallReplacesRatherThanRepeats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if _, err := Install(Clients[2], path, "/old/devbay"); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(Clients[2], path, "/new/devbay"); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(path)
	if n := strings.Count(string(got), "[mcp_servers.devbay]"); n != 1 {
		t.Errorf("the table appears %d times; Codex would read the last one and the file would grow on every install", n)
	}
	if strings.Contains(string(got), "/old/devbay") {
		t.Error("the old path is still there")
	}
}

func TestCodexInstallIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if _, err := Install(Clients[2], path, "/usr/local/bin/devbay"); err != nil {
		t.Fatal(err)
	}
	res, err := Install(Clients[2], path, "/usr/local/bin/devbay")
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Error("running it twice rewrote the file")
	}
}

// A table that sits at the end of the file has no following header to stop at.
func TestCodexInstallHandlesATrailingTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	before := "model = \"o3\"\n\n[mcp_servers.devbay]\ncommand = \"/old/devbay\"\nargs = [\"mcp\"]\n"
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(Clients[2], path, "/new/devbay"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if strings.Contains(string(got), "/old/devbay") {
		t.Error("the trailing table was not replaced")
	}
	if !strings.Contains(string(got), `model = "o3"`) {
		t.Error("content before the table was lost")
	}
}

// An unknown client should say what the known ones are.
func TestUnknownClientListsTheAlternatives(t *testing.T) {
	_, err := ClientByKey("windsurf")
	if err == nil {
		t.Fatal("an unknown client was accepted")
	}
	for _, want := range []string{"claude", "cursor", "codex"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// Every client has to name somewhere to write, or the install silently does
// nothing for it.
func TestEveryClientHasAConfigPath(t *testing.T) {
	for _, c := range Clients {
		project, global := c.Paths("/repo", "/home/dev")
		if project == "" && global == "" {
			t.Errorf("%s has nowhere to write", c.Name)
		}
		if c.Name == "" || c.Key == "" {
			t.Errorf("a client is missing its name or key: %+v", c)
		}
	}
}

// The instruction files are the part that makes devbay used rather than merely
// reachable, and they belong to whoever wrote them.

func TestRulesKeepWhatWasAlreadyInTheFile(t *testing.T) {
	root := t.TempDir()
	existing := "# my project\n\nConventions the team wrote.\n"
	if err := os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := WriteRules(RuleFiles[0], root); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(filepath.Join(root, "CLAUDE.md"))
	if !strings.Contains(string(got), "Conventions the team wrote.") {
		t.Error("the existing instructions were lost")
	}
	if !strings.Contains(string(got), "bay_run_task") {
		t.Error("the devbay block was not added")
	}
}

func TestRulesAreIdempotent(t *testing.T) {
	root := t.TempDir()
	if _, err := WriteRules(RuleFiles[0], root); err != nil {
		t.Fatal(err)
	}
	res, err := WriteRules(RuleFiles[0], root)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Error("running it twice rewrote the file, so every install would show as a diff")
	}
	got, _ := os.ReadFile(filepath.Join(root, "CLAUDE.md"))
	if n := strings.Count(string(got), ruleBegin); n != 1 {
		t.Errorf("the block appears %d times; the file would grow on every install", n)
	}
}

// Cursor ignores a rule file with no frontmatter, so a created one must have it.
func TestCursorRuleGetsItsFrontmatter(t *testing.T) {
	root := t.TempDir()
	var cursor RuleFile
	for _, f := range RuleFiles {
		if f.Client == "Cursor" {
			cursor = f
		}
	}
	if _, err := WriteRules(cursor, root); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(root, cursor.Path))
	if !strings.HasPrefix(string(got), "---\n") {
		t.Errorf("no frontmatter, so Cursor will not apply the rule:\n%s", string(got)[:40])
	}
	if !strings.Contains(string(got), "alwaysApply: true") {
		t.Error("the rule is not marked to always apply")
	}
}

// The block has to say the two things an agent gets wrong on its own.
func TestRulesSayWhatNotToDo(t *testing.T) {
	for _, want := range []string{"bay_run_task", "docker compose up", "public_url", "bay_create"} {
		if !strings.Contains(RulesBody, want) {
			t.Errorf("the instructions never mention %q", want)
		}
	}
}
