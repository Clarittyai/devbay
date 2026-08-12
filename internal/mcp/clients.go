package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Wiring devbay into the agents people actually use.
//
// devbay already speaks MCP, and until now that was where the story stopped:
// the developer had to find their client's config file, learn its dialect, and
// write the entry by hand. Every one of those steps is a place to give up, and
// an agent that cannot reach devbay is an agent that goes back to running
// `docker compose up` in the repository it is supposed to be isolated from.
//
// So devbay writes the entry. Three clients, three dialects, one command.

// Client is an agent that can be told about an MCP server.
type Client struct {
	// Key is what the developer types: `devbay mcp install --client cursor`.
	Key string
	// Name is what they call it.
	Name string
	// Format is how that client stores servers.
	Format Format
	// Project is the config path relative to a repository root. Empty when the
	// client has no project-scoped configuration.
	Project string
	// Global is the config path relative to the home directory.
	Global string
	// Note is what to say after writing, when the client needs a nudge.
	Note string
}

// Format is a config dialect. Two shapes cover all three clients, which is why
// they are enumerated rather than abstracted.
type Format int

const (
	// FormatMCPJSON is `{"mcpServers": {"devbay": {"command": …}}}`, used by
	// Claude Code and Cursor.
	FormatMCPJSON Format = iota
	// FormatCodexTOML is `[mcp_servers.devbay]` in ~/.codex/config.toml.
	FormatCodexTOML
)

// Clients is the supported set, in the order `devbay mcp install` lists them.
var Clients = []Client{
	{
		Key:    "claude",
		Name:   "Claude Code",
		Format: FormatMCPJSON,
		// Project-scoped and committed on purpose: a bay belongs to a
		// repository, so the wiring belongs there too, and everyone who clones
		// it gets the same tools without being told.
		Project: ".mcp.json",
		Global:  ".claude.json",
		Note:    "Claude Code asks you to approve a project-scoped server the first time it sees one.",
	},
	{
		Key:     "cursor",
		Name:    "Cursor",
		Format:  FormatMCPJSON,
		Project: ".cursor/mcp.json",
		Global:  ".cursor/mcp.json",
		Note:    "Cursor picks it up on the next reload; check Settings, MCP for a green dot.",
	},
	{
		Key:    "codex",
		Name:   "Codex CLI",
		Format: FormatCodexTOML,
		// Codex has no project-scoped config, so this one is always global.
		Global: ".codex/config.toml",
		Note:   "Codex reads this at startup, so restart it.",
	},
}

// ClientByKey finds a client, and lists the alternatives when there is no such
// thing rather than saying only that there is not.
func ClientByKey(key string) (Client, error) {
	for _, c := range Clients {
		if c.Key == key {
			return c, nil
		}
	}
	names := make([]string, 0, len(Clients))
	for _, c := range Clients {
		names = append(names, c.Key)
	}
	return Client{}, fmt.Errorf("no client called %q; devbay knows %s", key, strings.Join(names, ", "))
}

// ServerEntry is what gets written: the command that speaks MCP.
type ServerEntry struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

// Install writes the devbay server into a client's config.
//
// Merging rather than overwriting, because these files belong to the
// developer: a config holding six servers must still hold six servers
// afterwards. The only entry devbay touches is its own, and it reports whether
// it added one, changed one, or found the right one already there.
type Result struct {
	Path    string
	Changed bool
	Created bool
	// Before is the previous devbay entry, when there was one.
	Before string
}

// Install adds or updates the devbay entry in path, in the client's dialect.
func Install(c Client, path, binary string) (Result, error) {
	res := Result{Path: path}

	existing, err := os.ReadFile(path)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		existing, res.Created = nil, true
	default:
		return res, err
	}

	var out []byte
	switch c.Format {
	case FormatCodexTOML:
		out, res.Changed, res.Before, err = mergeCodexTOML(existing, binary)
	default:
		out, res.Changed, res.Before, err = mergeMCPJSON(existing, binary)
	}
	if err != nil {
		return res, err
	}
	if !res.Changed {
		return res, nil
	}

	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return res, err
		}
	}
	return res, os.WriteFile(path, out, 0o644)
}

// mergeMCPJSON edits the `mcpServers` object and leaves everything else alone.
//
// Decoded into a generic map rather than a struct so that keys devbay does not
// know about survive the round trip. A config editor that silently drops the
// fields it has no opinion about is worse than no config editor.
func mergeMCPJSON(existing []byte, binary string) (out []byte, changed bool, before string, err error) {
	doc := map[string]any{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &doc); err != nil {
			return nil, false, "", fmt.Errorf("this file is not valid JSON, so devbay will not rewrite it: %w", err)
		}
	}

	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}

	want := map[string]any{"command": binary, "args": []any{"mcp"}}
	if old, ok := servers["devbay"]; ok {
		if sameJSON(old, want) {
			return nil, false, "", nil
		}
		if b, e := json.Marshal(old); e == nil {
			before = string(b)
		}
	}

	servers["devbay"] = want
	doc["mcpServers"] = servers

	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, false, "", err
	}
	return append(body, '\n'), true, before, nil
}

func sameJSON(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

// codexBlock is the whole of devbay's presence in a Codex config.
func codexBlock(binary string) string {
	return fmt.Sprintf("[mcp_servers.devbay]\ncommand = %q\nargs = [\"mcp\"]\n", binary)
}

// mergeCodexTOML replaces devbay's table and leaves the rest of the file
// untouched, byte for byte.
//
// Written as text rather than through a TOML library on purpose. A config file
// a person wrote has comments and an order they chose, and a round trip through
// a parser loses both. devbay only needs to find one table and replace it.
func mergeCodexTOML(existing []byte, binary string) (out []byte, changed bool, before string, err error) {
	block := codexBlock(binary)
	text := string(existing)

	start, end := codexTableRange(text, "[mcp_servers.devbay]")
	if start == -1 {
		joiner := ""
		if text != "" && !strings.HasSuffix(text, "\n\n") {
			joiner = "\n"
			if strings.HasSuffix(text, "\n") {
				joiner = ""
			}
			joiner += "\n"
		}
		return []byte(text + joiner + block), true, "", nil
	}

	before = strings.TrimSpace(text[start:end])
	if strings.TrimSpace(before) == strings.TrimSpace(block) {
		return nil, false, "", nil
	}
	return []byte(text[:start] + block + text[end:]), true, before, nil
}

// codexTableRange finds a TOML table and everything under it, up to the next
// table header or the end of the file.
func codexTableRange(text, header string) (start, end int) {
	lines := strings.Split(text, "\n")
	offset := 0
	start = -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if start == -1 && trimmed == header {
			start = offset
		} else if start != -1 && strings.HasPrefix(trimmed, "[") {
			return start, offset
		}
		offset += len(lines[i]) + 1
	}
	if start == -1 {
		return -1, -1
	}
	return start, len(text)
}

// Paths returns where an install would write, project scope first.
func (c Client) Paths(repoRoot, home string) (project, global string) {
	if c.Project != "" && repoRoot != "" {
		project = filepath.Join(repoRoot, filepath.FromSlash(c.Project))
	}
	if c.Global != "" && home != "" {
		global = filepath.Join(home, filepath.FromSlash(c.Global))
	}
	return project, global
}

// SortedKeys is the client list as keys, for messages.
func SortedKeys() []string {
	out := make([]string, 0, len(Clients))
	for _, c := range Clients {
		out = append(out, c.Key)
	}
	sort.Strings(out)
	return out
}

// ToolNames lists the tools an agent gets, without needing a live server.
//
// Kept beside the client list because it is shown at the end of an install:
// telling somebody the wiring worked is less useful than telling them what
// their agent can now do.
func ToolNames() []string {
	return []string{
		"bay_create", "bay_list", "bay_run_task", "bay_logs",
		"bay_url", "bay_status", "bay_destroy",
	}
}
