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
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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

	d.inferToolchain()
	d.openDjangoHosts()
	d.rewireEnv()
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

	// composeRunsTheApp records that a compose file described the application
	// itself, not merely the things it talks to.
	//
	// It makes the language conventions stop adding services. A repository
	// whose compose file runs the app has already said how it runs, in a file
	// its authors wrote and use; package.json's `start` script is usually the
	// very same program that service runs. Adding it again produced a
	// duplicate that claimed the bay hostname, was wired to no database, and
	// exited on boot -- so a stack that composed perfectly well came up
	// degraded, and the service that failed was one the repository never had.
	//
	// Datastores do not count, because "compose brings up postgres and redis,
	// the app runs from package.json" is a normal way to work, and there the
	// conventions are the only thing that knows how to start the application.
	composeRunsTheApp bool
}

// conventionsMayAddServices reports whether guessing a service is still useful.
//
// Tasks are exempt on purpose: a test script is worth picking up from
// package.json however the app is started, because a compose file describes
// how to run the app and says nothing about how to test it.
func (d *detector) conventionsMayAddServices() bool { return !d.composeRunsTheApp }

func (d *detector) note(src Source, path, detail string) {
	rel := path
	if r, err := filepath.Rel(d.dir, path); err == nil && !strings.HasPrefix(r, "..") {
		rel = r
	}
	d.res.Evidence = append(d.res.Evidence, Evidence{Source: src, Path: rel, Detail: detail})
}

// composeBuild transcribes a Compose build stanza into devbay's form.
//
// Compose resolves the context relative to the project directory, which for a
// compose file at the repository root is the same place devbay resolves it
// from; a compose file in a subdirectory is the case worth being careful
// about, so an absolute context is turned back into a repository-relative one
// rather than written out as a path that only exists on this machine.
func (d *detector) composeBuild(svc composetypes.ServiceConfig) *manifest.Build {
	if svc.Build == nil {
		return nil
	}
	ctxDir := svc.Build.Context
	if ctxDir == "" {
		ctxDir = "."
	}
	if filepath.IsAbs(ctxDir) {
		if r, err := filepath.Rel(d.dir, ctxDir); err == nil && !strings.HasPrefix(r, "..") {
			ctxDir = r
		}
	}
	ctxDir = filepath.ToSlash(filepath.Clean(ctxDir))
	if !strings.HasPrefix(ctxDir, ".") {
		ctxDir = "./" + ctxDir
	}

	b := &manifest.Build{Context: ctxDir, Target: svc.Build.Target}
	// Only recorded when it differs from the default, so the generated file
	// stays readable.
	if df := svc.Build.Dockerfile; df != "" && df != "Dockerfile" {
		b.Dockerfile = df
	}
	// Build arguments decide what is inside the image, not just how it is
	// labelled: a Dockerfile that takes ARG NODE_ENV and runs `npm ci`
	// installs the development dependencies or skips them on that one value,
	// and the image that skipped them builds cleanly and then exits 127
	// because the command the compose file runs was never installed.
	for k, v := range svc.Build.Args {
		if v == nil || !safeEnvValue(*v) {
			// A value devbay cannot vouch for is one it will not bake into an
			// image and commit alongside the manifest.
			d.gap("service %q builds with %s, whose value devbay did not carry over; set it in `build.args`",
				svc.Name, k)
			continue
		}
		if b.Args == nil {
			b.Args = map[string]string{}
		}
		b.Args[k] = *v
	}
	return b
}

// composeSecrets turns a service's compose secrets into file mounts.
//
// Docker places a secret at /run/secrets/<name> and applications read it from
// there, so the mount is the transcription: same path, same contents, same
// behaviour. Only file-backed secrets can be transcribed this way -- an
// environment-backed one is a value devbay must not copy into a manifest,
// which is what `${secret:...}` exists for -- and the difference is reported
// rather than silently dropped.
func (d *detector) composeSecrets(project *composetypes.Project, svc composetypes.ServiceConfig, path string) []manifest.Mount {
	var out []manifest.Mount
	for _, ref := range svc.Secrets {
		def, ok := project.Secrets[ref.Source]
		if !ok {
			d.gap("service %q uses secret %q, which the compose file does not define", svc.Name, ref.Source)
			continue
		}
		target := ref.Target
		if target == "" {
			target = "/run/secrets/" + ref.Source
		} else if !strings.HasPrefix(target, "/") {
			target = "/run/secrets/" + target
		}
		switch {
		case def.File != "":
			src := def.File
			if !strings.HasPrefix(src, "./") && !strings.HasPrefix(src, "/") {
				src = "./" + src
			}
			if strings.HasPrefix(src, "/") || strings.Contains(src, "..") {
				// A mount source outside the repository would be refused by
				// the validator, so a manifest carrying it could never boot.
				d.gap("service %q reads secret %q from %s, which is outside the repository; "+
					"mount it yourself or use ${secret:...}", svc.Name, ref.Source, def.File)
				continue
			}
			out = append(out, manifest.Mount{Source: src, Target: target})
		case def.Environment != "":
			d.gap("service %q reads secret %q from the environment; devbay does not copy credentials "+
				"into a manifest -- give it as ${secret:%s} in `env:`", svc.Name, ref.Source, ref.Source)
		default:
			d.gap("service %q uses secret %q, which names no file devbay can mount", svc.Name, ref.Source)
		}
	}
	return out
}

// composeMounts transcribes the host bind mounts a compose service declares.
//
// Only binds whose source is inside the repository. A named volume is a
// different construct devbay models separately, and an absolute host path is
// something the developer has to decide about themselves -- silently binding
// /var/run/docker.sock, which real compose files do, would hand a bay the
// daemon and with it the machine.
func (d *detector) composeMounts(svc composetypes.ServiceConfig) ([]manifest.Mount, bool) {
	var out []manifest.Mount
	var dockerSocket bool
	for _, v := range svc.Volumes {
		if v.Type != "bind" || v.Source == "" || v.Target == "" || v.Target == "/" {
			continue
		}
		src := v.Source
		if filepath.IsAbs(src) {
			r, err := filepath.Rel(d.dir, src)
			if err != nil || strings.HasPrefix(r, "..") {
				if strings.HasSuffix(src, "docker.sock") {
					// Transcribed as the field that asks for it rather than
					// dropped. devbay is an orchestration layer, and a reverse
					// proxy that reads container labels or a test suite that
					// starts its own containers are things Docker runs -- so
					// refusing outright left devbay unable to run them at all.
					// The field is refused until a person approves it, so
					// nothing reaches the daemon by accident.
					dockerSocket = true
					d.gap("service %q asks for the Docker daemon socket, so devbay wrote "+
						"`docker_socket: true` and will not start it until you approve that: a "+
						"container holding the socket can start any other container on this machine",
						svc.Name)
					continue
				}
				d.gap("service %q binds %s, which is outside the repository; devbay did not carry it over",
					svc.Name, v.Source)
				continue
			}
			src = r
		}
		src = filepath.ToSlash(filepath.Clean(src))
		if src == ".." || strings.HasPrefix(src, "../") {
			continue
		}
		if !strings.HasPrefix(src, ".") {
			src = "./" + src
		}
		out = append(out, manifest.Mount{Source: src, Target: v.Target})
	}
	return out, dockerSocket
}

// composeShadowed transcribes the anonymous volumes a compose file uses to
// protect a directory from the bind mount over it.
//
// `- /project/node_modules` with no source is the standard dev-compose idiom:
// the line above bind-mounts the source tree, which would otherwise hide the
// dependencies the image installed at build time, and the anonymous volume
// puts them back. Dropping it leaves an application whose dependencies have
// vanished -- `ng serve` exits 1, `npm start` cannot find its entry point --
// with nothing in the log that points at a mount.
//
// It maps onto devbay's `volumes:`, which exists for the same reason and is
// also the answer to bind-mount performance on macOS.
func (d *detector) composeShadowed(svc composetypes.ServiceConfig, mounts []manifest.Mount) []string {
	var out []string
	for _, v := range svc.Volumes {
		if v.Source != "" || v.Target == "" || v.Target == "/" {
			continue
		}
		// Only meaningful when it sits inside a directory this service also
		// bind-mounts; anywhere else it is ordinary container-local storage
		// and devbay's own volume handling already covers it.
		for _, m := range mounts {
			rel, err := filepath.Rel(m.Target, v.Target)
			if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
				continue
			}
			// Absolute, so it lands inside the bind mount it is protecting
			// rather than inside the workspace.
			out = append(out, filepath.ToSlash(v.Target))
			break
		}
	}
	return out
}

// rewireEnv turns hardcoded addresses between services into references.
//
// This is the whole multi-service problem in one place. A compose file wires a
// client to its API with a literal -- "http://localhost:4000" for the browser,
// "http://server:4000" for server-to-server -- and both are wrong the moment
// there is more than one instance of the stack. localhost:4000 belongs to
// whichever bay happens to hold that port, and "server" resolves inside every
// bay at once, so every client would talk to some arbitrary bay's API.
// Transcribing them verbatim produces a bay that boots, reports healthy, and
// does not work.
//
// devbay already knows the right answers; they just have to be used. Which one
// depends on who does the calling, and a compose file says so more reliably
// than any guess about variable names:
//
//   - A value pointing at localhost was written to be opened from the
//     developer's machine -- a browser or a curl -- because inside compose,
//     containers address each other by service name. That is the browser
//     plane: ${bay.<svc>.public_url}.
//   - A value pointing at a service name is one container calling another.
//     That is the container plane: ${bay.<svc>.url}.
func (d *detector) rewireEnv() {
	byPort := map[int][]string{}
	for name, s := range d.m.Services {
		if s.Port != 0 {
			byPort[s.Port] = append(byPort[s.Port], name)
		}
		for _, p := range s.Ports {
			byPort[p] = append(byPort[p], name)
		}
	}
	for _, names := range byPort {
		sort.Strings(names)
	}

	for _, svcName := range sortedKeysOf(d.m.Services) {
		s := d.m.Services[svcName]
		for _, key := range sortedKeysOf(s.Env) {
			rewritten, target, plane, ok := d.rewireValue(s.Env[key], byPort, svcName)
			if !ok {
				continue
			}
			s.Env[key] = rewritten
			d.note(SourceConvention, "",
				fmt.Sprintf("%s.%s points at service %q, so it was rewired to its %s address",
					svcName, key, target, plane))
		}
	}
}

// rewireValue rewrites one address, and reports what it decided.
func (d *detector) rewireValue(value string, byPort map[int][]string, self string) (out, target, plane string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u.Host == "" || u.Scheme == "" {
		return "", "", "", false
	}
	host := u.Hostname()
	port, _ := strconv.Atoi(u.Port())

	// Only a browser distinguishes the two planes, and only over HTTP. A
	// postgres:// or redis:// address is read by a client library, never
	// opened in a tab, so it wants the container address whichever host was
	// written -- and those are the most common inter-service URLs there are.
	browserFacing := u.Scheme == "http" || u.Scheme == "https"

	switch {
	case host == "localhost" || host == "127.0.0.1" || host == "0.0.0.0" || host == "host.docker.internal":
		if !browserFacing {
			// A datastore addressed as localhost in compose is this stack's
			// own datastore; ${bay.<svc>.url} resolves correctly from inside a
			// container and from the host alike.
			if port == 0 {
				return "", "", "", false
			}
			owners := byPort[port]
			if len(owners) != 1 {
				return "", "", "", false
			}
			// No path: a datastore reference is the whole address. devbay
			// builds ${bay.db.url} from the service's own credentials and
			// database name, so appending compose's path produced
			// postgres://…/taskboard/taskboard.
			return "${bay." + owners[0] + ".url}", owners[0], "container", true
		}
		// Reached from the developer's machine, so it is the browser origin.
		// Without a port there is nothing to match against, and guessing which
		// service someone meant would be worse than leaving it alone.
		if port == 0 {
			return "", "", "", false
		}
		owners := byPort[port]
		if len(owners) != 1 {
			if len(owners) > 1 {
				d.gap("a value points at localhost:%d, which %d services expose (%s); pick one and write ${bay.<service>.public_url}",
					port, len(owners), strings.Join(owners, ", "))
			}
			return "", "", "", false
		}
		return replaceHost(u, "${bay."+owners[0]+".public_url}"), owners[0], "browser", true

	default:
		// A service name: one container calling another.
		target, isService := d.m.Services[host]
		if !isService || host == self {
			return "", "", "", false
		}
		// A datastore in a compose file usually publishes no port, so the
		// service devbay transcribed has none either -- and ${bay.db.url}
		// cannot resolve without one. The URL is the missing information: it
		// names the port that service listens on. Taking it from there turns
		// a manifest that could not validate into one that works, which is the
		// difference between the reference being an improvement and being a
		// confidently wrong file.
		if target.Port == 0 {
			if port == 0 {
				return "", "", "", false
			}
			target.Port = port
			d.note(SourceConvention, "",
				fmt.Sprintf("service %q listens on %d, read from the address %q used to reach it",
					host, port, self))
		}
		if !browserFacing {
			return "${bay." + host + ".url}", host, "container", true
		}
		return replaceHost(u, "${bay."+host+".url}"), host, "container", true
	}
}

// replaceHost swaps a URL's scheme and authority for a reference, keeping the
// path -- an API base of http://localhost:4000/v1 must stay pointed at /v1.
func replaceHost(u *url.URL, ref string) string {
	out := ref
	if p := strings.TrimSuffix(u.EscapedPath(), "/"); p != "" {
		out += p
	}
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
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
			// A compose service carrying both `image:` and `build:` builds the
			// image and tags it with that name -- the name is an output, not
			// something to pull. Preferring the image left devbay asking a
			// registry for a tag that only ever existed on the machine that
			// built it, and the error is a pull denial that says nothing about
			// the build sitting right beside it.
			if svc.Build != nil {
				image = ""
			}

			s := &manifest.Service{Image: image, Env: map[string]string{}}
			if s.Image == "" {
				// Transcribed rather than skipped. Most repositories describe
				// their own application with a Dockerfile, so dropping these
				// left a manifest holding the database and the cache and not
				// the thing the developer actually wanted to run.
				b := d.composeBuild(svc)
				if b == nil {
					d.gap("service %q declares neither an image nor a build context; set `image:` or `build:`", name)
					continue
				}
				s.Build = b
				// A service built from a directory in this repository is one
				// whose code lives there, so that directory is what a
				// developer will edit. Watching anything else would be a
				// guess; watching this is a transcription.
				if ctxDir := strings.TrimPrefix(b.Context, "./"); ctxDir != "" && ctxDir != "." {
					s.Watch = []string{ctxDir + "/**"}
					s.WatchAction = manifest.WatchRebuild
				}
				d.note(SourceCompose, path,
					fmt.Sprintf("service %q built from %s", name, b.Context))
			}

			for k, v := range svc.Environment {
				if v != nil && safeEnvValue(*v) {
					s.Env[k] = *v
				}
			}
			// An env_file is configuration devbay deliberately does not copy:
			// the values in one are frequently credentials, and a manifest is
			// a committed, reviewable file. Saying nothing about it produced a
			// service that boots with none of its configuration and no
			// explanation, which is worse than either copying or refusing.
			for _, ef := range svc.EnvFiles {
				d.gap("service %q reads %s, which devbay did not copy -- a manifest is committed and "+
					"those values are often credentials. Put the ones it needs in `env:`, "+
					"as literals or as ${secret:...}", name, ef.Path)
			}

			// Labels are how a label-driven proxy finds its backends. Dropping
			// them left traefik with nothing to route to: the stack came up
			// healthy and answered 404.
			for k, v := range svc.Labels {
				if !safeEnvValue(v) {
					continue
				}
				if s.Labels == nil {
					s.Labels = map[string]string{}
				}
				s.Labels[k] = v
			}

			// The command is what the service actually runs. compose-go has
			// already split it into an argv array -- devbay never does that
			// splitting itself -- so transcribing it preserves R1 exactly:
			// the same words, exec'd, with no shell anywhere. Dropping it
			// meant a service started with its image's default command, which
			// for prometheus is a different config file and for mariadb a
			// different authentication plugin.
			if len(svc.Command) > 0 {
				s.Start = manifest.Argv(svc.Command)
				d.note(SourceCompose, path, fmt.Sprintf("service %q runs %s", name, strings.Join(svc.Command, " ")))
			}
			if len(svc.Entrypoint) > 0 {
				d.gap("service %q overrides its image entrypoint (%s); devbay has no entrypoint field, "+
					"so fold it into `start:` or into the image", name, strings.Join(svc.Entrypoint, " "))
			}

			// Compose secrets are files, and the services that use them do not
			// start without them: a database told to read its root password
			// from /run/secrets/db-password exits immediately when the path is
			// not there. Dropping them produced a manifest that transcribed
			// the environment variable naming the file and not the file, which
			// is the most confusing possible half-transcription -- everything
			// looks right and the database dies on boot.
			binds, wantsSocket := d.composeMounts(svc)
			s.DockerSocket = wantsSocket
			if shadowed := d.composeShadowed(svc, binds); len(shadowed) > 0 {
				s.Volumes = shadowed
				d.note(SourceCompose, path, fmt.Sprintf("service %q keeps %s out of the bind mount",
					name, strings.Join(shadowed, ", ")))
			}
			secrets := d.composeSecrets(project, svc, path)
			mounts := append(binds, secrets...)
			if len(mounts) > 0 {
				s.Mounts = mounts
				for _, mt := range mounts {
					d.note(SourceCompose, path,
						fmt.Sprintf("service %q mounts %s at %s", name, mt.Source, mt.Target))
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

			// Transcribed, because it is load-bearing. A compose file that
			// cannot express "web starts after redis is answering" writes
			// `restart: on-failure` instead, and dropping it turns a stack
			// that recovers from its own startup race into one that dies.
			if r := manifest.Restart(svc.Restart); r != "" && r.Valid() {
				s.Restart = r
				d.note(SourceCompose, path,
					fmt.Sprintf("service %q restarts %s", name, r))
			}
			d.m.Services[name] = s
			// Set here rather than at the top of the loop: a compose file whose
			// every service was unusable described nothing, and the conventions
			// are the only thing left that can. A build stanza makes it the
			// application by definition -- nobody builds an image for postgres.
			if s.Build != nil || !datastore(s.Image) {
				d.composeRunsTheApp = true
			}
			if s.Build == nil {
				d.note(SourceCompose, path, fmt.Sprintf("service %q from image %s", name, s.Image))
			}
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
			if key == "" || d.m.Services[key] != nil || !d.conventionsMayAddServices() {
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

	// `dev` first, because a development server with reload is what a bay is
	// for -- but `start` counts too. Plenty of applications ship only `start`,
	// and reading a package.json that plainly says how to run the thing and
	// then reporting that no services were detected is the kind of answer that
	// makes a tool not worth running.
	for _, script := range []string{"dev", "start"} {
		if _, has := pkg.Scripts[script]; !has || d.m.Services["web"] != nil {
			continue
		}
		if !d.conventionsMayAddServices() {
			// The compose file already runs this application.
			break
		}
		d.m.Services["web"] = &manifest.Service{
			Start:   manifest.Argv{pm, "run", script},
			Port:    port,
			Install: manifest.Argv{pm, installVerb(pm)},
			Volumes: []string{"node_modules"},
		}
		detail := fmt.Sprintf("`%s run %s`", pm, script)
		if framework != "" {
			detail += fmt.Sprintf("; port %d inferred from %s", port, framework)
		}
		d.note(SourcePackageJSON, path, detail)
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
	if _, err := os.Stat(filepath.Join(d.dir, "manage.py")); err == nil && d.conventionsMayAddServices() {
		if d.m.Services["web"] == nil {
			d.m.Services["web"] = &manifest.Service{
				Start: manifest.Argv{"python", "manage.py", "runserver", "0.0.0.0:8000"},
				Port:  8000,
			}
			d.note(SourcePython, filepath.Join(d.dir, "manage.py"), "Django: runserver on 8000")
			d.gap("service \"web\" has no image; set one providing Python")
		}
		if d.m.Services["migrate"] == nil {
			d.m.Services["migrate"] = &manifest.Service{
				Kind: manifest.KindOneshot,
				Run:  manifest.Argv{"python", "manage.py", "migrate", "--no-input"},
			}
			d.note(SourcePython, filepath.Join(d.dir, "manage.py"), "Django: migrations as a oneshot")
			d.dependOn("migrate")
		}
		// collectstatic only when the project has somewhere to collect to.
		// Django raises without STATIC_ROOT, so running it unconditionally
		// would break every project that does not use it -- and skipping it
		// breaks every project that does, with a 500 from a missing manifest
		// entry that reads like an application bug.
		if d.grepRepo("STATIC_ROOT") && d.m.Services["collectstatic"] == nil {
			d.m.Services["collectstatic"] = &manifest.Service{
				Kind: manifest.KindOneshot,
				Run:  manifest.Argv{"python", "manage.py", "collectstatic", "--no-input"},
			}
			d.note(SourcePython, filepath.Join(d.dir, "manage.py"),
				"Django: collectstatic as a oneshot, because the settings declare STATIC_ROOT")
			d.dependOn("collectstatic")
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

// djangoHostEnv finds the environment variable a settings module reads its
// allowed hosts from.
var djangoHostEnv = regexp.MustCompile(
	`ALLOWED_HOSTS\s*=\s*[a-zA-Z_.]*\(?\s*["']?([A-Z_]*ALLOWED_HOSTS?[A-Z_]*)["']`)

// openDjangoHosts lets the bay's own hostname through Django's allowlist.
//
// A bay's browser origin is the point of the whole tool, and Django answers it
// with 400 because ALLOWED_HOSTS does not contain it -- while devbay's own
// health probe, which uses 127.0.0.1, sees 200 and reports the bay healthy.
// The developer opens the URL devbay printed and gets a bad request from an
// application that is working.
//
// devbay only does this when the settings file says it reads the value from
// the environment, and only into the variable it names. That is transcription:
// the repository states where the setting comes from, and devbay fills it in.
// A project that hardcodes its ALLOWED_HOSTS is left alone and told why.
func (d *detector) openDjangoHosts() {
	web := d.m.Services["web"]
	if web == nil {
		return
	}
	var name string
	var sawSettings bool
	_ = filepath.WalkDir(d.dir, func(p string, de fs.DirEntry, err error) error {
		if err != nil || name != "" {
			return nil
		}
		if de.IsDir() {
			switch de.Name() {
			case ".git", "node_modules", ".venv", "venv", "__pycache__":
				return fs.SkipDir
			}
			return nil
		}
		if de.Name() != "settings.py" {
			return nil
		}
		sawSettings = true
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		if m := djangoHostEnv.FindSubmatch(b); m != nil {
			name = string(m[1])
		}
		return nil
	})
	if name == "" {
		// Only worth saying to a project that has the setting. Said to
		// everyone, it is noise, and a gap list nobody reads is the same as no
		// gap list.
		if sawSettings {
			d.gap("this project's ALLOWED_HOSTS is not read from the environment, so devbay cannot add " +
				"the bay's hostname to it; add ${bay.web.public_host} yourself or a browser will get 400")
		}
		return
	}
	if web.Env == nil {
		web.Env = map[string]string{}
	}
	if _, set := web.Env[name]; set {
		return
	}
	// 127.0.0.1 and localhost stay in the list because that is the address
	// devbay's own health probe uses -- Go cannot resolve *.localhost -- and a
	// list holding only the bay hostname would make every boot fail.
	web.Env[name] = "${bay.web.public_host},localhost,127.0.0.1"
	d.note(SourcePython, "settings.py",
		fmt.Sprintf("Django: %s is read from the environment, so the bay's own hostname is added to it", name))
}

// dependOn makes the long-running services wait for a setup step.
//
// Without this the oneshot is in the manifest and nothing waits for it, so the
// application starts against an unmigrated database and fails in a way that
// has nothing to do with the real cause.
func (d *detector) dependOn(step string) {
	for name, s := range d.m.Services {
		if name == step || s.IsOneshot() {
			continue
		}
		if !contains(s.Needs, step) {
			s.Needs = append(s.Needs, step)
		}
	}
}

// grepRepo reports whether any Python file in the tree mentions a setting.
// Bounded to the files a settings module would live in, because this runs on
// repositories of unknown size.
func (d *detector) grepRepo(needle string) bool {
	found := false
	var seen int
	_ = filepath.WalkDir(d.dir, func(p string, de fs.DirEntry, err error) error {
		if err != nil || found || seen > 2000 {
			return nil
		}
		if de.IsDir() {
			switch de.Name() {
			case ".git", "node_modules", ".venv", "venv", "__pycache__", "static", "staticfiles":
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(p) != ".py" {
			return nil
		}
		seen++
		b, err := os.ReadFile(p)
		if err == nil && strings.Contains(string(b), needle) {
			found = true
		}
		return nil
	})
	return found
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// nonHTTPPorts are the registered ports of protocols that are not HTTP.
//
// Deliberately a list of known wire protocols rather than a list of known HTTP
// ports: an application can serve HTTP on anything, so the safe default for an
// unrecognised port is to assume HTTP and let the probe say otherwise. These
// are the ones where that assumption is known to be wrong.
var nonHTTPPorts = map[int]string{
	5432: "postgres", 3306: "mysql", 6379: "redis", 27017: "mongodb",
	5672: "amqp", 9092: "kafka", 11211: "memcached", 1025: "smtp",
	25: "smtp", 587: "smtp", 5044: "beats", 9300: "elasticsearch transport",
	2181: "zookeeper", 1433: "mssql", 5433: "postgres", 26257: "cockroach",
	7000: "cassandra", 9042: "cassandra", 6432: "pgbouncer", 22: "ssh",
}

func speaksHTTP(port int) bool {
	_, known := nonHTTPPorts[port]
	return !known
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
	// mongosh only exists from MongoDB 5.0; before that the shell is `mongo`,
	// and a probe that is absent from the image reads as a database that never
	// came up. A TCP probe is weaker and works on every version, which is the
	// better trade for something devbay guessed rather than read.
	{"mongo", manifest.Health{TCP: 27017}},
	{"rabbitmq", manifest.Health{Cmd: manifest.Argv{"rabbitmq-diagnostics", "ping"}}},
	{"elasticsearch", manifest.Health{HTTP: "/_cluster/health"}},
	{"opensearch", manifest.Health{HTTP: "/_cluster/health"}},
	{"minio", manifest.Health{HTTP: "/minio/health/live"}},
	{"mailpit", manifest.Health{HTTP: "/readyz"}},
	{"mailhog", manifest.Health{HTTP: "/"}},
	// Logstash's pipeline inputs are whatever the config says, so the only
	// port that reliably means "up" is its monitoring API.
	{"logstash", manifest.Health{TCP: 9600}},
	{"zookeeper", manifest.Health{TCP: 2181}},
	{"kafka", manifest.Health{TCP: 9092}},
	{"etcd", manifest.Health{TCP: 2379}},
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
				// A probe naming a port the service does not declare cannot
				// run: devbay publishes declared ports and probes the mapping,
				// so the probe would ask about a port that was never
				// forwarded. The port comes from the same evidence the probe
				// did -- it is that image family's own -- and a compose file
				// routinely leaves it out because nothing outside the stack
				// needs to reach the database.
				if h.TCP != 0 && s.Port == 0 {
					s.Port = h.TCP
					d.note(SourceConvention, "", fmt.Sprintf("port %d for %q from the same image family", h.TCP, name))
				}
				break
			}
		}
		if s.Health != nil {
			continue
		}
		if s.Port != 0 && !speaksHTTP(s.Port) {
			// A port devbay can name is a port it knows the protocol of, and
			// `GET /` against a wire protocol does not fail quickly or
			// clearly: it hangs until the timeout and reports EOF. A TCP probe
			// asks the only question that makes sense there.
			s.Health = &manifest.Health{TCP: s.Port, Timeout: "60s"}
			d.note(SourceConvention, "",
				fmt.Sprintf("health probe for %q is a TCP connect; %d is not an HTTP port", name, s.Port))
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
	if len(ported) == 0 {
		return
	}
	if len(ported) == 1 {
		return // inferred elsewhere; nothing to choose between
	}

	claim := func(name, why string) {
		d.m.Services[name].Primary = true
		d.note(SourceConvention, "", fmt.Sprintf("%q claims the bay hostname: %s", name, why))
	}

	// A datastore is never the primary. It has a port, it often sorts first,
	// and picking it points the bay's own hostname at postgres -- which is how
	// a developer opens their bay and gets a blank 502 from a database that is
	// working perfectly. This is a negative signal rather than a positive one
	// on purpose: devbay can recognise a database far more reliably than it
	// can recognise an application.
	var apps []string
	for _, name := range ported {
		if !datastore(d.m.Services[name].Image) {
			apps = append(apps, name)
		}
	}
	if len(apps) == 0 {
		// Every service is a datastore, so the developer is running a stack
		// with no application in it and any choice is arbitrary.
		claim(ported[0], "every service is a datastore")
		return
	}
	if len(apps) == 1 {
		claim(apps[0], "the only service that is not a datastore")
		return
	}

	// Nothing depends on the front of the stack. A proxy sits in front of the
	// application, the application in front of the database, and the thing a
	// browser should be pointed at is the one at the top -- which the
	// dependency graph already states.
	depended := map[string]bool{}
	for _, s := range d.m.Services {
		for _, n := range s.Needs {
			depended[n] = true
		}
	}
	var tops []string
	for _, name := range apps {
		if !depended[name] {
			tops = append(tops, name)
		}
	}
	if len(tops) == 1 {
		claim(tops[0], "nothing depends on it, so it is the front of the stack")
		return
	}
	if len(tops) > 1 {
		apps = tops
	}

	// A conventional web port beats a conventional name, because a repository
	// can call its web service anything and still serve it on 80.
	for _, want := range []int{80, 443, 8080, 3000, 8000, 5000, 4200, 5173} {
		for _, name := range apps {
			if d.m.Services[name].Port == want {
				claim(name, fmt.Sprintf("it serves on %d", want))
				return
			}
		}
	}
	for _, want := range []string{"web", "app", "frontend", "client", "ui", "api", "server", "proxy", "nginx"} {
		for _, name := range apps {
			if name == want {
				claim(name, "its name says so")
				return
			}
		}
	}
	d.m.Services[apps[0]].Primary = true
	d.gap("several services expose a port; %q was made primary, which may be wrong", apps[0])
}

// datastore reports whether an image is a database, cache or broker.
//
// Read off the same table that supplies their health probes: an image devbay
// knows how to ask "are you ready" is one it knows is not the application.
func datastore(image string) bool {
	base := imageBase(image)
	// Mailpit and minio are in the table too. They have browsable interfaces
	// and are still not what the developer opened the bay to look at, so they
	// are excluded on the same grounds as postgres.
	for _, k := range knownHealth {
		if strings.HasPrefix(base, k.prefix) {
			return true
		}
	}
	return false
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
		if s.Image == "" && s.Build == nil {
			d.gap("service %q has neither an image nor a build context", name)
		}
		// A stock datastore image starts its own server. Anything else that
		// was transcribed without a command will run its default entrypoint,
		// and the first symptom is a health probe that never passes.
		if !s.IsOneshot() && len(s.Start) == 0 && s.Image != "" && s.Build == nil && !startable(s.Image) {
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
