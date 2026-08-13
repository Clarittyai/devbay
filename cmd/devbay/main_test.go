package main

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/Clarittyai/devbay/internal/bay"
	"github.com/Clarittyai/devbay/internal/manifest"
)

// Go's flag package stops parsing at the first positional argument, so
// `devbay new add-oauth --alias oauth` silently dropped the alias -- and that
// is how almost everyone types it. A flag that is quietly ignored is worse
// than one that errors, because the command appears to have worked.
func TestPermuteMovesFlagsAhead(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []string
		want []string
	}{
		{"trailing value flag",
			[]string{"add-oauth", "--alias", "oauth"},
			[]string{"--alias", "oauth", "add-oauth"}},

		{"already in order",
			[]string{"--alias", "oauth", "add-oauth"},
			[]string{"--alias", "oauth", "add-oauth"}},

		{"equals form needs no lookahead",
			[]string{"add-oauth", "--alias=oauth"},
			[]string{"--alias=oauth", "add-oauth"}},

		{"boolean flag does not swallow the next argument",
			[]string{"add-oauth", "--no-boot"},
			[]string{"--no-boot", "add-oauth"}},

		{"boolean flag before a positional",
			[]string{"--no-boot", "add-oauth"},
			[]string{"--no-boot", "add-oauth"}},

		{"several flags and positionals",
			[]string{"mybay", "--alias", "a", "--branch", "feat/x", "--no-boot"},
			[]string{"--alias", "a", "--branch", "feat/x", "--no-boot", "mybay"}},

		{"two positionals keep their order",
			[]string{"baynme", "unit"},
			[]string{"baynme", "unit"}},

		{"short flag with a value",
			[]string{"mybay", "web", "-n", "50"},
			[]string{"-n", "50", "mybay", "web"}},

		{"double dash stops processing",
			[]string{"mybay", "--", "--not-a-flag"},
			[]string{"mybay", "--not-a-flag"}},

		{"empty", nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := permute(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("permute(%v)\n  got  %v\n  want %v", c.in, got, c.want)
			}
		})
	}
}

// A positional argument that happens to look like a negative number must not
// be eaten as a flag.
func TestPermuteLeavesBareDashAlone(t *testing.T) {
	got := permute([]string{"mybay", "-"})
	want := []string{"mybay", "-"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("permute = %v, want %v", got, want)
	}
}

// `devbay ls` showed whichever URL sorted first alphabetically, which for a
// typical stack is the cache rather than the application.
func TestPrimaryURLPrefersThePrimaryService(t *testing.T) {
	m, err := manifest.Parse([]byte(`
version: 1
project: demo
services:
  cache:
    image: redis:7
    port: 6379
    health: {tcp: 6379}
  web:
    image: node:22
    primary: true
    port: 3000
    health: {http: /}
tasks:
  unit: {run: ["true"], needs: []}
`))
	if err != nil {
		t.Fatal(err)
	}

	info := bay.Info{URLs: map[string]string{
		"cache": "http://cache.b.demo.localhost",
		"web":   "http://b.demo.localhost",
	}}
	if got := primaryURL(info, m); got != "http://b.demo.localhost" {
		t.Errorf("primaryURL = %q, want the primary service's URL", got)
	}

	// With no manifest to consult it falls back to a stable choice rather
	// than a random map iteration.
	first := primaryURL(info, nil)
	for i := 0; i < 20; i++ {
		if primaryURL(info, nil) != first {
			t.Fatal("the fallback URL is not stable between calls")
		}
	}
}

func TestPrimaryURLHandlesEmpty(t *testing.T) {
	if got := primaryURL(bay.Info{}, nil); got != "" {
		t.Errorf("primaryURL on an empty info = %q, want empty", got)
	}
}

func TestTruncate(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"short", 24, "short"},
		{"exactly-ten", 11, "exactly-ten"},
		{"feat/a-very-long-branch-name-here", 10, "feat/a-ve…"},
	} {
		if got := truncate(c.in, c.n); got != c.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
		if len([]rune(truncate(c.in, c.n))) > c.n {
			t.Errorf("truncate(%q, %d) is longer than %d", c.in, c.n, c.n)
		}
	}
}

// Colour must be suppressible, because this output is read by pipes, files and
// agents as often as by terminals.
func TestNoColorIsRespected(t *testing.T) {
	saved := colour
	defer func() { colour = saved }()

	colour = false
	for _, s := range []string{red("x"), green("x"), yellow("x"), bold("x"), dim("x"), stateColour("warm")} {
		if strings.Contains(s, "\x1b") {
			t.Errorf("escape sequence emitted with colour disabled: %q", s)
		}
	}

	colour = true
	if !strings.Contains(red("x"), "\x1b") {
		t.Error("colour enabled but no escape emitted")
	}
}

func TestStateColourPassesTheStateThrough(t *testing.T) {
	saved := colour
	defer func() { colour = saved }()
	colour = false

	for _, s := range []string{"hot", "warm", "frozen", "cold", "mixed", "unknown"} {
		if got := stateColour(s); got != s {
			t.Errorf("stateColour(%q) = %q; the state name must survive", s, got)
		}
	}
}

// Every value-taking flag must be known to permute.
//
// permute moves flags ahead of positional arguments, so a flag whose value it
// does not know about has that value treated as positional and moved away from
// its flag. `mcp install --client codex --dry-run` became `--client --dry-run
// codex`. The source is read rather than a list being kept by hand, because a
// hand-kept list is exactly what was already wrong here.
func TestEveryValueTakingFlagIsKnownToPermute(t *testing.T) {
	defined := regexp.MustCompile(`fs\.(?:String|Int|Duration)\("([a-z-]+)"`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var checked int
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		body, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range defined.FindAllStringSubmatch(string(body), -1) {
			checked++
			if !takesValue(m[1]) {
				t.Errorf("%s defines --%s, which takes a value, but takesValue does not know it: "+
					"its value will be reordered away from the flag", e.Name(), m[1])
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no flag definitions; the pattern no longer matches the source")
	}
}

// The shape that was broken, end to end through permute.
func TestPermuteKeepsAFlagWithItsValue(t *testing.T) {
	got := permute([]string{"--client", "codex", "--dry-run"})
	want := []string{"--client", "codex", "--dry-run"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("permute = %v, want %v", got, want)
	}
	// The case permute exists for still works.
	got = permute([]string{"mybay", "--alias", "oauth", "--no-boot"})
	want = []string{"--alias", "oauth", "--no-boot", "mybay"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("permute = %v, want %v", got, want)
	}
}
