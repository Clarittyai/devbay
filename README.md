# devbay

**Every branch gets its own containers, database, ports and browser origin — so
you can run five at once and they cannot touch each other.**

[![CI](https://github.com/Clarittyai/devbay/actions/workflows/ci.yml/badge.svg)](https://github.com/Clarittyai/devbay/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/Clarittyai/devbay.svg)](https://pkg.go.dev/github.com/Clarittyai/devbay)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Here is the bug that explains the whole tool. Two bays of one app, one cookie
jar — because a browser keys cookies by host and ignores the port:

```console
$ curl -c jar -b jar http://127.0.0.1:40160/login
logged in to alpha
$ curl -c jar -b jar http://127.0.0.1:41540/
bay=beta host=127.0.0.1:41540 cookie=session=alpha-session   ← beta has alpha's session
```

The same two bays, the same jar, reached on the hostnames devbay gives them:

```console
$ curl -c jar -b jar http://alpha.cookies.localhost/login
logged in to alpha
$ curl -c jar -b jar http://beta.cookies.localhost/
bay=beta host=beta.cookies.localhost cookie=(none)           ← kept apart
```

Run it yourself: `sh demo/cookie-jar.sh` boots both bays and prints exactly
that. ([demo/](demo) — the scripts are also recordings.)

```sh
curl -fsSL devbay.claritty.ai/install | sh
```

```sh
devbay init                  # read the repo, propose a devbay.yaml
devbay new add-search        # a bay: worktree, containers, database, hostname
devbay run add-search unit   # typed failures, not stdout to scrape
devbay url add-search        # open it
devbay rm add-search         # and nothing is left behind
```

Driven by that CLI, or by an agent over MCP — ten tools, and it connects to
Claude Code, Cursor and Codex.

---

Checked against forty real compose stacks and a set of real Procfile applications — each one compared with what `docker compose up` does with the same repository on the same machine, because that is the only baseline worth measuring against. Measured on 2026-08-12 against v0.5.1, devbay served 29 of 35 and compose 23, with nothing compose runs that devbay does not. That figure predates the detection changes in v0.5.2 and is being re-measured; the two narrower checks that were run against v0.5.2 are in [docs/ACCEPTANCE.md](docs/ACCEPTANCE.md#the-regression-check-after-the-2026-08-12-detection-changes). `scripts/corpus.sh` runs the full thing — on an idle machine, for the reason recorded there.

**What it does and does not do:** [docs/CAPABILITIES.md](docs/CAPABILITIES.md)
— including the limits and the unfinished parts. **Whether it actually does
it:** [docs/ACCEPTANCE.md](docs/ACCEPTANCE.md), and `make acceptance` to check.

## The problem

Coding agents got fast at producing code. The bottleneck moved to **verification**: an agent can write a change in three minutes and has no idea whether it works.

Git worktrees solve half of this. They isolate *code state*. They do not isolate *execution state* — the moment you run two branches side by side you hit port collisions, shared mutable data, caches that hide missing dependencies, and a setup ritual per worktree.

A **bay** is a named, isolated, runnable instance of a repo at a branch: its own worktree, containers, ports, browser origin, database, and environment. Agents drive it over MCP and get typed results back, not stdout to scrape.

## What's different

**Per-bay browser origins.** Browsers scope cookies by host and ignore the port, so `localhost:3000` and `localhost:3001` share one cookie jar and two bays clobber each other's sessions. Giving each bay its own origin eliminates that class of bug rather than working around it.

**Configuration is verified, not guessed.** A generated manifest is booted for real and probed. Failures feed back and get patched, bounded. Zero config authoring happens not because detection is perfect but because recovery from imperfect detection is automated.

**Tasks declare their own service subgraph.** Because an agent calls `run_task(bay, "unit")`, devbay knows what is about to happen and materializes only what that task needs. Unit tests boot zero containers.

## The manifest

`devbay.yaml` is committed to the repo, hand-editable, and reviewable in a pull request. It is designed so that a language model can author it safely:

| Rule | |
|---|---|
| **R1** | Commands are argv arrays, never shell strings. Executed with `execve`, not `sh -c`. There is no path from a manifest to arbitrary shell. |
| **R2** | `argv[0]` outside a default allowlist is permitted, but needs one-time human approval showing the exact argv. |
| **R3** | Secrets are references (`${secret:path}`). Literal credentials are rejected by prefix and by entropy. |
| **R4** | Egress is declared per service; anything undeclared is blocked. **This key is never authorable by the introspection agent** — otherwise an injection just adds its own destination. |
| **R5** | Every long-running service declares a health probe. Without one there is no verification loop, and without a verification loop generated config is only a guess that happened to parse. |
| **R6** | Every task declares `needs`. Empty is valid and common; omitted is an error. |
| **R7** | Interpolation is limited to `${bay.<service>.<field>}` and `${secret:<path>}`. |

Schema: [`spec/devbay.schema.json`](spec/devbay.schema.json). Design rationale for every construct lives in the schema's `description` fields.

### Address planes

The one detail worth reading twice. Each service has three addresses, and they are not interchangeable:

```yaml
env:
  DATABASE_URL:        ${bay.db.url}          # container → container
  NEXT_PUBLIC_API_URL: ${bay.api.public_url}  # browser → container
```

`url` is the container-network address when injected into a container, and `127.0.0.1:<port>` when handed to the host or to an agent. `public_url` is the browser-facing origin. Server-side variables want `url`; anything the browser sees wants `public_url`.

This matters because **`*.localhost` does not resolve outside browsers**. Verified on macOS 14: `getaddrinfo` fails at every depth, so Node, Go, Python and Safari all get `ENOTFOUND`; only Chrome, Firefox and curl special-case it. A health probe or an SSR fetch aimed at a `.localhost` hostname would simply fail.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/Clarittyai/devbay/main/install.sh | sh
```

One static binary, no Go toolchain, no sudo if `~/.local/bin` is on your PATH.
The script checks the download against the published checksums and tells you
where it put things.

<details>
<summary>Other ways</summary>

From source, if you have Go 1.26 or newer:

```sh
go install github.com/Clarittyai/devbay/cmd/devbay@latest
```

By hand, from [releases](https://github.com/Clarittyai/devbay/releases) — pick
the archive for your platform, then:

```sh
shasum -a 256 -c checksums.txt --ignore-missing
tar -xzf devbay_*_darwin_arm64.tar.gz
install -m 0755 devbay ~/.local/bin/
```

Pin a version with `DEVBAY_VERSION=v0.1.0`, or choose the destination with
`DEVBAY_INSTALL_DIR=/opt/bin`.

</details>

You also need a container runtime — Docker Desktop, OrbStack, or Colima. Then:

```sh
devbay doctor
```

It reports what it found and what it thinks of it, including the memory and
disk settings behind most "it got slow after a few bays" reports. Nothing else
needs configuring.

## Try it

There is a complete example in [`examples/taskboard`](examples/taskboard) — a
browser client, an API, a cache and a database, with no package installs, that
boots in seconds:

```sh
cd examples/taskboard && git init && git add -A && git commit -m taskboard
devbay init && devbay new my-change
```

Against your own repository:

```
git clone https://github.com/Clarittyai/devbay && cd devbay
make build

bin/devbay mcp install                # wire it into Claude Code, Cursor and Codex
bin/devbay doctor                     # is this machine set up well?
bin/devbay init                       # propose a devbay.yaml for this repo
bin/devbay init --verify              # …and prove it boots before writing it
bin/devbay validate .                 # check it against R1-R7
bin/devbay approve                    # read and allow any command outside the allowlist

bin/devbay new add-oauth --alias oauth
bin/devbay ls
bin/devbay run add-oauth unit
bin/devbay url add-oauth
bin/devbay rm add-oauth
```

Two bays of the same project run side by side on their own ports, their own
datastores, and their own browser origins:

```
BAY            ALIAS        STATE    BRANCH        URL
add-oauth      oauth        warm     add-oauth     http://add-oauth.demoapp.localhost
fix-login      login        warm     fix-login     http://fix-login.demoapp.localhost
```

Tests:

```
go test ./...                # unit tests plus real bays, if Docker is running
go test -short ./...         # skip anything needing Docker
go test -race ./...          # the concurrency the tool exists for
```

323 tests across 19 packages, race-clean — the count `go test ./... -list '.*'` reports.

The suite is arranged so that the parts which can be tested without Docker are,
and the parts that cannot are tested against real containers rather than mocks:

| Layer | What it proves |
|---|---|
| `internal/manifest` | 32 rule violations are rejected, and the five hand-written fixtures still validate |
| `internal/report` | parsers built against **captured output** from real pytest and `go test` runs |
| `internal/scrub` | planted canaries never survive; ordinary logs come back byte-identical |
| `internal/ports` | 400 bays share no port; a real hash collision resolves; a real listener is detected |
| `internal/worktree` | adoption, atomic unwind, and that include patterns cannot escape the repo |
| `internal/proxy` | two bays keep separate cookie jars — the reason the project exists |
| `internal/engine` | five probe forms, teardown audits, and that freezing does **not** reclaim memory |
| `internal/introspect` | detection is deterministic, and never writes an egress allowlist |
| `internal/egress` | a service with no declared egress genuinely cannot reach the internet |
| `internal/broker` | minted credentials are revoked on teardown, and the audit log never holds a value |
| `internal/verify` | the airlock strips egress from every proposal, including patched ones |
| `internal/e2e` | the whole stack, driven as a user and as an agent, including failure paths |

Failure paths get as much attention as success paths, because a half-created bay
is invisible: it holds a branch, a port block and possibly containers, and
nothing lists it. Every failure test asserts that nothing was left behind, not
merely that an error was returned.

## Editing while it runs

```sh
devbay watch <bay>
```

Applies edits made in the bay's worktree to its containers: restart, rebuild,
or nothing, per the service's `watch_action`.

devbay does the watching on the host on purpose. The FUSE and virtiofs inotify
patches were never merged, so a file edited outside a container does not
reliably produce an inotify event inside it — which is why every guide tells
you to set `CHOKIDAR_USEPOLLING`, and why five bays of a JavaScript monorepo
spin a fan for nothing. Watching where the events actually work costs one
process per bay you are editing, and no polling anywhere.

Each bay is its own checkout, so the files to edit are the ones in that bay's
worktree — `devbay status <bay>` prints the path, and `devbay watch` prints it
when it starts.

## Generating a manifest

`devbay init` reads what the repository already says about itself, in order of
how much each source actually knows: an existing compose file or devcontainer
first (that is transcription, not detection), then GitHub Actions `services:`
blocks — which carry health commands in their `options:` and cannot silently
rot, because CI fails when they are wrong — then Procfiles, package manifests
and framework conventions.

It writes a file that leads with its own evidence and its own gaps:

```yaml
# Where this came from:
#   github-actions   .github/workflows/pull-db-tests.yml — service "pgsql" from image postgres:14@sha256:2f4394…
#   convention       health probe for "pgsql" from the postgres image family
#
# STILL TO DECIDE — devbay could not work these out:
#   - Go services need an explicit `start:` command; devbay cannot tell which cmd/ package is the server
#   - several services expose a port; "azurite" was made primary, which may be wrong
```

Two rules it does not bend. It never writes an `egress:` allowlist — if
configuration derived from repository content could widen the network policy,
then repository content could widen the network policy. And it never emits an
image it cannot resolve: a tag eaten by an unset variable would silently mean
`:latest`, so the service is skipped and the reason recorded.

### Verified, not guessed

`devbay init --verify` boots the proposal in a throwaway bay before writing it,
so the file you get is one that demonstrably works — or one that tells you
exactly how it failed:

```
unverified the proposal did not boot
  boot web: container exited with code 0 before becoming healthy
```

Detection is imperfect and always will be. The developer does zero work not
because detection is perfect, but because recovery from imperfect detection is
automated. A `Patcher` can be plugged into the same loop to repair a failure
from what it printed, bounded at three attempts — after which the partial
manifest and the last failure go to the human, who is usually four lines from a
working file.

Every proposal crosses one checkpoint before anything runs it, including the
first, deterministic one — it read the same repository as everything else. It
must parse and pass the full validator, and `egress:` is stripped
unconditionally. A patched proposal has additionally seen the error output of
code from that repository, which makes it the more dangerous of the two, so it
is stripped again.

## For agents

`devbay mcp` speaks the Model Context Protocol on stdio, to both the current
spec and the handshake every shipping client still opens with. Three tools get
a repository ready — `repo_status`, `repo_init`, `manifest_validate` — because
an agent that has to shell out to a CLI to set devbay up is reading terminal
output again, which is the thing this interface exists to stop. Approval is
deliberately not among them: R2 is a checkpoint for a human, and an agent that
could approve its own commands would be holding both halves of it.

Then seven for driving bays:
`bay_create`, `bay_list`, `bay_run_task`, `bay_logs`, `bay_url`, `bay_status`,
`bay_destroy`.

`bay_run_task` is the one that matters. It starts only the services the task
declares it needs, runs it, and returns typed results:

```json
{
  "task": "failing", "exit_code": 1, "duration_ms": 214,
  "total": 2, "passed": 1, "failed": 1, "parsed": true,
  "failures": [
    {"name": "test_subtraction", "file": "suite.py", "line": 42,
     "message": "assert 5 - 3 == 1"}
  ]
}
```

An agent that receives that can open the file and fix the line. An agent that
receives stdout has to guess at the runner's format, and it guesses differently
each time.

The protocol core is stateless per the 2026-07-28 spec, so every tool takes an
explicit bay name rather than inferring one from the connection.

## Repository layout

```
spec/                    the published schema, and a Python reference validator
  devbay.schema.json     single source of truth; the Go validator reads its patterns
  GATE.md                what the five-repo spec gate found, and what it changed
internal/manifest/       parser and validator — the airlock
internal/worktree/       git worktrees; adopts an agent's rather than duplicating it
internal/engine/         address planes, boot plans, Docker lifecycle, health probes
internal/ports/          deterministic allocation with collision probing
internal/proxy/          Caddy, per-bay hostnames and browser origins
internal/report/         JUnit / Jest / go-json parsers -> typed failures
internal/scrub/          secret removal at the boundary
internal/bay/            orchestration: worktree + ports + containers + routes
internal/mcp/            the agent interface
internal/e2e/            the whole stack, driven as a user and an agent do
testdata/repos/          five hand-written manifests for five real repos
cmd/devbay/              CLI
```

## How a bay comes up

Services start in dependency waves: everything at the same depth starts at once, and a wave finishes when every service in it is healthy. A one-shot finishes by exiting zero; a long-running service finishes when its probe passes. The four-service test bay comes up in about a second.

Health probes always run from the host against `127.0.0.1:<published port>`, never against a bay hostname — the daemon's own resolver cannot resolve those. Published ports bind to loopback only, so no bay is reachable at `127.0.0.1:<port>` from anywhere but this machine. Routes are published last, so a hostname never answers 502 while the bay is still coming up.

The proxy is the deliberate exception, and worth knowing about: it binds `:80` on **all** interfaces, because a bay URL that cannot be opened from a phone or a simulator is not much of a URL. So anything that can reach your machine on port 80 can reach a bay by asking for its hostname. On a home or office network that is the point; on a conference or café network it is not what you want, and `devbay doctor` says so.

Teardown is a label query, not a list of remembered objects. Everything devbay creates carries `dev.devbay.*` labels and `Down` removes everything matching, which is the only approach that is still correct after a crash or a partial boot. An automated audit asserts zero orphaned containers, volumes, or networks.

## States, and an honest cost model

| State | Mechanism | CPU | Memory | Back to warm |
|---|---|---|---|---|
| `hot` | running, holds the canonical hostname | full | full | — |
| `warm` | running at its own hostname | full | full | — |
| `frozen` | `docker pause` | ~0 | **unchanged** | ~30 ms |
| `cold` | `docker stop`, volumes kept | 0 | released | full boot |

**Freezing does not reclaim memory.** The cgroup freezer stops scheduling, not allocation. This is measured rather than asserted — a test records all three states and fails if the numbers ever contradict the table:

```
running 30.7 MiB | frozen 30.0 MiB | cold 0.0 MiB
```

So a scheduler under memory pressure must demote to `cold`, not `frozen`. Freezing is still worth having: resume is ~30 ms with no state lost and no re-probing, because the processes never exited.

One further caveat on macOS: even `cold` only returns memory to the Linux VM. Under Apple's Virtualization.framework the VM does not hand it back to macOS, so host-level reclamation needs OrbStack or Docker's own VMM. Memory is therefore measured *inside* the VM — the host-side number is the VM's own footprint and says almost nothing about any individual bay.

## Ports and hostnames

Port allocation is deterministic so a bookmarked URL survives a restart, but the hash is the **first guess, not the answer**: it is confirmed against persisted state and against the host, then probed forward when either objects. Hashing alone collides — with 90 buckets and five bays, better than a one-in-ten chance, and a collision is total.

The proxy runs as a container so Docker's already-root helper performs the privileged `:80` bind; devbay itself never needs sudo. It joins each bay's network rather than reaching services through the host, because published ports are loopback-bound and a container cannot reach the host's loopback.

An unrouted hostname gets an explicit 404 rather than Caddy's default blank 200 — a torn-down bay's URL should say the bay is gone, not look like a broken app.

## Design notes

The format was not designed and then tested. It was written by hand against five dissimilar real repositories — a Node monorepo, a Rails app, a Django app, a Go service, and a FastAPI stack — and four of the five needed a construct the first draft could not express. [`spec/GATE.md`](spec/GATE.md) records what broke and why the fixes are the shape they are. Notable results:

- A `setup:` list of commands is the wrong primitive; ordering-sensitive steps have to be first-class one-shot services, because migrations must finish *before* the app starts, not after everything is healthy.
- Real repos contain processes with no port and nothing to probe — Sidekiq, Celery, asset watchers. Forcing them to declare an HTTP probe produces a fake one, which is worse than an honest weak one.
- Test wrappers like `start-server-and-test` become unnecessary once `needs` exists, so the manifest removes work rather than adding it.

## Secrets

devbay is a consumer of secret managers, not a replacement for one. The
ecosystem converged on `<tool> run -- <command>` years ago — `op run`,
`sops exec-env`, `dotenvx run`, `infisical run` all inject values into a
subprocess and hold nothing — so `op run -- devbay run mybay unit` already
works, via an environment fallback (`DEVBAY_SECRET_<REF>`). A command source
covers the rest: `["op", "read", "op://{ref}"]`, configured by the developer,
never by a manifest.

What none of them do is tie a credential's lifetime to an environment's. A
long-lived key handed to a bay a coding agent drives outlives every mistake
made with it. So where a provider can mint something short-lived and scoped,
devbay mints one **per bay** and revokes it when the bay is destroyed:

```
bay: GitHub tokens will be minted per bay and revoked on teardown
  revoked github (github-app)
```

GitHub App installation tokens are implemented first because they are the one
common case where the whole story works: one hour, narrowable to specific
repositories and permissions, and — unusually — genuinely revocable. AWS STS,
by contrast, has no per-session revocation at all, only a policy that
invalidates every session issued before now.

Every grant and revocation is appended to `~/.devbay/audit.jsonl`, so you can
answer "what was this environment given, and when" months later. **The value is
never recorded** — a log that answered the question by storing the credentials
would be a worse leak than the one it exists to detect. There is a test that
plants one and asserts it never appears.

## Network policy

With `DEVBAY_EGRESS=1`, a service reaches only what its manifest declares, and
a service that declares nothing reaches nothing. It matters most during
dependency installation, where a package lifecycle script runs code from a
repository that was just cloned — which is exactly what the self-replicating
npm worms use.

Two implementation notes explain the shape of it.

Docker's `internal` network would be a simpler mechanism and cannot be used:
a container on one **cannot publish a port**, and devbay publishes ports so the
daemon can health-probe a service. That was measured, not assumed. So filtering
happens inside each container's own network namespace, applied by a short-lived
sidecar that joins it and exits — never against the VM-wide chain, where a
broad `OUTPUT DROP` takes out the Docker VM's own DHCP.

Only the container's **own subnet** is permitted, not private address space in
general. Allowing RFC1918 wholesale looks harmless and is not: Docker's bridges
live in `172.16/12`, so it would silently permit every other bay on the machine
and the developer's LAN. The gateway is then rejected ahead of the subnet it
belongs to, because it is the route to everything else. An earlier version
allowed all of RFC1918 and its test passed — against a peer container that was
never off-subnet in the first place.

Install steps run behind a placeholder command so the policy is in force before
any repository code executes, rather than racing it.

This reduces blast radius. It is **not** a containment boundary: allowlisting
resolves names to addresses, so a name on a shared CDN address permits
everything else on that address.

## Security posture

devbay reduces blast radius. It is **not** a malware containment boundary and must not be described as one.

The threat model is precise: a language model writes boot instructions, with access to credentials, while reading content an attacker can influence — repo files, CI configs, dependency manifests, third-party error logs. Those three things are kept apart by an *authoring plane* with no credentials and no execution, an *execution plane* with no model, and schema validation as the only path between them.

Schema validation alone is not a security boundary — a manifest can be perfectly valid and still hostile. What makes the airlock real is capability restriction: a closed command vocabulary, digest-pinned images, `--ignore-scripts` by default on installs, and an egress allowlist the model cannot write.

## License

Apache-2.0.
