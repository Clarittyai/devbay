package mcp

import (
	"os"
	"path/filepath"
	"strings"
)

// Telling the agent how to work, not just what it may call.
//
// Wiring the server in makes devbay reachable. It does not make it used. An
// agent reads the repository's instruction file at the start of a session and
// forms a plan from it, long before it looks at a tool list, so a repository
// that says nothing gets a plan built around `npm test` and `docker compose
// up`. The tools are then available and ignored, and the ports collide anyway.
//
// So the install writes a short block into the files each client actually
// reads. It is fenced by sentinels, rewritten in place, and removable by
// deleting the fence.

const (
	ruleBegin = "<!-- devbay:begin -->"
	ruleEnd   = "<!-- devbay:end -->"
)

// RulesBody is what the agent is told. Imperative, short, and specific about
// the two mistakes an agent actually makes: running the test command itself,
// and starting the stack outside a bay.
const RulesBody = `## Running and testing this repository

This repository uses devbay. Every branch gets its own containers, database,
ports and browser origin, so several can run at once without colliding.

- If you do not know whether this repository is set up for devbay yet, ask
  ` + "`repo_status`" + `. It names the next step, and ` + "`repo_init`" + `
  proposes a devbay.yaml when there is none.
- Create a bay before running or verifying anything: ` + "`bay_create`" + `.
- Run tests with ` + "`bay_run_task`" + `, not by running the test command
  yourself. It starts only the services the task declares it needs, so a unit
  suite boots nothing, and failures come back with a file, a line and the
  assertion instead of output to parse.
- Do not run ` + "`docker compose up`" + `, ` + "`docker run`" + `, or the
  application's dev server directly. They bind the ports the bays own and
  undo the isolation.
- Open ` + "`public_url`" + ` in a browser and call ` + "`url`" + ` from code.
  They are different addresses on purpose.
- Use ` + "`bay_logs`" + ` when a failure needs more than the structured
  result, and ` + "`bay_destroy`" + ` when the work is merged or abandoned.`

// RuleFile is an instruction file a client reads.
type RuleFile struct {
	// Path is relative to the repository root.
	Path string
	// Client names who reads it, for the install output.
	Client string
	// Frontmatter is prepended when the file is created, for clients that
	// need it. Cursor ignores a rule file without one.
	Frontmatter string
}

// RuleFiles are the conventional locations, one per client.
var RuleFiles = []RuleFile{
	{Path: "CLAUDE.md", Client: "Claude Code"},
	{Path: "AGENTS.md", Client: "Codex CLI"},
	{
		Path:        filepath.Join(".cursor", "rules", "devbay.mdc"),
		Client:      "Cursor",
		Frontmatter: "---\ndescription: How to run and test this repository\nalwaysApply: true\n---\n\n",
	},
}

// WriteRules puts the block into path, replacing an existing one.
//
// Appended rather than merged into the prose, because these files belong to
// whoever wrote them: a CLAUDE.md with a team's conventions in it must still
// have them afterwards, in the order they chose.
func WriteRules(f RuleFile, root string) (Result, error) {
	path := filepath.Join(root, f.Path)
	res := Result{Path: path}

	existing, err := os.ReadFile(path)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		existing, res.Created = nil, true
	default:
		return res, err
	}

	block := ruleBegin + "\n" + RulesBody + "\n" + ruleEnd + "\n"
	text := string(existing)

	if start := strings.Index(text, ruleBegin); start != -1 {
		end := strings.Index(text[start:], ruleEnd)
		if end != -1 {
			end += start + len(ruleEnd)
			// Take the trailing newline with it, so rewriting does not add one
			// line of whitespace per run.
			if strings.HasPrefix(text[end:], "\n") {
				end++
			}
			if text[start:end] == block {
				return res, nil
			}
			text = text[:start] + block + text[end:]
			res.Changed = true
			return res, write(path, text)
		}
	}

	var b strings.Builder
	if res.Created && f.Frontmatter != "" {
		b.WriteString(f.Frontmatter)
	}
	b.WriteString(text)
	if text != "" && !strings.HasSuffix(text, "\n\n") {
		if !strings.HasSuffix(text, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	b.WriteString(block)

	res.Changed = true
	return res, write(path, b.String())
}

func write(path, body string) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, []byte(body), 0o644)
}
