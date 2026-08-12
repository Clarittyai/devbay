package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Clarittyai/devbay/internal/mcp"
)

// `devbay mcp install` writes devbay into an agent's MCP configuration.
//
// The gap this closes is small and it stopped people cold. devbay has spoken
// MCP since the first release, and using it still meant finding your client's
// config file, learning whether it wanted JSON or TOML and whether the key was
// `mcpServers` or `mcp_servers`, and getting the path to the binary right. An
// agent that cannot reach devbay runs `docker compose up` in the repository it
// was supposed to stay out of, which is the whole problem coming back.
func cmdMCPInstall(args []string) error {
	fs := flag.NewFlagSet("mcp install", flag.ExitOnError)
	client := fs.String("client", "", "claude, cursor or codex; omit to write all three")
	global := fs.Bool("global", false, "write the user-level config instead of this repository's")
	dry := fs.Bool("dry-run", false, "print what would change and write nothing")
	fs.Parse(permute(args))

	binary, err := devbayPath()
	if err != nil {
		return err
	}

	targets := mcp.Clients
	if *client != "" {
		c, err := mcp.ClientByKey(*client)
		if err != nil {
			return err
		}
		targets = []mcp.Client{c}
	}

	repoRoot := repoRootOrCwd()
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	var wrote int
	for _, c := range targets {
		project, globalPath := c.Paths(repoRoot, home)

		path := project
		scope := "this repository"
		if *global || path == "" {
			path, scope = globalPath, "your user config"
		}
		if path == "" {
			fmt.Printf("  %s %s has no config devbay can write\n", yellow("--"), c.Name)
			continue
		}

		if *dry {
			fmt.Printf("  %s %-12s %s %s\n", dim("would write"), c.Name, dim("→"), path)
			continue
		}

		res, err := mcp.Install(c, path, binary)
		if err != nil {
			return fmt.Errorf("%s: %w", c.Name, err)
		}

		switch {
		case !res.Changed:
			fmt.Printf("  %s %-12s already points at this devbay\n", dim("ok"), c.Name)
			continue
		case res.Created:
			fmt.Printf("  %s %-12s %s\n", green("wrote"), c.Name, dim(short(path, home)))
		default:
			fmt.Printf("  %s %-12s %s\n", green("updated"), c.Name, dim(short(path, home)))
		}
		if res.Before != "" {
			fmt.Printf("       %s %s\n", dim("replaced"), dim(res.Before))
		}
		if c.Note != "" {
			fmt.Printf("       %s\n", dim(c.Note))
		}
		_ = scope
		wrote++
	}

	if *dry {
		return nil
	}
	if wrote > 0 {
		fmt.Printf("\n%s ask your agent to create a bay and run a task. It has seven tools:\n", green("done"))
		fmt.Printf("  %s\n", dim(strings.Join(toolNames(), ", ")))
	}
	return nil
}

// devbayPath is the command the agent will run.
//
// An absolute path, because the client starts the server itself and its PATH is
// not the shell's: a GUI editor launched from Finder or Spotlight often cannot
// see /usr/local/bin at all, and `command: "devbay"` then fails with something
// that reads like devbay is broken rather than absent.
func devbayPath() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			return resolved, nil
		}
		return exe, nil
	}
	if found, lerr := exec.LookPath("devbay"); lerr == nil {
		return found, nil
	}
	return "", fmt.Errorf("could not work out where devbay is installed: %w", err)
}

// repoRootOrCwd finds the repository this is being run in, so the config lands
// beside the code rather than four directories up.
func repoRootOrCwd() string {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// short writes ~ for the home directory, the way a person would.
func short(path, home string) string {
	if home != "" && strings.HasPrefix(path, home) {
		return "~" + path[len(home):]
	}
	return path
}

func toolNames() []string { return mcp.ToolNames() }
