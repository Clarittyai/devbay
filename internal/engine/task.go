package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/moby/moby/client"

	"github.com/Clarittyai/devbay/internal/manifest"
	"github.com/Clarittyai/devbay/internal/report"
)

// TaskResult is what running a task produces.
//
// Everything here is typed. An agent that receives {file, line, message} can
// open the file and fix the line; an agent that receives stdout has to guess
// at the runner's format, and it guesses differently each time.
type TaskResult struct {
	Task       string           `json:"task"`
	ExitCode   int              `json:"exit_code"`
	DurationMS int64            `json:"duration_ms"`
	Total      int              `json:"total,omitempty"`
	Passed     int              `json:"passed,omitempty"`
	Failed     int              `json:"failed,omitempty"`
	Skipped    int              `json:"skipped,omitempty"`
	Failures   []report.Failure `json:"failures,omitempty"`
	// Output is the tail of the run, scrubbed. Present so a failure with no
	// parseable report is still actionable rather than opaque.
	Output string `json:"output,omitempty"`
	// Parsed reports whether structured results were available. An agent that
	// sees false knows the failure list is empty because nothing could be
	// parsed, not because nothing failed.
	Parsed bool `json:"parsed"`
}

// Succeeded reports whether the task passed.
func (r *TaskResult) Succeeded() bool { return r.ExitCode == 0 }

// maxTaskOutput bounds what a task's output can contribute to a response. A
// runaway suite must not be able to push megabytes into an agent's context.
const maxTaskOutput = 64 << 10

// RunTask materializes what a task needs, runs it, and parses the result.
//
// The materialization is the point of making `needs` mandatory: a task that
// declares no services boots no containers and returns in the time the tests
// take, rather than the time a full stack takes to come up.
func (e *Engine) RunTask(ctx context.Context, taskName string) (*TaskResult, error) {
	t, ok := e.m.Tasks[taskName]
	if !ok {
		return nil, fmt.Errorf("engine: unknown task %q", taskName)
	}

	plan, err := TaskPlan(e.m, taskName)
	if err != nil {
		return nil, err
	}
	if err := e.Up(ctx, plan); err != nil {
		return nil, fmt.Errorf("engine: preparing %q: %w", taskName, err)
	}

	target, err := e.taskTarget(t)
	if err != nil {
		return nil, err
	}

	if t.Timeout != "" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, parseDuration(t.Timeout, 10*time.Minute))
		defer cancel()
	}

	// The report file is written into the worktree, which is bind-mounted, so
	// it lands on the host and needs no copying out of the container. Removing
	// a stale one first means a run that crashes before writing cannot be
	// scored against the previous run's results.
	var reportPath string
	if t.Report != nil && t.Report.Path != "" {
		reportPath = filepath.Join(e.worktree, t.Report.Path)
		_ = os.Remove(reportPath)
	}

	env, err := e.res.ResolveEnv(t.Env, PlaneContainer)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	code, output, err := e.exec(ctx, target, t.Run, env)
	if err != nil {
		return nil, err
	}

	res := &TaskResult{
		Task:       taskName,
		ExitCode:   code,
		DurationMS: time.Since(start).Milliseconds(),
		Output:     tail(e.scrubText(output), maxTaskOutput),
	}

	if t.Report != nil {
		parsed, perr := e.parseReport(t.Report, reportPath, output)
		if perr == nil && parsed != nil {
			res.Parsed = true
			res.Total, res.Passed = parsed.Total, parsed.Passed
			res.Failed, res.Skipped = parsed.Failed, parsed.Skipped
			for i := range parsed.Failures {
				parsed.Failures[i].Message = e.scrubText(parsed.Failures[i].Message)
				parsed.Failures[i].Output = tail(e.scrubText(parsed.Failures[i].Output), 4<<10)
			}
			res.Failures = parsed.Failures
		}
	}
	return res, nil
}

// parseReport reads structured results, from a file or from stdout.
func (e *Engine) parseReport(r *manifest.Report, path, stdout string) (*report.Result, error) {
	if path == "" {
		// Streaming formats write nothing; the events were on stdout.
		return report.Parse(r.Format, strings.NewReader(stdout))
	}
	f, err := os.Open(path)
	if err != nil {
		// A missing report is not fatal: the exit code still says whether the
		// task passed, and Parsed:false tells the caller not to read the empty
		// failure list as "nothing failed".
		return nil, err
	}
	defer f.Close()
	return report.Parse(r.Format, f)
}

// taskTarget picks the service whose container a task runs in.
func (e *Engine) taskTarget(t *manifest.Task) (string, error) {
	if t.In != "" {
		return t.In, nil
	}
	if p := e.m.PrimaryService(); p != "" {
		return p, nil
	}
	names := make([]string, 0, len(e.m.Services))
	for n := range e.m.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		if !e.m.Services[name].IsOneshot() {
			return name, nil
		}
	}
	return "", errors.New("engine: task has nowhere to run; set `in` on the task")
}

// exec runs a command in a service's container, starting a throwaway one when
// the service is not running.
//
// The throwaway path is what lets a task with `needs: []` work at all: there
// is no running container to exec into, because booting one would defeat the
// purpose.
func (e *Engine) exec(ctx context.Context, service string, argv manifest.Argv, env map[string]string) (int, string, error) {
	id, err := e.containerID(ctx, service)
	if err == nil {
		if alive, _, cerr := e.running(ctx, id); cerr == nil && alive {
			return e.execIn(ctx, id, argv, env)
		}
	}
	return e.execThrowaway(ctx, service, argv, env)
}

func (e *Engine) execIn(ctx context.Context, id string, argv manifest.Argv, env map[string]string) (int, string, error) {
	created, err := e.cli.ExecCreate(ctx, id, client.ExecCreateOptions{
		Cmd:          argv,
		Env:          envList(env),
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return 0, "", err
	}
	att, err := e.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return 0, "", err
	}
	defer att.Close()

	var sb strings.Builder
	if _, err := io.Copy(&sb, io.LimitReader(demux(att.Reader), 8<<20)); err != nil && !errors.Is(err, io.EOF) {
		return 0, sb.String(), err
	}

	for {
		ins, err := e.cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
		if err != nil {
			return 0, sb.String(), err
		}
		if !ins.Running {
			return ins.ExitCode, sb.String(), nil
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return 0, sb.String(), ctx.Err()
		}
	}
}

// execThrowaway runs a command in a fresh container built like the service.
func (e *Engine) execThrowaway(ctx context.Context, service string, argv manifest.Argv, env map[string]string) (int, string, error) {
	s, ok := e.m.Services[service]
	if !ok {
		return 0, "", fmt.Errorf("engine: unknown service %q", service)
	}
	if err := e.ensureImage(ctx, s); err != nil {
		return 0, "", err
	}

	// A copy with no ports: a throwaway must never contend for the published
	// port the real service owns.
	tmp := *s
	tmp.Port, tmp.Ports = 0, nil
	tmp.Env = mergeEnv(s.Env, nil)

	// The bay network has to exist even when no service is running, because
	// the container is attached to it at creation.
	if err := e.ensureNetwork(ctx); err != nil {
		return 0, "", err
	}

	id, err := e.create(ctx, service+"-task", &tmp, argv)
	if err != nil {
		return 0, "", err
	}
	defer e.remove(context.WithoutCancel(ctx), id)

	if len(env) > 0 {
		// Task env is applied by recreating with the merged set rather than
		// mutating a running container, which Docker does not allow.
		_ = e.remove(context.WithoutCancel(ctx), id)
		tmp.Env = mergeEnv(s.Env, env)
		if id, err = e.create(ctx, service+"-task", &tmp, argv); err != nil {
			return 0, "", err
		}
		defer e.remove(context.WithoutCancel(ctx), id)
	}

	if _, err := e.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return 0, "", err
	}
	code, err := e.wait(ctx, id)
	if err != nil {
		return 0, "", err
	}
	out, _ := e.logsOf(context.WithoutCancel(ctx), id, 5000)
	return code, out, nil
}

// scrubText removes secret values before anything leaves the engine.
func (e *Engine) scrubText(s string) string {
	if e.scrubber == nil {
		return s
	}
	return e.scrubber.String(s)
}

func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func mergeEnv(base, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// tail keeps the end of a long output, which is where a failure is.
func tail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[len(s)-max:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i < 200 {
		cut = cut[i+1:]
	}
	return "...[truncated]\n" + cut
}
