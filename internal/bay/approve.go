package bay

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Clarittyai/devbay/internal/approve"
	"github.com/Clarittyai/devbay/internal/manifest"
	"github.com/Clarittyai/devbay/internal/verify"
)

// RequireApprovals refuses to boot a bay whose manifest runs a command no
// human has agreed to.
//
// The rule is only a boundary if it blocks. Printing "this needs approval" and
// then running the command teaches a developer that the warning is noise --
// and the next warning, the one that matters, is scrolled past at the same
// speed. So an unapproved argv stops the bay before any container starts.
//
// The error names the exact command and the exact way to allow it, because a
// refusal a developer cannot act on is indistinguishable from a bug, and the
// thing they will do about it is stop using the tool.
func (m *Manager) RequireApprovals(ctx context.Context, mf *manifest.Manifest, res *manifest.Result) error {
	pending := m.PendingApprovals(ctx, mf, res)
	if len(pending) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "bay: %d command(s) in %s have not been approved:\n", len(pending), mf.Path)
	for _, d := range pending {
		fmt.Fprintf(&b, "\n  %s\n    %s\n", d.Location(), strings.Join(d.Argv, " "))
	}
	b.WriteString("\nRead them -- they are scripts in this repository -- then run:\n\n  devbay approve\n")
	return errAwaitingApproval{msg: b.String()}
}

// approvalGate refuses a candidate manifest carrying unapproved commands.
//
// Returned as a verify.Failure so it travels the airlock's own error path, and
// with its own stage so the repair loop can tell it apart from a boot failure.
// A patcher handed this would otherwise do the worst possible thing: rewrite
// the command until something passes.
func (m *Manager) approvalGate(ctx context.Context, mf *manifest.Manifest) *verify.Failure {
	pending := m.PendingApprovals(ctx, mf, manifest.Validate(mf))
	if len(pending) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("this configuration runs commands outside the allowlist, which a human has to approve before anything executes:\n")
	for _, d := range pending {
		fmt.Fprintf(&b, "\n  %s\n    %s\n", d.Location(), strings.Join(d.Argv, " "))
	}
	b.WriteString("\nRead them, then run: devbay approve")
	return &verify.Failure{Stage: verify.StageApproval, Message: b.String()}
}

// PendingApprovals is the subset of a validation's approvals not yet granted.
func (m *Manager) PendingApprovals(ctx context.Context, mf *manifest.Manifest, res *manifest.Result) []manifest.Diagnostic {
	if res == nil {
		return nil
	}
	var out []manifest.Diagnostic
	for _, d := range res.Approvals() {
		if len(d.Argv) == 0 || m.appr.Granted(ctx, mf.Project, d.Argv) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// GrantApproval records a human's decision.
func (m *Manager) GrantApproval(ctx context.Context, project string, d manifest.Diagnostic) error {
	return m.appr.Grant(ctx, approve.Record{Project: project, At: d.Path, Argv: d.Argv})
}

// Approvals returns what has been granted for a project.
func (m *Manager) Approvals(ctx context.Context, project string) ([]approve.Record, error) {
	return m.appr.List(ctx, project)
}

// RevokeApproval withdraws one by key.
func (m *Manager) RevokeApproval(ctx context.Context, key string) (bool, error) {
	return m.appr.Revoke(ctx, key)
}

// errAwaitingApproval is a distinct type so callers -- notably MCP -- can tell
// "a human has to look at this" apart from "the manifest is wrong". An agent
// that cannot distinguish them will try to fix a manifest that is already
// correct.
type errAwaitingApproval struct{ msg string }

func (e errAwaitingApproval) Error() string { return e.msg }

// AwaitingApproval reports whether an error is a pending human decision.
func AwaitingApproval(err error) bool {
	var e errAwaitingApproval
	return errors.As(err, &e)
}
