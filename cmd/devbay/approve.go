package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Clarittyai/devbay/internal/manifest"
)

// cmdApprove is the human half of R2.
//
// Deliberately its own command rather than a prompt inside `devbay new`. A
// decision made mid-boot, with a bay half-created and a developer waiting, is
// made by pressing y -- and an approval granted that way is not an approval,
// it is an obstacle cleared. Separating it means the argv is read at a moment
// when reading it is the only thing happening.
func cmdApprove(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	list := fs.Bool("list", false, "show what has already been approved")
	revoke := fs.String("revoke", "", "withdraw an approval by key")
	yes := fs.Bool("yes", false, "approve without prompting; for a human who has already read the commands")
	fs.Parse(permute(args))

	m, err := open(ctx, false)
	if err != nil {
		return err
	}
	defer m.Close()

	path := "devbay.yaml"
	if fs.NArg() > 0 {
		path = fs.Arg(0)
	}
	mf, err := manifest.Load(path)
	if err != nil {
		return err
	}

	if *revoke != "" {
		found, err := m.RevokeApproval(ctx, *revoke)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("no approval with key %s; run devbay approve --list", *revoke)
		}
		fmt.Printf("%s revoked %s\n", green("ok"), *revoke)
		return nil
	}

	if *list {
		granted, err := m.Approvals(ctx, mf.Project)
		if err != nil {
			return err
		}
		if len(granted) == 0 {
			fmt.Printf("nothing approved for %s\n", mf.Project)
			return nil
		}
		for _, r := range granted {
			fmt.Printf("%s  %s\n  %s\n  %s by %s\n\n", bold(r.Key), dim(r.At),
				strings.Join(r.Argv, " "), r.When.Format("2006-01-02 15:04"), r.By)
		}
		return nil
	}

	res := manifest.Validate(mf)
	if !res.OK() {
		return fmt.Errorf("%s does not validate, so there is nothing to approve yet: %w", path, res.Err())
	}
	pending := m.PendingApprovals(ctx, mf, res)
	if len(pending) == 0 {
		fmt.Printf("%s every command in %s is either on the allowlist or already approved\n", green("ok"), path)
		return nil
	}

	// An agent must not be able to approve on the developer's behalf. That is
	// the entire content of the rule: the boundary is a human reading the
	// command, and a boundary the caller can cross by itself is not one.
	if !*yes && !onATerminal() {
		return errNoHuman(len(pending))
	}

	in := bufio.NewReader(os.Stdin)
	var granted int
	for _, d := range pending {
		fmt.Printf("\n%s\n  %s\n", bold(d.Location()), strings.Join(d.Argv, " "))
		fmt.Printf("  %s\n", dim("this runs inside the bay, with the bay's environment and secrets"))
		if !*yes {
			fmt.Print("approve? [y/N] ")
			line, err := in.ReadString('\n')
			if errors.Is(err, io.EOF) && strings.TrimSpace(line) == "" {
				// Nobody is answering. Stdin can be a character device and
				// still have no human behind it -- /dev/null is the obvious
				// case -- so the reliable test is whether the question got an
				// answer, not what kind of file the question was asked down.
				fmt.Println()
				return errNoHuman(len(pending))
			}
			if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
				fmt.Printf("  %s\n", dim("skipped"))
				continue
			}
		}
		if err := m.GrantApproval(ctx, mf.Project, d); err != nil {
			return err
		}
		granted++
	}

	fmt.Printf("\n%s approved %d of %d\n", green("ok"), granted, len(pending))
	if granted < len(pending) {
		fmt.Println(dim("a bay will not boot until every command is approved"))
	}
	return nil
}

// errNoHuman is what a caller that cannot answer gets. Phrased as an
// instruction to the person, because the caller is usually an agent and the
// only useful thing it can do is say this to them.
func errNoHuman(n int) error {
	return fmt.Errorf("%d command(s) need a human's approval, and nothing is answering.\n"+
		"Run `devbay approve` in a terminal, or pass --yes once you have read them", n)
}

// onATerminal reports whether a human is answering. A pipe, a heredoc or an
// agent's captured stdin all read as not-a-terminal, which is the distinction
// that matters here.
func onATerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
