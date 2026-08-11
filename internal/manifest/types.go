// Package manifest parses and validates devbay.yaml.
//
// This package is the airlock. Everything upstream of it — an introspection
// agent reading a repository, a human editing YAML, a pull request — is
// untrusted. Everything downstream of it interprets a Manifest as instructions
// while holding credentials. A manifest that does not pass Validate must never
// reach the execution plane.
//
// The rules the validator enforces are numbered R1-R7 and documented on the
// constructs they govern in spec/devbay.schema.json. The load-bearing one is
// R1: commands are argv arrays, never shell strings, and are executed with
// execve rather than sh -c. That single constraint is what makes it safe to
// have a language model author this file, because there is no path from a
// manifest to arbitrary shell.
package manifest

import "gopkg.in/yaml.v3"

// Argv is a command as an argument vector.
//
// R1. Declared as a slice so that a YAML string fails to decode structurally
// rather than being coerced — the type system does the first half of the
// enforcement and Validate does the rest.
type Argv []string

// Manifest is a parsed devbay.yaml.
type Manifest struct {
	Version     int                  `yaml:"version"`
	Project     string               `yaml:"project"`
	Services    map[string]*Service  `yaml:"services"`
	Tasks       map[string]*Task     `yaml:"tasks"`
	Externals   map[string]*External `yaml:"externals,omitempty"`
	Supervision *Supervision         `yaml:"supervision,omitempty"`

	// Path is the file this manifest was loaded from. Not part of the format.
	Path string `yaml:"-"`
}

// Kind distinguishes a long-running service from a one-shot step.
type Kind string

const (
	// KindService is a long-running process. Requires a health probe (R5).
	KindService Kind = "service"
	// KindOneshot runs to completion and is healthy on exit code 0.
	//
	// Oneshot exists because every real repository has ordering-sensitive
	// setup — migrate, then seed, then start the app — and a flat list of
	// setup commands gets the ordering wrong: migrations must finish before
	// the app starts, not after every service is healthy. Modelling each step
	// as a oneshot means `needs` alone expresses the ordering, and there is
	// one dependency mechanism in the manifest rather than two.
	KindOneshot Kind = "oneshot"
)

// Scope controls how many instances of a service exist.
type Scope string

const (
	ScopeBay    Scope = "bay"    // one per bay; the normal case, including datastores
	ScopeShared Scope = "shared" // one total, forked per bay
)

// Fork is how per-bay data isolation is achieved for a stateful service.
type Fork string

const (
	// ForkImage bakes the seeded state into a per-project image; each bay runs
	// its own container and the writable layer provides copy-on-write. This is
	// the default because it is instant, parallelisable, and makes a leaked
	// fork structurally impossible — teardown is a container removal.
	ForkImage Fork = "image"
	// ForkTemplate is Postgres CREATE DATABASE ... TEMPLATE. Note this is a
	// full O(size) physical copy, not copy-on-write, and no other session may
	// be connected to the template while it runs, so forks serialize.
	ForkTemplate Fork = "template"
	ForkPrefix   Fork = "prefix" // key namespace prefix, e.g. Redis
	ForkSchema   Fork = "schema" // separate schema in one database
	ForkNone     Fork = "none"   // genuinely shared; warned about
)

// WatchAction is what the daemon does when a watched path changes.
type WatchAction string

const (
	WatchRestart WatchAction = "restart"
	WatchSync    WatchAction = "sync"
	WatchRebuild WatchAction = "rebuild"
)

// Service is a container in a bay.
type Service struct {
	Kind  Kind   `yaml:"kind,omitempty"`
	Image string `yaml:"image,omitempty"`
	Build *Build `yaml:"build,omitempty"`

	Scope Scope `yaml:"scope,omitempty"`
	Fork  Fork  `yaml:"fork,omitempty"`
	Seed  *Seed `yaml:"seed,omitempty"`

	Workdir string `yaml:"workdir,omitempty"`

	Install Argv `yaml:"install,omitempty"`
	// InstallScripts permits package lifecycle scripts during Install.
	//
	// False by default: devbay appends --ignore-scripts or the ecosystem
	// equivalent. Running install scripts on a freshly cloned untrusted
	// repository is the delivery mechanism used by the self-replicating
	// Shai-Hulud npm worm, which steals cloud credentials. Setting this true
	// is approval-gated.
	InstallScripts bool `yaml:"install_scripts,omitempty"`

	Start Argv `yaml:"start,omitempty"` // long-running services
	Run   Argv `yaml:"run,omitempty"`   // oneshots

	// Port is the primary port: the one that gets a hostname and, unless
	// overridden, the one an http probe targets. Exactly one exists per
	// service so hostname routing is unambiguous.
	Port int `yaml:"port,omitempty"`
	// Ports are additional named ports. Real services routinely expose more
	// than one — a mail catcher listens on SMTP and serves a web UI, an object
	// store serves an API and a console.
	Ports map[string]int `yaml:"ports,omitempty"`

	Primary bool     `yaml:"primary,omitempty"`
	Needs   []string `yaml:"needs,omitempty"`

	Health *Health `yaml:"health,omitempty"`

	// Watch globs are evaluated by the daemon on the host using native
	// FSEvents or inotify, never by a watcher inside the container: virtiofs
	// does not implement inotify, so host edits do not reliably produce
	// events in a container, and polling costs real CPU per watcher per bay.
	Watch       []string    `yaml:"watch,omitempty"`
	WatchAction WatchAction `yaml:"watch_action,omitempty"`

	// Volumes are paths backed by a named volume rather than the bind mount —
	// node_modules, .venv, vendor/bundle, target, .next. Not an optimisation
	// to defer: a bind-mounted dependency tree runs at roughly 2.5x native on
	// macOS and a named volume recovers most of that.
	Volumes []string `yaml:"volumes,omitempty"`

	// Egress is the outbound allowlist. Absent or empty means no outbound
	// network at all.
	//
	// R4. This field is never authorable by the introspection agent: the
	// validator strips it from model-produced manifests. If a model could
	// write the allowlist, a prompt injection would append its own
	// destination and the sandbox would defeat itself.
	Egress []string `yaml:"egress,omitempty"`

	Env map[string]string `yaml:"env,omitempty"`
}

// Build builds an image from the repo instead of pulling one.
type Build struct {
	Context    string `yaml:"context,omitempty"`
	Dockerfile string `yaml:"dockerfile,omitempty"`
	Target     string `yaml:"target,omitempty"`
}

// UnmarshalYAML accepts `build: ./dir` as well as the full mapping.
//
// The shorthand is what Compose uses and therefore what people write, what
// every example they have seen uses, and what devbay's own `init` suggests
// when it finds a service that builds from source. Rejecting it produced the
// worst kind of error: a type-mismatch message that blamed R1, the rule about
// commands being argv arrays, for a field that has nothing to do with
// commands.
//
// Accepting a scalar here does not weaken anything. Unlike a command, a
// context path is not executed, and it is confined to the worktree before use.
func (b *Build) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		var dir string
		if err := value.Decode(&dir); err != nil {
			return err
		}
		b.Context = dir
		return nil
	}
	// A named type, so decoding the mapping does not recurse into this method.
	type raw Build
	var out raw
	if err := value.Decode(&out); err != nil {
		return err
	}
	*b = Build(out)
	return nil
}

// Seed defines the state captured into a service's image for ForkImage.
//
// Seeding names oneshot services rather than carrying commands, because a
// migration does not run inside the database container: it runs inside the
// application container, against the database, using the application's own
// toolchain. A command here would have no unambiguous place to execute.
type Seed struct {
	After   []string `yaml:"after"`
	Sources []string `yaml:"sources"`
}

// Health is a readiness probe. Exactly one of the five forms must be set.
//
// R5. Without a health probe there is no verification loop, and without a
// verification loop auto-detection is only guessing.
//
// Probes run from the host against 127.0.0.1:<allocated port>, never against a
// *.localhost hostname — the daemon's own resolver cannot resolve those,
// because macOS does not honour RFC 6761 for subdomains of localhost and only
// Chrome, Firefox and curl special-case them.
type Health struct {
	HTTP string `yaml:"http,omitempty"` // path probed on the primary port
	TCP  int    `yaml:"tcp,omitempty"`  // port that must accept a connection
	Cmd  Argv   `yaml:"cmd,omitempty"`  // exec in container; healthy on exit 0

	// Log is an RE2 regex matched against stdout and stderr. Preferred for
	// processes with no port, and often better than an HTTP probe for dev
	// servers, which print an explicit ready line: Vite emits "ready in 412
	// ms", Sidekiq "Starting processing, hit Ctrl-C to stop", Celery
	// "celery@host ready.". Matching that is real readiness.
	Log string `yaml:"log,omitempty"`

	// Process is liveness only: healthy while the main process runs. The
	// weakest probe available, and Validate warns on it. It exists because the
	// alternative — inventing a fake HTTP endpoint for a Sidekiq or Celery
	// worker — is a lie that silently defeats the verification loop.
	Process bool `yaml:"process,omitempty"`

	Timeout     string `yaml:"timeout,omitempty"`
	Interval    string `yaml:"interval,omitempty"`
	StartPeriod string `yaml:"start_period,omitempty"`
}

// Task is a named finite command an agent or human runs against a bay.
type Task struct {
	Run Argv `yaml:"run"`

	// Needs is the service subgraph this task requires. An empty slice is
	// valid and common: a unit suite that boots zero containers is the
	// fastest path to a verified result.
	//
	// R6. Omitting the key entirely is an error rather than a default,
	// because forcing the author to think about it is the point.
	//
	// The distinction is carried by nil versus empty: YAML decodes `needs: []`
	// to an empty non-nil slice and an omitted key to nil, so no side channel
	// is needed -- which also means a Task built in Go rather than parsed from
	// a file can satisfy the rule, by writing []string{}.
	Needs []string `yaml:"needs"`

	In      string            `yaml:"in,omitempty"`
	Report  *Report           `yaml:"report,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
	Timeout string            `yaml:"timeout,omitempty"`

	needsSet bool
}

// ReportFormat is the machine-readable shape a test runner emits.
//
// There is no cross-language standard for agent-consumable test results.
// JUnit XML is the universal fallback; native streaming JSON is preferred
// where it exists because it gives live progress a report file cannot.
type ReportFormat string

const (
	ReportJUnit  ReportFormat = "junit"   // pytest, phpunit, vitest natively
	ReportJSON   ReportFormat = "json"    // jest --json, vitest --reporter=json
	ReportGoJSON ReportFormat = "go-json" // go test -json, a streaming event log
	ReportTAP    ReportFormat = "tap"
)

// Report says where a runner writes results. Path is empty for formats that
// stream to stdout rather than writing a file, notably go-json.
type Report struct {
	Format ReportFormat `yaml:"format"`
	Path   string       `yaml:"path,omitempty"`
}

// External is a third-party dependency and how it is satisfied locally.
//
// The default is always the emulator. This is the highest-leverage security
// decision in the format: most bays then need zero real credentials, and the
// ones that do are a short list a human actually looked at.
type External struct {
	Emulate string `yaml:"emulate"`
	Real    string `yaml:"real,omitempty"` // "never" or "gated"; there is no "always"
	Mint    *Mint  `yaml:"mint,omitempty"`
}

// Mint describes how to issue a short-lived scoped credential instead of
// passing a long-lived one through.
type Mint struct {
	Provider string         `yaml:"provider,omitempty"` // aws-sts, github-app, gcp-sa
	TTL      string         `yaml:"ttl,omitempty"`
	Scope    map[string]any `yaml:"scope,omitempty"`
}

// Supervision controls the per-bay identity surface injected by the proxy.
type Supervision struct {
	Banner      *bool `yaml:"banner,omitempty"`
	FaviconTint *bool `yaml:"favicon_tint,omitempty"`
}

// IsOneshot reports whether s runs to completion rather than staying up.
func (s *Service) IsOneshot() bool { return s.Kind == KindOneshot }

// Command returns the argv that launches s, whichever field holds it.
func (s *Service) Command() Argv {
	if s.IsOneshot() {
		return s.Run
	}
	return s.Start
}
