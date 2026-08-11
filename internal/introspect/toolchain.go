package introspect

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/Clarittyai/devbay/internal/manifest"
)

// A repository without a compose file still says what it needs; it just says
// it in four smaller files instead of one big one.
//
// Before this, devbay read a Procfile, transcribed `web: node index.js`, and
// then handed back a manifest with no image -- which does not validate, let
// alone boot. That is a worse outcome than not reading the Procfile at all,
// because the developer now has a file to finish rather than a tool that
// works, and the tool's one job was to write that file.
//
// So the toolchain is inferred the same way everything else here is: from
// evidence, in order of how much the repository committed to it. A declared
// version in engines, .nvmrc or go.mod is a transcription. Only when nothing
// is declared does devbay choose, and then it says so in the manifest's
// provenance and in the gap list, because a chosen version is the developer's
// decision to make and their right to disagree with.

// ecosystem is what a service is written in, as far as its start command and
// the files beside it can say.
type ecosystem struct {
	// image is the family; version is appended.
	image string
	// fallback is the version devbay uses when the repository names none.
	fallback string
	// suffix keeps the image small where the ecosystem has a slim variant that
	// is not a trap. Node and Ruby alpine builds are fine; Python alpine is
	// not -- it has no wheels, so every dependency compiles from source.
	suffix string
	// parts is how much of a version the official images actually tag. Node
	// publishes a major line; Python, Ruby, Go and PHP publish major.minor.
	// Asking for more precision than the registry has produces a tag that does
	// not exist, and the failure arrives as a pull error with no explanation
	// of where the number came from.
	parts int
}

var ecosystems = map[string]ecosystem{
	"node":   {image: "node", fallback: "22", suffix: "-alpine", parts: 1},
	"python": {image: "python", fallback: "3.12", suffix: "-slim", parts: 2},
	"ruby":   {image: "ruby", fallback: "3.3", suffix: "-slim", parts: 2},
	"go":     {image: "golang", fallback: "1.23", suffix: "", parts: 2},
	"php":    {image: "php", fallback: "8.3", suffix: "-cli", parts: 2},
}

// commandEcosystem reads the ecosystem off the program being run.
var commandEcosystem = map[string]string{
	"node": "node", "npm": "node", "npx": "node", "pnpm": "node", "yarn": "node",
	"next": "node", "vite": "node", "nest": "node", "ts-node": "node", "tsx": "node",
	"python": "python", "python3": "python", "gunicorn": "python", "uvicorn": "python",
	"flask": "python", "celery": "python", "manage.py": "python", "hypercorn": "python",
	"ruby": "ruby", "bundle": "ruby", "rails": "ruby", "puma": "ruby", "rake": "ruby", "sidekiq": "ruby",
	"go":  "go",
	"php": "php", "php-fpm": "php", "composer": "php",
}

// inferToolchain gives every image-less service an image it can actually run.
func (d *detector) inferToolchain() {
	for _, name := range sortedKeysOf(d.m.Services) {
		s := d.m.Services[name]
		if s.Image != "" || s.Build != nil {
			continue
		}
		eco, why := d.ecosystemOf(s)
		if eco == "" {
			d.gap("service %q has no image and devbay could not tell what it is written in; set `image:`", name)
			continue
		}
		spec := ecosystems[eco]

		version, vwhy := d.toolchainVersion(eco)
		version = trimVersion(version, spec.parts)
		if version == "" {
			version = spec.fallback
			vwhy = fmt.Sprintf("no version is declared anywhere, so devbay chose %s", version)
			d.gap("service %q runs on %s %s because the repository does not say which version; "+
				"pin it in %s", name, eco, version, versionFileFor(eco))
		}
		s.Image = spec.image + ":" + version + spec.suffix
		d.note(SourceConvention, "", fmt.Sprintf("service %q runs on %s (%s; %s)", name, s.Image, why, vwhy))

		if inst, file := d.installFor(eco); len(inst) > 0 {
			s.Install = inst
			d.note(SourceConvention, file, fmt.Sprintf("service %q installs with %s", name, strings.Join(inst, " ")))

			// Dependencies belong somewhere both the install container and the
			// service can see, and somewhere off the bind mount -- which is a
			// named volume. Where "somewhere" is differs by ecosystem, and
			// getting it wrong is invisible: pip installs into the throwaway
			// install container's own filesystem, reports success, and the
			// service then starts without Django.
			if dep := dependencyDir(eco); dep != "" {
				s.Volumes = append(s.Volumes, dep)
			}
			if s.Env == nil {
				s.Env = map[string]string{}
			}
			for k, v := range dependencyEnv(eco) {
				if _, set := s.Env[k]; !set {
					s.Env[k] = v
				}
			}
		}
		d.inferPort(name, s, eco)
	}
}

// defaultPort is the port devbay asks a web process to listen on.
//
// Any number would do. What matters is that devbay tells the application which
// one it picked, rather than guessing which one the application picked.
const defaultPort = 8080

// inferPort gives a web process a port by telling it which one to use.
//
// The Procfile contract is that the web process binds $PORT -- it is how
// Heroku, Render, Fly and every platform-as-a-service that reads this file
// starts an application, and it is why almost every such application already
// contains `process.env.PORT || 3000`. So devbay does not guess the port: it
// sets PORT and routes to it, which is right whenever the application honours
// the contract and visible in one line of the manifest when it does not.
//
// Only for the web process. A worker has no port, and giving it one produces a
// health probe that can never pass.
func (d *detector) inferPort(name string, s *manifest.Service, eco string) {
	if s.Port != 0 || s.IsOneshot() || eco == "go" {
		return // go binaries take a flag far more often than an env var
	}
	if name != "web" && len(d.m.Services) != 1 {
		return
	}
	if s.Env == nil {
		s.Env = map[string]string{}
	}
	if _, set := s.Env["PORT"]; !set {
		s.Env["PORT"] = strconv.Itoa(defaultPort)
	}
	s.Port = defaultPort
	d.note(SourceConvention, "",
		fmt.Sprintf("service %q is told to listen on PORT=%d, the contract every Procfile platform uses", name, defaultPort))
	d.gap("service %q was given PORT=%d; if it ignores PORT and hardcodes its own, set `port:` to that instead",
		name, defaultPort)
}

// ecosystemOf identifies a service from its command, then from the repository.
func (d *detector) ecosystemOf(s *manifest.Service) (string, string) {
	argv := s.Command()
	if len(argv) > 0 {
		base := filepath.Base(argv[0])
		if eco, ok := commandEcosystem[base]; ok {
			return eco, "it runs " + base
		}
		// `bundle exec puma`, `npm run start` and friends already matched on
		// argv[0]; a bare script is identified by its extension instead.
		switch filepath.Ext(base) {
		case ".js", ".mjs", ".cjs", ".ts":
			return "node", "it runs a JavaScript file"
		case ".py":
			return "python", "it runs a Python file"
		case ".rb":
			return "ruby", "it runs a Ruby file"
		case ".php":
			return "php", "it runs a PHP file"
		}
	}
	// No usable command, so fall back to what the repository is made of.
	for _, c := range []struct{ file, eco string }{
		{"package.json", "node"},
		{"pyproject.toml", "python"},
		{"requirements.txt", "python"},
		{"Gemfile", "ruby"},
		{"go.mod", "go"},
		{"composer.json", "php"},
	} {
		if d.exists(c.file) {
			return c.eco, "the repository has a " + c.file
		}
	}
	return "", ""
}

// toolchainVersion finds the version the repository committed to.
//
// Ordered by how deliberate each source is. A .tool-versions or mise.toml
// entry is a version the developer pinned for themselves; engines.node is one
// they published to their users; a bare go directive is the language's own
// record. Any of them beats devbay choosing.
func (d *detector) toolchainVersion(eco string) (string, string) {
	type source struct {
		file string
		find func(string) string
	}
	var sources []source

	switch eco {
	case "node":
		sources = []source{
			{".nvmrc", firstVersion},
			{".node-version", firstVersion},
			{"package.json", func(b string) string {
				var pkg struct {
					Engines struct {
						Node string `json:"node"`
					} `json:"engines"`
				}
				if json.Unmarshal([]byte(b), &pkg) != nil {
					return ""
				}
				return firstVersion(pkg.Engines.Node)
			}},
		}
	case "python":
		sources = []source{
			{".python-version", firstVersion},
			{"pyproject.toml", func(b string) string {
				return firstVersion(matchLine(b, `requires-python\s*=\s*"([^"]+)"`))
			}},
			{"runtime.txt", firstVersion}, // python-3.12.1
		}
	case "ruby":
		sources = []source{
			{".ruby-version", firstVersion},
			{"Gemfile", func(b string) string { return firstVersion(matchLine(b, `(?m)^\s*ruby\s+["']([^"']+)["']`)) }},
		}
	case "go":
		sources = []source{
			{"go.mod", func(b string) string { return firstVersion(matchLine(b, `(?m)^go\s+([0-9.]+)`)) }},
		}
	case "php":
		sources = []source{
			{"composer.json", func(b string) string {
				var pkg struct {
					Require map[string]string `json:"require"`
				}
				if json.Unmarshal([]byte(b), &pkg) != nil {
					return ""
				}
				return firstVersion(pkg.Require["php"])
			}},
		}
	}
	// Version managers first: they are the file a developer actually switches
	// their own shell with, so they are the most current thing in the tree.
	sources = append([]source{
		{".tool-versions", func(b string) string { return firstVersion(matchLine(b, `(?m)^`+eco+`\s+([0-9][0-9.]*)`)) }},
		{"mise.toml", func(b string) string { return firstVersion(matchLine(b, `(?m)^\s*`+eco+`\s*=\s*["']([^"']+)["']`)) }},
		{".mise.toml", func(b string) string { return firstVersion(matchLine(b, `(?m)^\s*`+eco+`\s*=\s*["']([^"']+)["']`)) }},
	}, sources...)

	for _, src := range sources {
		b, err := os.ReadFile(filepath.Join(d.dir, src.file))
		if err != nil {
			continue
		}
		if v := src.find(string(b)); v != "" {
			return v, "from " + src.file
		}
	}
	return "", ""
}

// installFor is the install command the lockfile implies.
//
// The lockfile, not the package manager's name: a repository with a
// pnpm-lock.yaml is a pnpm repository whatever its README says, and installing
// it with npm produces a different dependency tree from the one its tests were
// written against.
func (d *detector) installFor(eco string) (manifest.Argv, string) {
	switch eco {
	case "node":
		switch {
		case d.exists("pnpm-lock.yaml"):
			return manifest.Argv{"pnpm", "install", "--frozen-lockfile"}, "pnpm-lock.yaml"
		case d.exists("yarn.lock"):
			return manifest.Argv{"yarn", "install", "--frozen-lockfile"}, "yarn.lock"
		case d.exists("package-lock.json"):
			return manifest.Argv{"npm", "ci"}, "package-lock.json"
		case d.exists("package.json"):
			return manifest.Argv{"npm", "install"}, "package.json"
		}
	case "python":
		// --target, because the alternative is site-packages inside a
		// container devbay is about to delete.
		if d.exists("requirements.txt") {
			return manifest.Argv{"pip", "install", "--no-cache-dir", "--target", pythonDeps, "-r", "requirements.txt"}, "requirements.txt"
		}
		if d.exists("pyproject.toml") {
			return manifest.Argv{"pip", "install", "--no-cache-dir", "--target", pythonDeps, "."}, "pyproject.toml"
		}
	case "ruby":
		if d.exists("Gemfile") {
			return manifest.Argv{"bundle", "install"}, "Gemfile"
		}
	case "php":
		if d.exists("composer.json") {
			return manifest.Argv{"composer", "install"}, "composer.json"
		}
	}
	return nil, ""
}

// pythonDeps is where devbay puts Python packages: outside the worktree,
// because it is not the developer's code and does not belong in their
// `git status`.
const pythonDeps = "/opt/devbay/python"

// dependencyDir is where an ecosystem puts what it installs.
func dependencyDir(eco string) string {
	switch eco {
	case "node":
		return "node_modules"
	case "ruby":
		return "vendor/bundle"
	case "php":
		return "vendor"
	case "python":
		return pythonDeps
	}
	// Go builds into the module cache and links a binary, so there is nothing
	// in the worktree to keep.
	return ""
}

// dependencyEnv points the runtime at what install produced.
func dependencyEnv(eco string) map[string]string {
	switch eco {
	case "python":
		return map[string]string{
			"PYTHONPATH": pythonDeps,
			// pip --target puts console scripts here: gunicorn, uvicorn,
			// celery. Spelled out in full rather than prepended to $PATH,
			// because devbay never expands a variable inside a value -- that
			// would be shell semantics in a file that deliberately has none.
			"PATH": pythonDeps + "/bin:/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin",
		}
	case "ruby":
		return map[string]string{"BUNDLE_PATH": "vendor/bundle"}
	}
	return nil
}

func versionFileFor(eco string) string {
	switch eco {
	case "node":
		return ".nvmrc or package.json engines.node"
	case "python":
		return ".python-version"
	case "ruby":
		return ".ruby-version"
	case "go":
		return "go.mod"
	case "php":
		return "composer.json require.php"
	}
	return ".tool-versions"
}

func (d *detector) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(d.dir, rel))
	return err == nil
}

var versionPattern = regexp.MustCompile(`[0-9]+(\.[0-9]+)*`)

// firstVersion pulls a usable image tag out of whatever the file said.
//
// Ranges and prefixes are everywhere in this data -- ">=18", "^20.11.0",
// "~> 3.2", "v18.17.1", "python-3.12.1" -- and every one of them means the
// same thing for choosing a base image: the major version, sometimes the
// minor. Anything more precise than that is a tag that may not exist.
func firstVersion(s string) string {
	m := versionPattern.FindString(strings.TrimSpace(s))
	if m == "" {
		return ""
	}
	parts := strings.Split(m, ".")
	if len(parts) > 2 {
		parts = parts[:2]
	}
	// A leading zero major is not a version anyone ships an image for.
	if parts[0] == "0" || parts[0] == "" {
		return ""
	}
	return strings.Join(parts, ".")
}

// trimVersion cuts a version down to the precision the registry publishes.
func trimVersion(v string, parts int) string {
	if v == "" || parts <= 0 {
		return v
	}
	p := strings.Split(v, ".")
	if len(p) > parts {
		p = p[:parts]
	}
	return strings.Join(p, ".")
}

func matchLine(body, pattern string) string {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return ""
	}
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}
