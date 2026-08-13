package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/Clarittyai/devbay/internal/introspect"
	"github.com/Clarittyai/devbay/internal/manifest"
)

// Getting a repository ready, as tools rather than as advice.
//
// The bay tools all assume a devbay.yaml that already exists. Everything that
// produces one lived only in the CLI, so an agent asked to set a repository up
// had to shell out -- which is the thing this package's own doc says it is
// not: a wrapper around a terminal, guessing at formatting that changes
// between versions.
//
// It also loses the part that matters most. `devbay init` records what it
// could not work out, and in the file those are YAML comments: an agent has to
// read prose back out of a document it just generated to find its own work
// list. Here they are a list of strings, and the diagnostics that would have
// been terminal output are objects with a rule, a path and a message.
//
// `approve` is deliberately absent. R2 exists so that a human sees the exact
// argv before anything outside the allowlist runs, and a tool that let an
// agent approve its own commands would remove the only checkpoint that rule
// has.

func (s *Server) registerSetup() {
	s.add(Tool{
		Name:  "repo_init",
		Title: "Propose a devbay.yaml for this repository",
		Description: "Read what this repository already says about itself -- a compose file, a devcontainer, " +
			"GitHub Actions services, a Procfile, package manifests -- and propose a devbay.yaml. " +
			"Use this once, before creating any bay, on a repository that does not have one yet. " +
			"Returns the services and tasks it found, the evidence for each, and the things it could not " +
			"work out, which are the decisions left for you to make. Writes nothing unless write=true.",
		InputSchema: object(map[string]any{
			"write": prop("boolean", "Write the proposal to devbay.yaml. Defaults to false, which returns it for review without touching the repository."),
			"force": prop("boolean", "Overwrite an existing devbay.yaml. Defaults to false, which refuses rather than replacing a file someone wrote."),
		}),
		handler: s.repoInit,
	})

	s.add(Tool{
		Name:  "manifest_validate",
		Title: "Check a devbay.yaml against R1-R7",
		Description: "Validate this repository's devbay.yaml and return each finding as an object with its rule, " +
			"path and message. Use it after editing the manifest and before creating a bay: a manifest that " +
			"does not validate cannot boot, and the finding names the exact key to fix. " +
			"Findings marked approval are not errors -- they are commands outside the default allowlist, " +
			"which a human has to approve with `devbay approve` at a terminal.",
		InputSchema: object(map[string]any{
			"yaml": prop("string", "Manifest text to check instead of the one in the repository. Use this to check an edit before writing it."),
		}),
		handler: s.manifestValidate,
	})

	s.add(Tool{
		Name:  "repo_status",
		Title: "Is this repository ready for devbay",
		Description: "Report whether this repository can run bays yet: whether a devbay.yaml exists, whether it " +
			"validates, what services and tasks it declares, and what is missing. " +
			"Use this first, before anything else, when you do not know how a repository is set up.",
		InputSchema: object(nil),
		handler:     s.repoStatus,
	})
}

// repoDir is the repository these tools operate on.
func (s *Server) repoDir() (string, error) {
	if s.mgr != nil && s.mgr.RepoRoot != "" {
		return s.mgr.RepoRoot, nil
	}
	// A repository with no manifest yet has no manager, and that is exactly
	// when repo_init is the tool to call.
	return os.Getwd()
}

type initArgs struct {
	Write bool `json:"write"`
	Force bool `json:"force"`
}

func (s *Server) repoInit(ctx context.Context, raw json.RawMessage) (any, error) {
	var args initArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, err
		}
	}
	dir, err := s.repoDir()
	if err != nil {
		return nil, err
	}
	out := filepath.Join(dir, "devbay.yaml")
	if args.Write && !args.Force {
		if _, err := os.Stat(out); err == nil {
			return nil, fmt.Errorf("devbay.yaml already exists; pass force=true to replace it, or write=false to see the proposal without touching it")
		}
	}

	res, err := introspect.Detect(ctx, dir)
	if err != nil {
		return nil, err
	}
	data, err := introspect.Render(res)
	if err != nil {
		return nil, err
	}
	report, err := introspect.Verify(data)
	if err != nil {
		return nil, fmt.Errorf("the generated manifest does not parse, which is a devbay bug: %w", err)
	}

	evidence := make([]map[string]any, 0, len(res.Evidence))
	for _, e := range res.Evidence {
		evidence = append(evidence, map[string]any{
			"source": string(e.Source),
			"path":   e.Path,
			"detail": e.Detail,
		})
	}
	services := make([]string, 0, len(res.Manifest.Services))
	for name := range res.Manifest.Services {
		services = append(services, name)
	}
	tasks := make([]string, 0, len(res.Manifest.Tasks))
	for name := range res.Manifest.Tasks {
		tasks = append(tasks, name)
	}
	sort.Strings(services)
	sort.Strings(tasks)

	result := map[string]any{
		"services": services,
		"tasks":    tasks,
		"evidence": evidence,
		// The gaps are the point. In the file they are comments; here they are
		// the work list, and every one of them is a decision devbay could not
		// make from the repository alone.
		"undecided":   res.Gaps,
		"diagnostics": diagnostics(report),
		"valid":       report.OK(),
		"yaml":        string(data),
		"written":     false,
	}
	if args.Write {
		if err := os.WriteFile(out, data, 0o644); err != nil {
			return nil, err
		}
		result["written"] = true
		result["path"] = out
		result["note"] = "commit devbay.yaml before creating a bay: a bay is a fresh checkout of the branch, " +
			"and an uncommitted manifest is not in it"
	} else {
		result["note"] = "nothing was written. Review `yaml`, settle everything in `undecided`, then call again with write=true"
	}
	return result, nil
}

type validateArgs struct {
	YAML string `json:"yaml"`
}

func (s *Server) manifestValidate(ctx context.Context, raw json.RawMessage) (any, error) {
	var args validateArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, err
		}
	}
	data := []byte(args.YAML)
	source := "the text you supplied"
	if len(data) == 0 {
		dir, err := s.repoDir()
		if err != nil {
			return nil, err
		}
		path := filepath.Join(dir, "devbay.yaml")
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("no devbay.yaml in this repository; call repo_init to propose one")
		}
		source = path
	}

	report, err := introspect.Verify(data)
	if err != nil {
		// A parse failure is a finding, not a tool failure: the agent wants to
		// know what is wrong with the manifest, not that the call did not work.
		return map[string]any{
			"source":      source,
			"valid":       false,
			"parses":      false,
			"diagnostics": []any{},
			"parse_error": err.Error(),
		}, nil
	}
	return map[string]any{
		"source":      source,
		"valid":       report.OK(),
		"parses":      true,
		"diagnostics": diagnostics(report),
	}, nil
}

func (s *Server) repoStatus(ctx context.Context, _ json.RawMessage) (any, error) {
	dir, err := s.repoDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "devbay.yaml")
	out := map[string]any{"repository": dir, "manifest": path}

	data, err := os.ReadFile(path)
	if err != nil {
		out["ready"] = false
		out["has_manifest"] = false
		out["next"] = "call repo_init to propose a devbay.yaml from what this repository already says about itself"
		return out, nil
	}
	out["has_manifest"] = true

	m, perr := manifest.Parse(data)
	if perr != nil {
		out["ready"] = false
		out["parses"] = false
		out["parse_error"] = perr.Error()
		out["next"] = "fix the manifest; manifest_validate will point at the key"
		return out, nil
	}
	report := manifest.Validate(m)

	services := make([]string, 0, len(m.Services))
	for name := range m.Services {
		services = append(services, name)
	}
	tasks := make([]string, 0, len(m.Tasks))
	for name := range m.Tasks {
		tasks = append(tasks, name)
	}
	sort.Strings(services)
	sort.Strings(tasks)

	out["parses"] = true
	out["project"] = m.Project
	out["services"] = services
	out["tasks"] = tasks
	out["valid"] = report.OK()
	out["diagnostics"] = diagnostics(report)
	out["ready"] = report.OK()

	// A task with no `report:` runs fine and comes back as text, so the one
	// thing bay_run_task exists to give an agent -- a file, a line and an
	// assertion -- is missing, and nothing about the call says so except a
	// `parsed: false` that is easy to read past. Said here because an agent
	// that arrives at a repository someone else set up never sees repo_init's
	// version of this.
	typed := 0
	for _, t := range m.Tasks {
		if t != nil && t.Report != nil {
			typed++
		}
	}

	switch {
	case !report.OK():
		out["next"] = "the manifest does not validate; each diagnostic names the key to fix"
	case len(tasks) > 0 && typed == 0:
		out["untyped_tasks"] = true
		out["next"] = fmt.Sprintf("ready, but no task declares `report:`, so bay_run_task will return output "+
			"rather than failures with a file and a line. Add `report:` to %s if you want to act on failures "+
			"directly; otherwise call bay_create", tasks[0])
	case len(tasks) == 0:
		// Worth saying rather than leaving to be discovered: with no tasks
		// declared there is nothing to run, and the agent's only remaining
		// option is the test command it was told not to run itself.
		out["next"] = "this manifest declares no tasks, so bay_run_task has nothing to run. " +
			"Add one under `tasks:` -- a unit suite with `needs: []` boots no containers"
	default:
		out["next"] = "ready; call bay_create to make an isolated environment"
	}
	return out, nil
}

// diagnostics turns a validation result into objects an agent can act on,
// rather than the lines a person reads.
func diagnostics(r *manifest.Result) []map[string]any {
	if r == nil {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(r.Diagnostics))
	for _, d := range r.Diagnostics {
		item := map[string]any{
			// Severity.String(), not a conversion: it is an int, and converting
			// it yields the rune at that code point rather than "error".
			"severity": d.Severity.String(),
			"path":     d.Path,
			"message":  d.Msg,
		}
		if d.Rule != "" {
			item["rule"] = d.Rule
		}
		if len(d.Argv) > 0 {
			// Shown verbatim, because this is the command a human is being
			// asked to approve and paraphrasing it would defeat the point.
			item["argv"] = []string(d.Argv)
			item["approval"] = "a human must run `devbay approve` at a terminal; " +
				"this is not something an agent can grant itself"
		}
		out = append(out, item)
	}
	return out
}
