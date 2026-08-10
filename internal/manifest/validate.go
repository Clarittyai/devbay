package manifest

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/Clarittyai/devbay/spec"
)

// Severity distinguishes a manifest that must be rejected from one that is
// merely questionable.
type Severity int

const (
	// Error means the manifest does not reach the execution plane.
	Error Severity = iota
	// Warn means it does, but a human should see the message. Two things
	// produce warnings rather than errors: an argv[0] outside the allowlist,
	// which is permitted subject to approval, and a liveness-only health
	// probe, which is weak but honest.
	Warn
	// Approval means execution is blocked until a human approves the exact
	// argv. This is the R2 escape hatch: rejecting outright would make people
	// fork the project, and permitting a shell string would make R1 theatre.
	Approval
)

func (s Severity) String() string {
	switch s {
	case Error:
		return "error"
	case Warn:
		return "warn"
	case Approval:
		return "approval"
	}
	return "?"
}

// Diagnostic is one finding, located in the manifest.
type Diagnostic struct {
	Severity Severity
	Rule     string // "R1".."R7", or "" for structural findings
	Path     string // e.g. services/api/env/DATABASE_URL
	Msg      string
	Argv     Argv // populated for Approval findings, shown verbatim to the human
}

func (d Diagnostic) String() string {
	rule := ""
	if d.Rule != "" {
		rule = d.Rule + " "
	}
	return fmt.Sprintf("%s: %s%s: %s", d.Severity, rule, d.Path, d.Msg)
}

// Result is the outcome of validating a manifest.
type Result struct {
	Diagnostics []Diagnostic
}

// OK reports whether the manifest may cross into the execution plane.
// Approvals do not make a manifest invalid; they gate execution of one command.
func (r *Result) OK() bool {
	for _, d := range r.Diagnostics {
		if d.Severity == Error {
			return false
		}
	}
	return true
}

// Errors returns only the findings that reject the manifest.
func (r *Result) Errors() []Diagnostic { return r.filter(Error) }

// Approvals returns the commands needing one-time human approval.
func (r *Result) Approvals() []Diagnostic { return r.filter(Approval) }

// Warnings returns the advisory findings.
func (r *Result) Warnings() []Diagnostic { return r.filter(Warn) }

func (r *Result) filter(s Severity) []Diagnostic {
	var out []Diagnostic
	for _, d := range r.Diagnostics {
		if d.Severity == s {
			out = append(out, d)
		}
	}
	return out
}

// Err returns a single error summarising the rejections, or nil.
func (r *Result) Err() error {
	errs := r.Errors()
	if len(errs) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d validation error(s):", len(errs))
	for _, d := range errs {
		fmt.Fprintf(&b, "\n  %s", d)
	}
	return fmt.Errorf("%s", b.String())
}

// refPattern extracts bay references from an interpolated value. The grammar
// itself is enforced by the schema's pattern; this only locates the pieces.
var refPattern = regexp.MustCompile(
	`\$\{bay\.([a-z0-9-]+)\.(url|public_url|host|port|name|user|password|ports\.[a-z0-9-]+)\}`)

// Validate applies rules R1-R7 and the semantic checks a schema cannot express.
//
// This function is the airlock. A manifest that does not pass it must never be
// interpreted by anything holding credentials.
func Validate(m *Manifest) *Result {
	r := &Result{}
	pat, err := spec.Load()
	if err != nil {
		r.add(Error, "", "<schema>", err.Error())
		return r
	}

	if m.Version != 1 {
		r.add(Error, "", "version", fmt.Sprintf("unsupported version %d; only 1 exists", m.Version))
	}
	if !pat.Slug.MatchString(m.Project) {
		r.add(Error, "", "project", fmt.Sprintf("%q is not a DNS label; it becomes part of every bay hostname", m.Project))
	}
	if len(m.Services) == 0 {
		r.add(Error, "", "services", "a manifest with no services cannot boot anything")
	}

	for _, name := range sortedKeys(m.Services) {
		validateService(r, m, pat, name, m.Services[name])
	}
	for _, name := range sortedKeys(m.Tasks) {
		validateTask(r, m, pat, name, m.Tasks[name])
	}

	validateGraph(r, m)
	validatePrimary(r, m)
	return r
}

func validateService(r *Result, m *Manifest, pat spec.Rules, name string, s *Service) {
	at := "services/" + name
	if s == nil {
		r.add(Error, "", at, "service is empty")
		return
	}
	if !pat.Slug.MatchString(name) {
		r.add(Error, "", at, "service name is not a DNS label; it becomes a hostname")
	}

	// Exactly one image source.
	switch {
	case s.Image == "" && s.Build == nil:
		r.add(Error, "", at, "needs either image or build")
	case s.Image != "" && s.Build != nil:
		r.add(Error, "", at, "has both image and build; they are mutually exclusive")
	case s.Image != "" && !strings.Contains(s.Image, "@sha256:"):
		r.add(Warn, "", at+"/image",
			fmt.Sprintf("%q is a floating tag; pin by digest so a bay booted today and one booted next month are the same bay", s.Image))
	}

	// Kind-specific shape. A oneshot's exit code is its probe, so it is exempt
	// from R5 — and must not pretend to serve traffic.
	if s.IsOneshot() {
		if len(s.Run) == 0 {
			r.add(Error, "", at, "kind: oneshot needs a run command")
		}
		if len(s.Start) > 0 {
			r.add(Error, "", at, "kind: oneshot uses run, not start")
		}
		if s.Health != nil {
			r.add(Error, "R5", at, "kind: oneshot must not declare health; its exit code is the probe")
		}
		if s.Port != 0 || len(s.Ports) > 0 {
			r.add(Error, "", at, "kind: oneshot runs to completion and cannot serve a port")
		}
	} else {
		if len(s.Run) > 0 {
			r.add(Error, "", at, "a long-running service uses start, not run")
		}
		validateHealth(r, pat, at, s)
	}

	validateArgv(r, pat, at+"/install", s.Install)
	validateArgv(r, pat, at+"/start", s.Start)
	validateArgv(r, pat, at+"/run", s.Run)

	if s.InstallScripts {
		r.add(Warn, "", at+"/install_scripts",
			"package lifecycle scripts are enabled; running install scripts on untrusted code is the Shai-Hulud delivery mechanism, so this needs approval")
	}

	// R4. An absent egress list is not an oversight — it means no outbound
	// network — so only malformed entries are reported.
	for i, host := range s.Egress {
		if host == "" || strings.ContainsAny(host, "/ :") {
			r.add(Error, "R4", fmt.Sprintf("%s/egress/%d", at, i),
				fmt.Sprintf("%q is not a hostname; egress is matched on hostname, not URL", host))
		}
	}

	for _, pn := range sortedKeys(s.Ports) {
		if !pat.Slug.MatchString(pn) {
			r.add(Error, "", at+"/ports/"+pn, "named port is not a DNS label; it becomes a hostname")
		}
		if s.Ports[pn] == s.Port {
			r.add(Error, "", at+"/ports/"+pn, "duplicates the primary port")
		}
	}

	if s.Seed != nil {
		validateSeed(r, m, at, name, s)
	}

	validateEnv(r, m, pat, at, s.Env)
	validateDurations(r, pat, at, s)
}

func validateHealth(r *Result, pat spec.Rules, at string, s *Service) {
	h := s.Health
	if h == nil {
		// R5. This is the rule most tempting to relax and the one that must
		// not be: with no probe there is no verification loop, and without a
		// verification loop generated configuration is only a guess that
		// happened to parse.
		r.add(Error, "R5", at, "no health probe; use http, tcp, cmd, log, or process")
		return
	}
	var forms []string
	if h.HTTP != "" {
		forms = append(forms, "http")
	}
	if h.TCP != 0 {
		forms = append(forms, "tcp")
	}
	if len(h.Cmd) > 0 {
		forms = append(forms, "cmd")
	}
	if h.Log != "" {
		forms = append(forms, "log")
	}
	if h.Process {
		forms = append(forms, "process")
	}
	switch len(forms) {
	case 1: // good
	case 0:
		r.add(Error, "R5", at+"/health", "no probe form set; use http, tcp, cmd, log, or process")
	default:
		r.add(Error, "R5", at+"/health",
			fmt.Sprintf("%d probe forms set (%s); exactly one must be, or it is ambiguous which one determines readiness",
				len(forms), strings.Join(forms, ", ")))
	}

	if h.HTTP != "" {
		if !strings.HasPrefix(h.HTTP, "/") {
			r.add(Error, "R5", at+"/health/http", "must be a path beginning with /")
		}
		if s.Port == 0 {
			r.add(Error, "R5", at+"/health/http", "an http probe needs a port to probe")
		}
	}
	if h.Log != "" {
		if _, err := regexp.Compile(h.Log); err != nil {
			r.add(Error, "R5", at+"/health/log", fmt.Sprintf("not a valid RE2 regex: %v", err))
		}
	}
	if h.Process {
		r.add(Warn, "R5", at+"/health",
			"liveness-only probe: this reports healthy for a process that is running but not working; prefer log if the process prints anything deterministic on startup")
	}
	validateArgv(r, pat, at+"/health/cmd", h.Cmd)
}

func validateSeed(r *Result, m *Manifest, at, name string, s *Service) {
	if s.Fork != ForkImage {
		r.add(Error, "", at+"/seed",
			fmt.Sprintf("seed applies to fork: image, not %q", s.Fork))
	}
	if len(s.Seed.Sources) == 0 {
		r.add(Error, "", at+"/seed/sources",
			"no sources, so devbay cannot tell when the seed image is stale and would serve last month's schema forever")
	}
	for _, step := range s.Seed.After {
		dep, ok := m.Services[step]
		if !ok {
			r.add(Error, "", at+"/seed/after", fmt.Sprintf("unknown service %q", step))
			continue
		}
		if !dep.IsOneshot() {
			r.add(Error, "", at+"/seed/after",
				fmt.Sprintf("%q must be kind: oneshot; seeding completes, it does not run forever", step))
		}
		if !dependsOn(m, step, name) {
			r.add(Warn, "", at+"/seed/after",
				fmt.Sprintf("%q does not depend on %q, so it may run before the database is ready", step, name))
		}
	}
}

func validateTask(r *Result, m *Manifest, pat spec.Rules, name string, t *Task) {
	at := "tasks/" + name
	if t == nil {
		r.add(Error, "", at, "task is empty")
		return
	}
	if len(t.Run) == 0 {
		r.add(Error, "", at+"/run", "no command")
	}
	validateArgv(r, pat, at+"/run", t.Run)

	// R6. An empty list is valid and common; an omitted key is not. The
	// distinction is the whole rule: a task that boots zero containers is the
	// fastest path to a verified result, and that only happens if the author
	// was made to think about the subgraph.
	if t.Needs == nil {
		r.add(Error, "R6", at,
			"needs is required, even when empty; declaring the service subgraph is what lets devbay boot only what the task uses")
	}
	for _, dep := range t.Needs {
		if _, ok := m.Services[dep]; !ok {
			r.add(Error, "R6", at+"/needs", fmt.Sprintf("unknown service %q", dep))
		}
	}
	if t.In != "" {
		if _, ok := m.Services[t.In]; !ok {
			r.add(Error, "", at+"/in", fmt.Sprintf("unknown service %q", t.In))
		}
	}
	if t.Report != nil {
		switch t.Report.Format {
		case ReportJUnit, ReportJSON, ReportTAP:
			if t.Report.Path == "" {
				r.add(Error, "", at+"/report",
					fmt.Sprintf("format %q writes a file, so path is required", t.Report.Format))
			}
		case ReportGoJSON:
			// Streams to stdout; a path would be meaningless.
		case "":
			r.add(Error, "", at+"/report/format", "missing")
		default:
			r.add(Error, "", at+"/report/format", fmt.Sprintf("unknown format %q", t.Report.Format))
		}
	}
	if t.Timeout != "" && !pat.Duration.MatchString(t.Timeout) {
		r.add(Error, "", at+"/timeout", fmt.Sprintf("%q is not a duration such as 30s or 10m", t.Timeout))
	}
	validateEnv(r, m, pat, at, t.Env)
}

// validateArgv enforces R1's non-structural half and R2.
func validateArgv(r *Result, pat spec.Rules, at string, argv Argv) {
	if len(argv) == 0 {
		return
	}
	for i, a := range argv {
		if a == "" {
			r.add(Error, "R1", fmt.Sprintf("%s/%d", at, i), "empty argument")
		}
	}
	// R2. Not an error: the command may be a committed, reviewable repo script
	// such as bin/rspec. It is surfaced with the exact argv and approved once,
	// keyed by that argv, so changing it re-prompts.
	if !pat.Allowlist[argv[0]] {
		r.add2(Diagnostic{
			Severity: Approval,
			Rule:     "R2",
			Path:     at,
			Msg:      fmt.Sprintf("%q is not on the default allowlist and needs one-time approval", argv[0]),
			Argv:     argv,
		})
	}
}

// validateEnv enforces R3 and R7 over a set of environment values.
func validateEnv(r *Result, m *Manifest, pat spec.Rules, at string, env map[string]string) {
	for _, k := range sortedKeys(env) {
		v := env[k]
		where := at + "/env/" + k

		// R3. The manifest is committed and reviewed in pull requests, so a
		// literal credential in it is already leaked.
		if loc := pat.Credential.FindString(v); loc != "" {
			r.add(Error, "R3", where,
				fmt.Sprintf("value contains what looks like a live credential (%q); use ${secret:path}", loc))
		} else if looksHighEntropy(v) {
			r.add(Error, "R3", where,
				"value looks like a literal credential (long, high entropy, no structure); use ${secret:path}")
		}

		// R7. The grammar itself.
		if !pat.Interpolated.MatchString(v) {
			r.add(Error, "R7", where,
				"contains an interpolation outside ${bay.<service>.<field>} and ${secret:<path>}")
			continue
		}

		// R7, resolved: a reference that parses but points nowhere is worse
		// than one that does not parse, because it fails at boot instead of
		// at validation.
		for _, mt := range refPattern.FindAllStringSubmatch(v, -1) {
			svc, field := mt[1], mt[2]
			target, ok := m.Services[svc]
			if !ok {
				r.add(Error, "R7", where, fmt.Sprintf("references unknown service %q", svc))
				continue
			}
			switch {
			case strings.HasPrefix(field, "ports."):
				pn := strings.TrimPrefix(field, "ports.")
				if _, ok := target.Ports[pn]; !ok {
					r.add(Error, "R7", where, fmt.Sprintf("service %q declares no named port %q", svc, pn))
				}
			case field == "url" || field == "public_url" || field == "port":
				if target.Port == 0 {
					r.add(Error, "R7", where,
						fmt.Sprintf("service %q declares no port, so .%s cannot resolve", svc, field))
				}
			}
			if field == "public_url" && target.IsOneshot() {
				r.add(Error, "R7", where, fmt.Sprintf("oneshot %q has no browser-facing origin", svc))
			}
		}
	}
}

func validateDurations(r *Result, pat spec.Rules, at string, s *Service) {
	if s.Health == nil {
		return
	}
	for field, v := range map[string]string{
		"timeout":      s.Health.Timeout,
		"interval":     s.Health.Interval,
		"start_period": s.Health.StartPeriod,
	} {
		if v != "" && !pat.Duration.MatchString(v) {
			r.add(Error, "", at+"/health/"+field, fmt.Sprintf("%q is not a duration such as 30s or 2m", v))
		}
	}
}

// validateGraph checks that needs edges resolve and form a DAG.
func validateGraph(r *Result, m *Manifest) {
	for _, name := range sortedKeys(m.Services) {
		s := m.Services[name]
		if s == nil {
			continue
		}
		for _, dep := range s.Needs {
			if _, ok := m.Services[dep]; !ok {
				r.add(Error, "", "services/"+name+"/needs", fmt.Sprintf("unknown service %q", dep))
			}
			if dep == name {
				r.add(Error, "", "services/"+name+"/needs", "depends on itself")
			}
		}
	}

	const (
		white = 0
		grey  = 1
		black = 2
	)
	colour := map[string]int{}
	var stack []string
	var visit func(string)
	visit = func(n string) {
		switch colour[n] {
		case grey:
			cycle := append(append([]string{}, stack[indexOf(stack, n):]...), n)
			r.add(Error, "", "services/"+n+"/needs",
				"dependency cycle: "+strings.Join(cycle, " -> "))
			return
		case black:
			return
		}
		colour[n] = grey
		stack = append(stack, n)
		s := m.Services[n]
		if s != nil {
			for _, dep := range s.Needs {
				if _, ok := m.Services[dep]; ok && dep != n {
					visit(dep)
				}
			}
		}
		stack = stack[:len(stack)-1]
		colour[n] = black
	}
	for _, n := range sortedKeys(m.Services) {
		visit(n)
	}
}

// validatePrimary ensures exactly one service claims the bare bay hostname.
func validatePrimary(r *Result, m *Manifest) {
	var claimed, ported []string
	for _, name := range sortedKeys(m.Services) {
		s := m.Services[name]
		if s == nil || s.IsOneshot() {
			continue
		}
		if s.Primary {
			claimed = append(claimed, name)
		}
		if s.Port != 0 {
			ported = append(ported, name)
		}
	}
	switch {
	case len(claimed) > 1:
		r.add(Error, "", "services",
			fmt.Sprintf("%d services claim primary (%s); the bare bay hostname can only route to one",
				len(claimed), strings.Join(claimed, ", ")))
	case len(claimed) == 1:
	case len(ported) == 1:
		// Inferred.
	case len(ported) == 0:
		r.add(Warn, "", "services", "no service exposes a port, so the bay has no browser-facing URL")
	default:
		r.add(Error, "", "services",
			fmt.Sprintf("%d services expose a port (%s); one must set primary: true to claim the bare bay hostname",
				len(ported), strings.Join(ported, ", ")))
	}
}

// looksHighEntropy is the heuristic half of R3, catching credentials with no
// recognisable prefix. It is deliberately conservative: a false positive here
// is a confusing error, and the prefix screen already covers the common cases.
func looksHighEntropy(v string) bool {
	if strings.Contains(v, "${") || len(v) < 24 {
		return false
	}
	if strings.ContainsAny(v, " /:\n") {
		return false // URLs, paths and prose are not bare credentials
	}
	distinct := map[rune]bool{}
	var upper, lower, digit int
	for _, c := range v {
		distinct[c] = true
		switch {
		case c >= 'A' && c <= 'Z':
			upper++
		case c >= 'a' && c <= 'z':
			lower++
		case c >= '0' && c <= '9':
			digit++
		}
	}
	return len(distinct) >= 16 && upper > 0 && lower > 0 && digit > 0
}

// dependsOn reports whether a transitively needs b.
func dependsOn(m *Manifest, a, b string) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(n string) bool {
		if n == b {
			return true
		}
		if seen[n] {
			return false
		}
		seen[n] = true
		s := m.Services[n]
		if s == nil {
			return false
		}
		for _, dep := range s.Needs {
			if walk(dep) {
				return true
			}
		}
		return false
	}
	s := m.Services[a]
	if s == nil {
		return false
	}
	for _, dep := range s.Needs {
		if walk(dep) {
			return true
		}
	}
	return false
}

func (r *Result) add(sev Severity, rule, path, msg string) {
	r.add2(Diagnostic{Severity: sev, Rule: rule, Path: path, Msg: msg})
}

func (r *Result) add2(d Diagnostic) { r.Diagnostics = append(r.Diagnostics, d) }

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return 0
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Location returns the manifest path a diagnostic refers to, with a rule
// prefix when one applies. Used by the CLI; Diagnostic.String is for logs.
func (d Diagnostic) Location() string {
	if d.Rule == "" {
		return d.Path
	}
	return d.Rule + " " + d.Path
}
