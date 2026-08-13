package engine

import "testing"

// The path an agent opens. A container path here is the difference between an
// agent that edits the failing line and one that reports a file that does not
// exist.
func TestFailurePathsAreRepoRelative(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"/workspace/api/server.test.js", "api/server.test.js"},
		{"/workspace/suite.py", "suite.py"},
		{"api/server.test.js", "api/server.test.js"},
		{"", ""},
		{"/usr/lib/node_modules/thing.js", "/usr/lib/node_modules/thing.js"},
		{"at Test (/workspace/api/x.js:12:10)", "at Test (api/x.js:12:10)"},
	} {
		if got := repoRelative(c.in); got != c.want {
			t.Errorf("repoRelative(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
