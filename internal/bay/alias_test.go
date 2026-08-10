package bay

import "testing"

// Aliases exist because agents generate branch names nobody can read. The
// label has to survive a browser tab strip, where past about eight tabs only
// a few characters are visible.
func TestDeriveAlias(t *testing.T) {
	for _, c := range []struct{ branch, want string }{
		{"add-oauth", "add-oauth"},
		{"feat/add-oauth", "add-oauth"},
		{"fix/login", "login"},
		{"chore/bump-deps", "bump-deps"},

		// The case this exists for.
		{"feat/refactor-auth-middleware-to-support-refresh-token-rotation", "refactor"},
		{"feature/implement-the-new-billing-system", "implement"},

		// Cut at a word boundary rather than mid-word, so the label reads as a
		// word instead of a truncation.
		{"add-oauth-provider-support", "add-oauth"},

		{"Feature/Add_OAuth", "add-oauth"},
		{"release/v2.1.0", "v210"},
		{"---weird---", "weird"},
		{"a", "a"},
	} {
		if got := DeriveAlias(c.branch); got != c.want {
			t.Errorf("DeriveAlias(%q) = %q, want %q", c.branch, got, c.want)
		}
	}
}

func TestDerivedAliasesAreAlwaysUsable(t *testing.T) {
	for _, branch := range []string{
		"feat/a-very-long-branch-name-that-goes-on-and-on-forever",
		"UPPER/CASE",
		"weird!!chars@@here",
		"trailing-",
		"-leading",
	} {
		got := DeriveAlias(branch)
		if len(got) > MaxAlias {
			t.Errorf("DeriveAlias(%q) = %q, %d chars; max is %d", branch, got, len(got), MaxAlias)
		}
		for _, r := range got {
			ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
			if !ok {
				t.Errorf("DeriveAlias(%q) = %q contains %q, which is not safe in a hostname", branch, got, r)
			}
		}
		if len(got) > 0 && (got[0] == '-' || got[len(got)-1] == '-') {
			t.Errorf("DeriveAlias(%q) = %q has a leading or trailing hyphen", branch, got)
		}
	}
}
