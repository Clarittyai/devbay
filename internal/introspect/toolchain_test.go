package introspect

import "testing"

// The version in these files is written a dozen ways and every one of them
// means the same thing for choosing a base image.
func TestFirstVersion(t *testing.T) {
	for in, want := range map[string]string{
		">=18":          "18",
		"^20.11.0":      "20.11",
		"~> 3.2":        "3.2",
		"v18.17.1":      "18.17",
		"python-3.12.1": "3.12",
		"20":            "20",
		"lts/*":         "",
		"":              "",
		"node":          "",
	} {
		if got := firstVersion(in); got != want {
			t.Errorf("firstVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// Asking for more precision than the registry publishes produces a tag that
// does not exist, and the failure arrives as a pull error with no hint about
// where the number came from.
func TestTrimVersionMatchesTheRegistry(t *testing.T) {
	cases := []struct {
		in    string
		parts int
		want  string
	}{
		{"20.11", 1, "20"},    // node tags a major line
		{"3.12.1", 2, "3.12"}, // python tags major.minor
		{"1.23", 2, "1.23"},
		{"8", 2, "8"},
	}
	for _, c := range cases {
		if got := trimVersion(c.in, c.parts); got != c.want {
			t.Errorf("trimVersion(%q, %d) = %q, want %q", c.in, c.parts, got, c.want)
		}
	}
}
