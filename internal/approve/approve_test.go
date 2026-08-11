package approve

import (
	"context"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestGrantIsRemembered(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	argv := []string{"bin/dev"}

	if s.Granted(ctx, "app", argv) {
		t.Fatal("a command was approved before anyone approved it")
	}
	if err := s.Grant(ctx, Record{Project: "app", At: "services/api/start", Argv: argv}); err != nil {
		t.Fatal(err)
	}
	if !s.Granted(ctx, "app", argv) {
		t.Error("an approval did not survive being written")
	}
}

// The whole value of keying by the argv array rather than by argv[0].
func TestApprovingACommandDoesNotApproveItsArguments(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.Grant(ctx, Record{Project: "app", Argv: []string{"bin/dev"}}); err != nil {
		t.Fatal(err)
	}
	if s.Granted(ctx, "app", []string{"bin/dev", "--seed-production"}) {
		t.Error("approving bin/dev also approved arguments nobody read")
	}
}

func TestApprovalDoesNotCrossProjects(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.Grant(ctx, Record{Project: "app", Argv: []string{"bin/dev"}}); err != nil {
		t.Fatal(err)
	}
	if s.Granted(ctx, "other", []string{"bin/dev"}) {
		t.Error("approving one repository's script approved another repository's file of the same name")
	}
}

// A separator inside an argument must not be able to imitate a different argv.
func TestArgumentsCannotBeSmuggledAcrossTheSeparator(t *testing.T) {
	a := Key("app", []string{"bin/dev", "b"})
	b := Key("app", []string{"bin/dev b"})
	if a == b {
		t.Error("two different commands share an approval key")
	}
}

func TestRevoke(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	argv := []string{"./scripts/setup.sh"}
	if err := s.Grant(ctx, Record{Project: "app", Argv: argv}); err != nil {
		t.Fatal(err)
	}
	found, err := s.Revoke(ctx, Key("app", argv))
	if err != nil || !found {
		t.Fatalf("revoke: %v found=%v", err, found)
	}
	if s.Granted(ctx, "app", argv) {
		t.Error("a revoked command is still approved")
	}
	if found, _ := s.Revoke(ctx, "nosuchkey"); found {
		t.Error("revoking something that was never approved reported success")
	}
}

func TestListIsScopedAndReadable(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for _, r := range []Record{
		{Project: "app", At: "services/api/start", Argv: []string{"bin/dev"}},
		{Project: "app", At: "tasks/unit/run", Argv: []string{"./t.sh"}},
		{Project: "other", Argv: []string{"bin/dev"}},
	} {
		if err := s.Grant(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.List(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected this project's two approvals, got %d", len(got))
	}
	for _, r := range got {
		if len(r.Argv) == 0 || r.By == "" || r.When.IsZero() {
			t.Errorf("an approval came back without enough to audit it: %+v", r)
		}
	}
}
