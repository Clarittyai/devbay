package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Clarittyai/devbay/internal/bay"
	"github.com/Clarittyai/devbay/internal/introspect"
	"github.com/Clarittyai/devbay/internal/patch"
	"github.com/Clarittyai/devbay/internal/verify"
)

// cmdInit proposes a devbay.yaml for the current repository.
//
// It never overwrites without being asked, and it never writes a file that
// would fail validation. Both matter more than convenience: this file is about
// to be executed with whatever credentials the developer supplies, so the one
// unacceptable outcome is a manifest that looks authoritative and is wrong.
// verifyProposal boots a candidate in a throwaway bay, repairing it with the
// given patcher when one is configured.
func verifyProposal(ctx context.Context, dir string, data []byte, patcher verify.Patcher) (*verify.Result, error) {
	m, err := bay.Open(ctx, bay.Options{
		Dir:     dir,
		NoProxy: true, // hostnames are irrelevant to whether it boots
		Egress:  egressEnabled(),
		Log:     func(f string, a ...any) { fmt.Fprintf(os.Stderr, dim("  "+f)+"\n", a...) },
	})
	if err != nil {
		return nil, err
	}
	defer m.Close()
	return m.VerifyManifest(ctx, data, patcher)
}

// modelPatcher returns the repair loop's patcher, or nil when the developer
// has not asked for one.
//
// Off by default and announced when on. `devbay init` reads a repository and
// would otherwise send it to a third party without saying so, which is not a
// thing a local-first tool gets to do quietly.
func modelPatcher() verify.Patcher {
	c, err := patch.FromEnv(patch.WithLog(func(f string, a ...any) {
		fmt.Fprintf(os.Stderr, dim(f)+"\n", a...)
	}))
	if err != nil {
		return nil
	}
	fmt.Fprintf(os.Stderr, "%s repairs will be proposed by %s\n", dim("model"), bold(c.Model))
	return c
}

func indent(s, prefix string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString(prefix + line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func cmdInit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	force := fs.Bool("force", false, "overwrite an existing devbay.yaml")
	stdout := fs.Bool("stdout", false, "print the proposal instead of writing it")
	check := fs.Bool("verify", false, "boot the proposal in a throwaway bay before writing it")
	fs.Parse(permute(args))

	dir := mustCwd()
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	out := filepath.Join(dir, "devbay.yaml")

	if !*stdout && !*force {
		if _, err := os.Stat(out); err == nil {
			return fmt.Errorf("%s already exists; pass --force to replace it, or --stdout to see the proposal", out)
		}
	}

	res, err := introspect.Detect(ctx, dir)
	if err != nil {
		return err
	}
	data, err := introspect.Render(res)
	if err != nil {
		return err
	}

	// Round-trip through the validator. Writing a file devbay itself would
	// reject is the one failure a generator must not have.
	report, err := introspect.Verify(data)
	if err != nil {
		return fmt.Errorf("the generated manifest does not parse, which is a devbay bug: %w", err)
	}

	// Verification is what makes a generated file trustworthy: it is the
	// difference between "this parses" and "this works".
	if *check {
		verified, err := verifyProposal(ctx, dir, data, modelPatcher())
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s could not verify: %v\n", yellow("skipped"), err)
		} else if verified.OK {
			fmt.Fprintf(os.Stderr, "%s booted successfully in %d attempt(s)\n",
				green("verified"), len(verified.Attempts))
			data = verified.Manifest
		} else {
			fmt.Fprintf(os.Stderr, "%s the proposal did not boot\n", red("unverified"))
			if f := verified.LastFailure(); f != nil {
				fmt.Fprintf(os.Stderr, "  %s\n", f.Error())
				if f.Logs != "" {
					fmt.Fprintf(os.Stderr, "%s\n", dim(indent(f.Logs, "    ")))
				}
			}
			if verified.Note != "" {
				fmt.Fprintf(os.Stderr, "  %s\n", dim(verified.Note))
			}
		}
	}

	if *stdout {
		os.Stdout.Write(data)
	}

	for _, e := range res.Evidence {
		fmt.Fprintf(os.Stderr, "  %s %-14s %s\n", dim("from"), e.Source, dim(e.Detail))
	}

	if report != nil && !report.OK() {
		fmt.Fprintf(os.Stderr, "\n%s the proposal is incomplete:\n", yellow("incomplete"))
		for _, d := range report.Errors() {
			fmt.Fprintf(os.Stderr, "  %s %s: %s\n", red("needs work"), bold(d.Location()), d.Msg)
		}
		for _, g := range res.Gaps {
			fmt.Fprintf(os.Stderr, "  %s %s\n", yellow("decide"), g)
		}
		if *stdout {
			return nil
		}
		// Written anyway: a partial manifest with its gaps listed in the file
		// is a far better starting point than a blank page, and refusing to
		// write would leave the developer with nothing.
		if err := os.WriteFile(out, data, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "\nwrote %s — finish the items above, then run `devbay validate .`\n", bold(out))
		return nil
	}

	for _, g := range res.Gaps {
		fmt.Fprintf(os.Stderr, "  %s %s\n", yellow("check"), g)
	}
	if *stdout {
		return nil
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return err
	}
	fmt.Printf("%s %s (%d services, %d tasks)\n", green("wrote"), bold(out),
		len(res.Manifest.Services), len(res.Manifest.Tasks))
	fmt.Println(dim("  review it, then: devbay new <name>"))
	return nil
}
