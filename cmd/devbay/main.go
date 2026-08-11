// Command devbay creates and drives isolated local environments.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Clarittyai/devbay/internal/bay"
	"github.com/Clarittyai/devbay/internal/engine"
	"github.com/Clarittyai/devbay/internal/manifest"
	"github.com/Clarittyai/devbay/internal/mcp"
	"github.com/Clarittyai/devbay/spec"
)

const usage = `devbay — parallel, isolated local environments for coding agents

  devbay new <name> [--alias a] [--branch b] [--from ref] [--no-boot]
  devbay ls                                list bays
  devbay status <bay>                      state, services, URLs
  devbay run <bay> <task>                  run a declared task
  devbay logs <bay> [service] [-n 100]     read scrubbed logs
  devbay url <bay> [service]               addresses for a service
  devbay focus <bay>                       move the canonical hostname
  devbay watch <bay>                       apply host edits inside the containers
  devbay freeze|thaw|cool <bay>            change resident state
  devbay rm <bay> [--force]                destroy a bay

  devbay init [--verify] [--force]         generate a devbay.yaml for this repo
  devbay validate [path ...]               check a manifest against R1-R7
  devbay schema                            print the JSON Schema
  devbay doctor                            diagnose the environment
  devbay version                           build and toolchain versions

  DEVBAY_EGRESS=1  enforce each service's declared egress allowlist
  devbay mcp                               serve the agent interface on stdio
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "init":
		err = cmdInit(ctx, args)
	case "validate":
		os.Exit(validate(args))
	case "schema":
		os.Stdout.Write(spec.Schema)
	case "new":
		err = cmdNew(ctx, args)
	case "ls", "list":
		err = cmdList(ctx, args)
	case "status":
		err = cmdStatus(ctx, args)
	case "run":
		err = cmdRun(ctx, args)
	case "logs":
		err = cmdLogs(ctx, args)
	case "url":
		err = cmdURL(ctx, args)
	case "focus":
		err = cmdFocus(ctx, args)
	case "watch":
		err = cmdWatch(ctx, args)
	case "freeze", "thaw", "cool":
		err = cmdState(ctx, cmd, args)
	case "rm", "destroy":
		err = cmdRemove(ctx, args)
	case "doctor":
		err = cmdDoctor(ctx)
	case "mcp":
		err = cmdMCP(ctx, args)
	case "version", "-v", "--version":
		cmdVersion()
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "devbay: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %v\n", red("error"), err)
		os.Exit(1)
	}
}

// egressEnabled reports whether per-service network allowlists are enforced.
//
// Opt-in for now: enforcement costs a privileged sidecar per service, and
// turning it on by default would change the behaviour of every existing
// manifest at once -- a service whose `egress:` is missing would lose its
// network without anyone having asked for that.
func egressEnabled() bool {
	v := os.Getenv("DEVBAY_EGRESS")
	return v == "1" || v == "true" || v == "on"
}

// open connects to Docker and restores known bays.
func open(ctx context.Context, verbose bool) (*bay.Manager, error) {
	logf := func(string, ...any) {}
	if verbose {
		logf = func(f string, a ...any) { fmt.Fprintf(os.Stderr, dim("  "+f)+"\n", a...) }
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return bay.Open(ctx, bay.Options{Dir: cwd, Log: logf, Egress: egressEnabled()})
}

func cmdNew(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("new", flag.ExitOnError)
	alias := fs.String("alias", "", "short human label (max 12 chars); derived from the branch when omitted")
	branch := fs.String("branch", "", "branch to check out; defaults to the bay name")
	from := fs.String("from", "", "starting point for a new branch; defaults to HEAD")
	noBoot := fs.Bool("no-boot", false, "prepare the worktree without starting services")
	fs.Parse(permute(args))

	if fs.NArg() < 1 {
		return fmt.Errorf("usage: devbay new <name> [--alias a] [--branch b]")
	}
	name := fs.Arg(0)

	m, err := open(ctx, true)
	if err != nil {
		return err
	}
	defer m.Close()

	start := time.Now()
	b, err := m.Create(ctx, bay.CreateOptions{
		Name: name, Alias: *alias, Branch: *branch, From: *from, Boot: !*noBoot,
	})
	if err != nil {
		return err
	}

	info, err := m.Describe(ctx, b)
	if err != nil {
		return err
	}
	fmt.Printf("%s %s (%s) on %s in %s\n",
		green("created"), bold(b.Name), b.Alias, b.Branch, time.Since(start).Round(time.Millisecond))
	printURLs(info)
	return nil
}

func cmdList(ctx context.Context, _ []string) error {
	m, err := open(ctx, false)
	if err != nil {
		return err
	}
	defer m.Close()

	list, err := m.List(ctx)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("no bays; create one with `devbay new <name>`")
		return nil
	}

	fmt.Printf("%-14s %-12s %-8s %-24s %s\n", "BAY", "ALIAS", "STATE", "BRANCH", "URL")
	for _, b := range list {
		var mf *manifest.Manifest
		if inst, ok := m.Get(b.Name); ok {
			mf = inst.Manifest
		}
		url := primaryURL(b, mf)
		marker := ""
		if b.Focused {
			marker = " *"
		}
		fmt.Printf("%-14s %-12s %-8s %-24s %s%s\n",
			b.Name, b.Alias, stateColour(b.State), truncate(b.Branch, 24), url, marker)
	}
	return nil
}

func cmdStatus(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: devbay status <bay>")
	}
	m, err := open(ctx, false)
	if err != nil {
		return err
	}
	defer m.Close()

	b, ok := m.Get(args[0])
	if !ok {
		return fmt.Errorf("no bay named %q", args[0])
	}
	info, err := m.Describe(ctx, b)
	if err != nil {
		return err
	}

	fmt.Printf("%s  %s  %s  %s\n", bold(info.Name), info.Alias, stateColour(info.State), info.Branch)
	fmt.Printf("worktree %s\n", dim(info.Worktree))
	if info.MemoryMB > 0 {
		fmt.Printf("memory   %d MiB\n", info.MemoryMB)
	}
	if len(info.Services) > 0 {
		fmt.Println("services")
		for _, s := range info.Services {
			fmt.Printf("  %-14s %-10s %s\n", s.Name, s.State, dim(s.URL))
		}
	}
	printURLs(info)
	return nil
}

func cmdRun(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: devbay run <bay> <task>")
	}
	m, err := open(ctx, true)
	if err != nil {
		return err
	}
	defer m.Close()

	res, err := m.RunTask(ctx, args[0], args[1])
	if err != nil {
		return err
	}

	if res.Succeeded() {
		fmt.Printf("%s %s in %dms", green("pass"), bold(res.Task), res.DurationMS)
	} else {
		fmt.Printf("%s %s in %dms", red("fail"), bold(res.Task), res.DurationMS)
	}
	if res.Parsed {
		fmt.Printf("  (%d passed, %d failed, %d skipped)", res.Passed, res.Failed, res.Skipped)
	}
	fmt.Println()

	for _, f := range res.Failures {
		loc := f.File
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		fmt.Printf("\n  %s\n", bold(f.Name))
		if loc != "" {
			fmt.Printf("  %s\n", dim(loc))
		}
		fmt.Printf("  %s\n", f.Message)
	}

	// Without structured results the raw output is all an agent or a human
	// has, so it is shown rather than swallowed.
	if !res.Succeeded() && !res.Parsed && res.Output != "" {
		fmt.Printf("\n%s\n%s\n", dim("no parseable report; raw output follows"), res.Output)
	}
	if !res.Succeeded() {
		os.Exit(1)
	}
	return nil
}

func cmdLogs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	n := fs.Int("n", 100, "number of trailing lines")
	fs.Parse(permute(args))
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: devbay logs <bay> [service] [-n 100]")
	}

	m, err := open(ctx, false)
	if err != nil {
		return err
	}
	defer m.Close()

	b, ok := m.Get(fs.Arg(0))
	if !ok {
		return fmt.Errorf("no bay named %q", fs.Arg(0))
	}
	service := fs.Arg(1)
	if service == "" {
		service = b.Manifest.PrimaryService()
	}
	out, err := b.Engine.Logs(ctx, service, *n)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

func cmdURL(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: devbay url <bay> [service]")
	}
	m, err := open(ctx, false)
	if err != nil {
		return err
	}
	defer m.Close()

	b, ok := m.Get(args[0])
	if !ok {
		return fmt.Errorf("no bay named %q", args[0])
	}
	service := b.Manifest.PrimaryService()
	if len(args) > 1 {
		service = args[1]
	}
	res := b.Engine.Resolver()
	if ep, err := res.Endpoint(service, engine.PlaneHost); err == nil {
		fmt.Printf("%-12s http://%s\n", "url", ep.Addr())
	}
	fmt.Printf("%-12s %s://%s\n", "public_url", res.Scheme, res.Hostname(service))
	return nil
}

func cmdFocus(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: devbay focus <bay>")
	}
	m, err := open(ctx, false)
	if err != nil {
		return err
	}
	defer m.Close()

	if err := m.Focus(ctx, args[0]); err != nil {
		return err
	}
	b, _ := m.Get(args[0])
	fmt.Printf("%s %s now holds %s\n", green("focus"), bold(args[0]),
		b.Manifest.Project+".localhost")
	return nil
}

func cmdState(ctx context.Context, which string, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: devbay %s <bay>", which)
	}
	m, err := open(ctx, false)
	if err != nil {
		return err
	}
	defer m.Close()

	b, ok := m.Get(args[0])
	if !ok {
		return fmt.Errorf("no bay named %q", args[0])
	}

	start := time.Now()
	switch which {
	case "freeze":
		err = b.Engine.Freeze(ctx)
	case "thaw":
		// State-aware: a developer typing "thaw" wants the bay working, and
		// should not have to know whether it was paused or stopped.
		err = b.Engine.Resume(ctx)
	case "cool":
		err = b.Engine.Cool(ctx)
	}
	if err != nil {
		return err
	}

	state, _ := b.Engine.State(ctx)
	fmt.Printf("%s is %s (%s)\n", bold(args[0]), stateColour(string(state)),
		time.Since(start).Round(time.Millisecond))
	if which == "freeze" {
		// Saying this plainly is the difference between a user who reaches for
		// the right control and one who freezes five bays and wonders why the
		// machine is still swapping.
		fmt.Println(dim("  note: freezing stops CPU, not memory. Use `devbay cool` to reclaim memory."))
	}
	return nil
}

func cmdRemove(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rm", flag.ExitOnError)
	force := fs.Bool("force", false, "discard uncommitted changes in the worktree")
	fs.Parse(permute(args))
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: devbay rm <bay> [--force]")
	}

	m, err := open(ctx, true)
	if err != nil {
		return err
	}
	defer m.Close()

	if err := m.Destroy(ctx, fs.Arg(0), *force); err != nil {
		return err
	}
	fmt.Printf("%s %s\n", green("removed"), bold(fs.Arg(0)))
	return nil
}

func cmdMCP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	socket := fs.String("socket", "", "also listen on this unix socket")
	fs.Parse(permute(args))

	// Progress goes to stderr: stdout is the protocol channel and anything
	// else written there corrupts the stream.
	m, err := bay.Open(ctx, bay.Options{
		Dir: mustCwd(),
		Log: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
	})
	if err != nil {
		return err
	}
	defer m.Close()

	srv := mcp.NewServer(m)
	if *socket != "" {
		go func() { _ = srv.ListenUnix(ctx, *socket) }()
	}
	return srv.Serve(ctx, stdio{in: os.Stdin, out: os.Stdout})
}

// permute moves flags ahead of positional arguments.
//
// Go's flag package stops parsing at the first non-flag argument, so
// `devbay new add-oauth --alias oauth` silently drops the alias -- and that is
// the order almost everyone types. A flag that is quietly ignored is worse
// than one that errors, because the command appears to succeed.
func permute(args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return append(append(flags, positional...), args[i+1:]...)
		case strings.HasPrefix(a, "-") && len(a) > 1:
			flags = append(flags, a)
			// A flag written as two words takes the next argument with it,
			// unless it is boolean. Only the value-taking flags devbay
			// defines are listed, so `--no-boot next` is not misread.
			if !strings.Contains(a, "=") && i+1 < len(args) && takesValue(a) {
				i++
				flags = append(flags, args[i])
			}
		default:
			positional = append(positional, a)
		}
	}
	return append(flags, positional...)
}

func takesValue(flag string) bool {
	switch strings.TrimLeft(flag, "-") {
	case "alias", "branch", "from", "n", "socket":
		return true
	}
	return false
}

// primaryURL picks the URL a human most likely wants: the primary service's.
func primaryURL(info bay.Info, m *manifest.Manifest) string {
	if m != nil {
		if p := m.PrimaryService(); p != "" {
			if u, ok := info.URLs[p]; ok {
				return u
			}
		}
	}
	keys := make([]string, 0, len(info.URLs))
	for k := range info.URLs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	return info.URLs[keys[0]]
}

func printURLs(info bay.Info) {
	if len(info.URLs) == 0 {
		return
	}
	keys := make([]string, 0, len(info.URLs))
	for k := range info.URLs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-16s %s\n", k, info.URLs[k])
	}
}

func validate(paths []string) int {
	if len(paths) == 0 {
		paths = []string{"devbay.yaml"}
	}
	var failed, approvals int
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			p = p + "/devbay.yaml"
		}
		m, err := manifest.Load(p)
		if err != nil {
			fmt.Printf("%s\n  %s %v\n", p, red("error"), err)
			// Only when a *command* failed to decode. The hint used to fire on
			// any string-into-struct mismatch, so `build: ./web` was answered
			// with a lecture about argv arrays -- pointing at a rule that has
			// nothing to do with the field, which is worse than no hint.
			if strings.Contains(err.Error(), "manifest.Argv") {
				fmt.Printf("         %s\n", dim(`R1: commands are argv arrays, not shell strings — write [pnpm, dev], not "pnpm dev"`))
			}
			failed++
			continue
		}

		r := manifest.Validate(m)
		fmt.Println(p)
		for _, d := range r.Errors() {
			fmt.Printf("  %s %s: %s\n", red("error"), bold(d.Location()), d.Msg)
		}
		for _, d := range r.Approvals() {
			approvals++
			fmt.Printf("  %s %s: %s\n", yellow("approve"), bold(d.Location()), d.Msg)
			fmt.Printf("          %s\n", dim(strings.Join(d.Argv, " ")))
		}
		for _, d := range r.Warnings() {
			fmt.Printf("  %s %s: %s\n", dim("warn"), dim(d.Location()), dim(d.Msg))
		}
		if !r.OK() {
			failed++
			fmt.Printf("  %s %d error(s)\n", red("FAIL"), len(r.Errors()))
			continue
		}
		fmt.Printf("  %s %d service(s), %d task(s), primary %s\n",
			green("ok"), len(m.Services), len(m.Tasks), m.PrimaryService())
	}
	if approvals > 0 && failed == 0 {
		fmt.Printf("\n%d command(s) need one-time approval before they run.\n", approvals)
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func mustCwd() string {
	d, err := os.Getwd()
	if err != nil {
		return "."
	}
	return d
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func stateColour(s string) string {
	switch s {
	case "hot":
		return green(s)
	case "warm":
		return green(s)
	case "frozen":
		return yellow(s)
	case "cold":
		return dim(s)
	case "mixed":
		return red(s)
	}
	return s
}

var colour = os.Getenv("NO_COLOR") == "" && isTTY()

func isTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func wrap(code, s string) string {
	if !colour {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func red(s string) string    { return wrap("31", s) }
func green(s string) string  { return wrap("32", s) }
func yellow(s string) string { return wrap("33", s) }
func bold(s string) string   { return wrap("1", s) }
func dim(s string) string    { return wrap("2", s) }
