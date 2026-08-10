// Package introspect works out how a repository runs.
//
// It is deliberately deterministic: no model is involved. Rule-based detection
// cannot reach every repository, but it reaches a useful majority, and what it
// produces can be explained line by line -- every service and task it emits
// carries the file it was read from. A generated manifest a developer cannot
// audit is worse than no manifest, because they will run it anyway.
//
// The evidence is consulted in order of how much it actually knows:
//
//  1. An existing compose file or devcontainer. This is not detection at all,
//     it is transcription, and it is the highest-signal input by a wide margin.
//  2. GitHub Actions `services:` blocks. Near-literal service definitions,
//     with health commands already spelled out in `options:`, kept current
//     because CI breaks when they are wrong.
//  3. Procfiles, package manifests and framework conventions, which are good
//     for the start command and poor for the port.
//
// What it will not do is invent an egress allowlist. That is not an oversight:
// if configuration produced from repository content could widen the network
// policy, then repository content could widen the network policy, and the
// sandbox would be arguing with itself.
package introspect

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	interp "github.com/compose-spec/compose-go/v2/interpolation"
	"github.com/compose-spec/compose-go/v2/loader"
	composetypes "github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"

	"github.com/Clarittyai/devbay/internal/manifest"
)

// Source names where a conclusion came from.
type Source string

const (
	SourceCompose      Source = "compose"
	SourceDevcontainer Source = "devcontainer"
	SourceActions      Source = "github-actions"
	SourceProcfile     Source = "procfile"
	SourcePackageJSON  Source = "package.json"
	SourcePython       Source = "python"
	SourceGo           Source = "go"
	SourceRuby         Source = "ruby"
	SourceConvention   Source = "convention"
)

// Evidence is one thing the detector concluded, and why.
type Evidence struct {
	Source Source `json:"source"`
	Path   string `json:"path,omitempty"`
	Detail string `json:"detail"`
}

// Result is a detected manifest plus an account of how it was reached.
type Result struct {
	Manifest *manifest.Manifest `json:"-"`
	Evidence []Evidence         `json:"evidence"`
	// Gaps are the things a human still has to decide. Naming them is the
	// difference between a manifest that fails cleanly and one that fails
	// mysteriously.
	Gaps []string `json:"gaps"`
}

// Detect examines a repository and proposes a manifest.
func Detect(ctx context.Context, dir string) (*Result, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	d := &detector{
		dir: abs,
		m: &manifest.Manifest{
			Version:  1,
			Project:  slug(filepath.Base(abs)),
			Services: map[string]*manifest.Service{},
			Tasks:    map[string]*manifest.Task{},
		},
		res: &Result{},
	}

	d.fromCompose(ctx)
	d.fromActions()
	d.fromProcfile()
	d.fromPackageJSON()
	d.fromPython()
	d.fromGo()

	d.inferHealth()
	d.choosePrimary()
	d.checkGaps()

	d.res.Manifest = d.m
	return d.res, nil
}

type detector struct {
	dir string
	m   *manifest.Manifest
	res *Result
}

func (d *detector) note(src Source, path, detail string) {
	rel := path
	if r, err := filepath.Rel(d.dir, path); err == nil && !strings.HasPrefix(r, "..") {
		rel = r
	}
	d.res.Evidence = append(d.res.Evidence, Evidence{Source: src, Path: rel, Detail: detail})
}

func (d *detector) gap(format string, args ...any) {
	d.res.Gaps = append(d.res.Gaps, fmt.Sprintf(format, args...))
}

// ---------------------------------------------------------------------------
// compose
// ---------------------------------------------------------------------------

var composeNames = []string{
	"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml",
	filepath.Join(".devcontainer", "docker-compose.yml"),
	filepath.Join(".devcontainer", "compose.yml"),
	filepath.Join("docker", "development", "compose.yml"),
}

// fromCompose transcribes an existing compose file.
//
// Interpolation is enabled but forgiving. A compose file routinely references
// variables from a .env nobody committed, and skipping interpolation entirely
// does not help: a port written as "${DB_PORT:-5432}:5432" cannot be parsed as
// a port until the variable is substituted, so the short form -- which is how
// almost every compose file is written -- would be rejected.
//
// So unknown variables resolve to empty and are recorded as gaps, which also
// lets compose's own `:-default` syntax do its job. A detector that refused
// the file over an unset variable would have discarded the best evidence in
// the repository to protect a value it does not need.
func (d *detector) fromCompose(ctx context.Context) {
	for _, name := range composeNames {
		path := filepath.Join(d.dir, name)
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		env := d.composeEnv()
		var unset []string
		project, err := loader.LoadWithContext(ctx, composetypes.ConfigDetails{
			WorkingDir:  d.dir,
			ConfigFiles: []composetypes.ConfigFile{{Filename: path, Content: body}},
			Environment: env,
		}, func(o *loader.Options) {
			o.SkipValidation = true
			o.SkipConsistencyCheck = true
			o.SkipResolveEnvironment = true
			// Normalization expands compose's short forms -- ports written as
			// "5432:5432" rather than as a mapping -- so it must stay on.
			o.SkipNormalization = false
			o.ResolvePaths = false
			o.SetProjectName(d.m.Project, true)
			o.Interpolate = &interp.Options{
				LookupValue: func(key string) (string, bool) {
					if v, ok := env[key]; ok {
						return v, true
					}
					unset = append(unset, key)
					return "", false
				},
			}
		})
		if err != nil {
			d.note(SourceCompose, path, "could not be parsed: "+err.Error())
			continue
		}
		if len(unset) > 0 {
			d.gap("%s references variables that are not set (%s); check anything they fed into",
				name, strings.Join(dedupe(unset), ", "))
		}

		for _, svc := range project.Services {
			name := slug(svc.Name)
			if name == "" || d.m.Services[name] != nil {
				continue
			}
			image := svc.Image
			if image != "" && !usableImage(image) {
				// An unset variable ate the tag. A bare name would silently
				// resolve to :latest, which is not what the file said.
				d.gap("service %q has an unresolvable image %q; set a concrete one", name, image)
				continue
			}
			s := &manifest.Service{Image: image, Env: map[string]string{}}
			if s.Image == "" {
				// A build stanza needs a Dockerfile path devbay can reach, and
				// resolving that correctly is worth more than guessing.
				d.gap("service %q in %s builds from source; set `image:` or a `build:` block", name, name)
				continue
			}

			for k, v := range svc.Environment {
				if v != nil && safeEnvValue(*v) {
					s.Env[k] = *v
				}
			}
			ports := composePorts(svc)
			if len(ports) > 0 {
				s.Port = ports[0]
				if len(ports) > 1 {
					s.Ports = map[string]int{}
					for i, p := range ports[1:] {
						s.Ports[fmt.Sprintf("port%d", i+2)] = p
					}
					d.gap("service %q exposes %d ports; name them meaningfully in `ports:`", name, len(ports))
				}
			}
			for _, dep := range sortedKeysOf(svc.DependsOn) {
				s.Needs = append(s.Needs, slug(dep))
			}
			d.m.Services[name] = s
			d.note(SourceCompose, path, fmt.Sprintf("service %q from image %s", name, s.Image))
		}
		if len(project.Services) > 0 {
			return // the first compose file found wins
		}
	}
}

// composeEnv is the environment interpolation draws on: the real environment,
// plus a committed .env if there is one.
func (d *detector) composeEnv() composetypes.Mapping {
	env := composetypes.Mapping{}
	if body, err := os.ReadFile(filepath.Join(d.dir, ".env")); err == nil {
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if k, v, ok := strings.Cut(line, "="); ok {
				env[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
	}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			if _, taken := env[k]; !taken {
				env[k] = v
			}
		}
	}
	return env
}

// usableImage rejects references devbay could never pull: workflow
// expressions, shell substitutions, and leftovers from an unset variable.
func usableImage(image string) bool {
	if image == "" || strings.Contains(image, "${{") || strings.Contains(image, "$(") ||
		strings.Contains(image, "${") {
		return false
	}
	return !strings.HasSuffix(image, ":") && !strings.HasPrefix(image, ":")
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// composePorts extracts published container ports in declaration order.
func composePorts(svc composetypes.ServiceConfig) []int {
	var out []int
	seen := map[int]bool{}
	for _, p := range svc.Ports {
		// Target is the container port, which is what a manifest declares.
		// Published is the host side and is devbay's to allocate.
		n := int(p.Target)
		if n == 0 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) == 0 {
		for _, e := range svc.Expose {
			if n, err := strconv.Atoi(strings.SplitN(e, "/", 2)[0]); err == nil && n > 0 && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// GitHub Actions
// ---------------------------------------------------------------------------

type workflow struct {
	Jobs map[string]struct {
		Services map[string]struct {
			Image   string            `yaml:"image"`
			Env     map[string]string `yaml:"env"`
			Ports   []string          `yaml:"ports"`
			Options string            `yaml:"options"`
		} `yaml:"services"`
		Steps []struct {
			Name string `yaml:"name"`
			Run  string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// fromActions reads backing services and test commands out of CI.
//
// A workflow's `services:` block is a near-literal service definition, and its
// `options:` field usually carries the health command already. It is also the
// one description of a repository that cannot silently rot, because CI fails
// when it is wrong.
func (d *detector) fromActions() {
	dir := filepath.Join(d.dir, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, e := range entries {
		if e.IsDir() || !isYAML(e.Name()) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var wf workflow
		if err := yaml.Unmarshal(body, &wf); err != nil {
			continue
		}

		for _, job := range sortedKeysOf(wf.Jobs) {
			for _, svcName := range sortedKeysOf(wf.Jobs[job].Services) {
				svc := wf.Jobs[job].Services[svcName]
				name := slug(svcName)
				if name == "" || d.m.Services[name] != nil || svc.Image == "" {
					continue
				}
				// A workflow expression is resolved by the matrix at run time,
				// so there is no single image to record. Emitting the literal
				// would produce a manifest that cannot pull anything.
				if !usableImage(svc.Image) {
					d.gap("service %q in CI uses the expression %q as its image; pick a concrete one",
						name, svc.Image)
					continue
				}
				s := &manifest.Service{Image: svc.Image, Env: map[string]string{}}
				for k, v := range svc.Env {
					if safeEnvValue(v) {
						s.Env[k] = v
					}
				}
				if p := firstPort(svc.Ports); p != 0 {
					s.Port = p
				}
				// `options: --health-cmd pg_isready` is a probe the repository
				// already relies on, which beats anything inferred.
				if cmd := healthCmdFrom(svc.Options); len(cmd) > 0 {
					s.Health = &manifest.Health{Cmd: cmd}
				}
				d.m.Services[name] = s
				d.note(SourceActions, path, fmt.Sprintf("service %q from image %s", name, svc.Image))
			}
		}
	}
}

// healthCmdFrom pulls the argv out of a `--health-cmd` option.
func healthCmdFrom(options string) manifest.Argv {
	i := strings.Index(options, "--health-cmd")
	if i < 0 {
		return nil
	}
	rest := strings.TrimSpace(options[i+len("--health-cmd"):])
	rest = strings.TrimPrefix(rest, "=")
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return nil
	}
	// The value may be quoted, and runs to the next option or line.
	if rest[0] == '"' || rest[0] == '\'' {
		q := rest[0]
		if end := strings.IndexByte(rest[1:], q); end >= 0 {
			rest = rest[1 : end+1]
		}
	} else {
		if end := strings.IndexAny(rest, "\n"); end >= 0 {
			rest = rest[:end]
		}
		if end := strings.Index(rest, " --"); end >= 0 {
			rest = rest[:end]
		}
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return nil
	}
	return manifest.Argv(fields)
}

// ---------------------------------------------------------------------------
// application services
// ---------------------------------------------------------------------------

// fromProcfile reads process types, which is the cleanest description of how
// an application starts that anyone has shipped.
func (d *detector) fromProcfile() {
	for _, name := range []string{"Procfile.dev", "Procfile"} {
		path := filepath.Join(d.dir, name)
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(body), "\n") {
			proc, cmd, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok || strings.HasPrefix(proc, "#") {
				continue
			}
			key := slug(proc)
			if key == "" || d.m.Services[key] != nil {
				continue
			}
			argv, env := splitEnvPrefix(strings.Fields(cmd))
			if len(argv) == 0 {
				continue
			}
			s := &manifest.Service{Start: manifest.Argv(argv), Env: env}
			if p, ok := env["PORT"]; ok {
				if n, err := strconv.Atoi(p); err == nil {
					s.Port = n
				}
			}
			d.m.Services[key] = s
			d.note(SourceProcfile, path, fmt.Sprintf("process %q runs %s", key, strings.Join(argv, " ")))
			d.gap("service %q from %s has no image; set one that provides its toolchain", key, name)
		}
		return
	}
}

type packageJSON struct {
	Name            string            `json:"name"`
	PackageManager  string            `json:"packageManager"`
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

func (d *detector) fromPackageJSON() {
	path := filepath.Join(d.dir, "package.json")
	body, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var pkg packageJSON
	if err := json.Unmarshal(body, &pkg); err != nil {
		d.note(SourcePackageJSON, path, "could not be parsed: "+err.Error())
		return
	}

	pm := packageManager(d.dir, pkg.PackageManager)
	deps := map[string]bool{}
	for k := range pkg.Dependencies {
		deps[k] = true
	}
	for k := range pkg.DevDependencies {
		deps[k] = true
	}

	// The framework says more about the port than any script name does.
	//
	// The order is fixed and deliberate, for two reasons. Ranging over a map
	// would make the inferred port differ between runs, which is intolerable
	// in generated configuration. And a bundler is usually a transitive
	// dependency of the framework built on it, so Vite must be consulted last
	// or a React Router app is mistaken for a bare Vite app.
	port, framework := 0, ""
	for _, f := range []struct {
		dep  string
		port int
	}{
		{"next", 3000},
		{"nuxt", 3000},
		{"@remix-run/dev", 3000},
		{"react-router", 3000},
		{"@nestjs/core", 3000},
		{"astro", 4321},
		{"react-scripts", 3000},
		{"@sveltejs/kit", 5173},
		{"vite", 5173},
	} {
		if deps[f.dep] {
			port, framework = f.port, f.dep
			break
		}
	}

	if _, has := pkg.Scripts["dev"]; has && d.m.Services["web"] == nil {
		s := &manifest.Service{
			Start:   manifest.Argv{pm, "run", "dev"},
			Port:    port,
			Install: manifest.Argv{pm, installVerb(pm)},
			Volumes: []string{"node_modules"},
		}
		d.m.Services["web"] = s
		detail := fmt.Sprintf("`%s run dev`", pm)
		if framework != "" {
			detail += fmt.Sprintf("; port %d inferred from %s", port, framework)
		}
		d.note(SourcePackageJSON, path, detail)
		d.gap("service \"web\" has no image; set one providing Node")
		if port == 0 {
			d.gap("service \"web\" has no port; devbay cannot tell which port the dev server listens on")
		}
	}

	// Test and lint scripts become tasks. A task with needs: [] boots nothing,
	// which is the right default for a unit suite; anything else is a decision
	// only the author can make.
	for _, name := range []string{"test", "test:unit", "lint", "typecheck"} {
		if _, ok := pkg.Scripts[name]; !ok {
			continue
		}
		task := slug(strings.ReplaceAll(name, ":", "-"))
		if d.m.Tasks[task] != nil {
			continue
		}
		d.m.Tasks[task] = &manifest.Task{Run: manifest.Argv{pm, "run", name}, Needs: []string{}}
		d.note(SourcePackageJSON, path, fmt.Sprintf("task %q from script %q", task, name))
	}
}

func (d *detector) fromPython() {
	if _, err := os.Stat(filepath.Join(d.dir, "manage.py")); err == nil {
		if d.m.Services["web"] == nil {
			d.m.Services["web"] = &manifest.Service{
				Start: manifest.Argv{"python", "manage.py", "runserver", "0.0.0.0:8000"},
				Port:  8000,
			}
			d.note(SourcePython, filepath.Join(d.dir, "manage.py"), "Django: runserver on 8000")
			d.gap("service \"web\" has no image; set one providing Python")
		}
		if d.m.Tasks["migrate"] == nil {
			d.m.Services["migrate"] = &manifest.Service{
				Kind: manifest.KindOneshot,
				Run:  manifest.Argv{"python", "manage.py", "migrate", "--no-input"},
			}
			d.note(SourcePython, filepath.Join(d.dir, "manage.py"), "Django: migrations as a oneshot")
		}
	}
	for _, f := range []string{"pyproject.toml", "requirements.txt"} {
		if _, err := os.Stat(filepath.Join(d.dir, f)); err == nil {
			if d.m.Tasks["unit"] == nil {
				d.m.Tasks["unit"] = &manifest.Task{
					Run:    manifest.Argv{"pytest", "--junit-xml=reports/unit.xml"},
					Needs:  []string{},
					Report: &manifest.Report{Format: manifest.ReportJUnit, Path: "reports/unit.xml"},
				}
				d.note(SourcePython, filepath.Join(d.dir, f), "task \"unit\" runs pytest with a JUnit report")
			}
			return
		}
	}
}

func (d *detector) fromGo() {
	if _, err := os.Stat(filepath.Join(d.dir, "go.mod")); err != nil {
		return
	}
	if d.m.Tasks["unit"] == nil {
		d.m.Tasks["unit"] = &manifest.Task{
			Run:    manifest.Argv{"go", "test", "-json", "./..."},
			Needs:  []string{},
			Report: &manifest.Report{Format: manifest.ReportGoJSON},
		}
		d.note(SourceGo, filepath.Join(d.dir, "go.mod"), "task \"unit\" runs `go test -json`, parsed as a stream")
	}
	d.gap("Go services need an explicit `start:` command; devbay cannot tell which cmd/ package is the server")
}

// ---------------------------------------------------------------------------
// finishing
// ---------------------------------------------------------------------------

// knownHealth maps an image family to the probe its maintainers document.
//
// A short hardcoded table beats inference here: these are the images that
// appear in a services block, and their health commands are stable and
// well known.
var knownHealth = []struct {
	prefix string
	health manifest.Health
}{
	{"postgres", manifest.Health{Cmd: manifest.Argv{"pg_isready"}}},
	{"pgvector", manifest.Health{Cmd: manifest.Argv{"pg_isready"}}},
	{"mysql", manifest.Health{Cmd: manifest.Argv{"mysqladmin", "ping"}}},
	{"mariadb", manifest.Health{Cmd: manifest.Argv{"mysqladmin", "ping"}}},
	{"redis", manifest.Health{Cmd: manifest.Argv{"redis-cli", "ping"}}},
	{"valkey", manifest.Health{Cmd: manifest.Argv{"redis-cli", "ping"}}},
	{"mongo", manifest.Health{Cmd: manifest.Argv{"mongosh", "--eval", "db.runCommand('ping')"}}},
	{"rabbitmq", manifest.Health{Cmd: manifest.Argv{"rabbitmq-diagnostics", "ping"}}},
	{"elasticsearch", manifest.Health{HTTP: "/_cluster/health"}},
	{"opensearch", manifest.Health{HTTP: "/_cluster/health"}},
	{"minio", manifest.Health{HTTP: "/minio/health/live"}},
	{"mailpit", manifest.Health{HTTP: "/readyz"}},
	{"mailhog", manifest.Health{HTTP: "/"}},
}

// inferHealth fills in probes, and records where it could not.
func (d *detector) inferHealth() {
	for _, name := range sortedKeysOf(d.m.Services) {
		s := d.m.Services[name]
		if s.IsOneshot() || s.Health != nil {
			continue
		}
		base := imageBase(s.Image)
		for _, k := range knownHealth {
			if strings.HasPrefix(base, k.prefix) {
				h := k.health
				s.Health = &h
				d.note(SourceConvention, "", fmt.Sprintf("health probe for %q from the %s image family", name, k.prefix))
				break
			}
		}
		if s.Health != nil {
			continue
		}
		if s.Port != 0 {
			// An HTTP probe on the declared port is the best guess for an
			// application, and the path is the part most likely to be wrong.
			s.Health = &manifest.Health{HTTP: "/", Timeout: "60s"}
			d.gap("service %q was given a placeholder probe `GET /`; point it at a real health endpoint", name)
			continue
		}
		// Better an honest weak probe than a fabricated strong one.
		s.Health = &manifest.Health{Process: true}
		d.gap("service %q has no port, so it got a liveness-only probe; a `log:` pattern would be stronger", name)
	}
}

func (d *detector) choosePrimary() {
	var ported []string
	for _, name := range sortedKeysOf(d.m.Services) {
		if s := d.m.Services[name]; !s.IsOneshot() && s.Port != 0 {
			ported = append(ported, name)
		}
	}
	if len(ported) <= 1 {
		return // inferred, or nothing to choose between
	}
	// Prefer the obvious application names over a datastore.
	for _, want := range []string{"web", "app", "frontend", "api", "server"} {
		for _, name := range ported {
			if name == want {
				d.m.Services[name].Primary = true
				d.note(SourceConvention, "", fmt.Sprintf("%q claims the bay hostname", name))
				return
			}
		}
	}
	d.m.Services[ported[0]].Primary = true
	d.gap("several services expose a port; %q was made primary, which may be wrong", ported[0])
}

// startable reports whether an image is expected to run without an explicit
// command. A datastore's entrypoint is its server; an application base image's
// is a shell.
func startable(image string) bool {
	base := imageBase(image)
	for _, k := range knownHealth {
		if strings.HasPrefix(base, k.prefix) {
			return true
		}
	}
	return false
}

func (d *detector) checkGaps() {
	if len(d.m.Services) == 0 {
		d.gap("no services were detected; devbay.yaml needs at least one")
	}
	if len(d.m.Tasks) == 0 {
		d.gap("no tasks were detected; add at least a unit test task so agents can verify their work")
	}
	for _, name := range sortedKeysOf(d.m.Services) {
		s := d.m.Services[name]
		if s.Image == "" {
			d.gap("service %q has no image", name)
		}
		// A stock datastore image starts its own server. Anything else that
		// was transcribed without a command will run its default entrypoint,
		// and the first symptom is a health probe that never passes.
		if !s.IsOneshot() && len(s.Start) == 0 && s.Image != "" && !startable(s.Image) {
			d.gap("service %q has no `start:` command; its image will run its default entrypoint", name)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// splitEnvPrefix pulls leading `VAR=value` words, and a leading `env`, out of a
// command. `env PORT=3000 bundle exec puma` is shell syntax, and the variables
// belong in an env map where they can be read.
func splitEnvPrefix(fields []string) ([]string, map[string]string) {
	env := map[string]string{}
	i := 0
	for i < len(fields) {
		f := fields[i]
		if f == "env" && i == 0 {
			i++
			continue
		}
		k, v, ok := strings.Cut(f, "=")
		if !ok || k == "" || strings.ContainsAny(k, "/.-") {
			break
		}
		env[k] = v
		i++
	}
	return fields[i:], env
}

// safeEnvValue rejects values that would fail manifest validation, so the
// detector never writes a file it knows will be refused.
func safeEnvValue(v string) bool {
	return !strings.Contains(v, "${") && !strings.Contains(v, "$(")
}

func packageManager(dir, declared string) string {
	if declared != "" {
		if name, _, _ := strings.Cut(declared, "@"); name != "" {
			return name
		}
	}
	for file, pm := range map[string]string{
		"pnpm-lock.yaml":    "pnpm",
		"yarn.lock":         "yarn",
		"bun.lockb":         "bun",
		"bun.lock":          "bun",
		"package-lock.json": "npm",
	} {
		if _, err := os.Stat(filepath.Join(dir, file)); err == nil {
			return pm
		}
	}
	return "npm"
}

func installVerb(pm string) string {
	if pm == "npm" {
		return "ci"
	}
	return "install"
}

func imageBase(image string) string {
	if i := strings.IndexAny(image, ":@"); i >= 0 {
		image = image[:i]
	}
	if i := strings.LastIndex(image, "/"); i >= 0 {
		image = image[i+1:]
	}
	return image
}

func firstPort(ports []string) int {
	for _, p := range ports {
		part := p
		if i := strings.LastIndex(p, ":"); i >= 0 {
			part = p[i+1:]
		}
		part = strings.SplitN(part, "/", 2)[0]
		if n, err := strconv.Atoi(part); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")
}

// slug makes a DNS-safe label, since names become hostnames.
func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.' || r == ' ':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-")
	}
	return out
}

func sortedKeysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
