# What devbay does, and what it does not

Written so you can tell in a minute whether it is the right tool, and so the
acceptance suite has something to be checked against. Every "does" below is
covered by a scenario in [ACCEPTANCE.md](ACCEPTANCE.md); every "does not" is a
decision, not a gap waiting to be filled — except the last section, which is
honestly unfinished work.

## The job

Run several instances of one repository at once, on your machine, each properly
isolated, without hand-writing the configuration — and let an agent drive it.

Everything below follows from that sentence.

## What it does

**Creates a bay: a named, runnable instance of a repo at a branch.** Its own
git worktree, containers, port block, hostname, browser origin, database and
environment. `devbay new` and `devbay rm` are the whole lifecycle.

**Writes the configuration for you, from what the repository already says.**
`devbay init` reads a compose file, GitHub Actions services, a Procfile,
`package.json`, framework conventions — in that order of trustworthiness — and
emits a `devbay.yaml` with a provenance comment for every line and an explicit
list of what it could not work out. It never silently guesses.

From a compose file it carries over the parts that decide whether the stack
runs, not just the parts that describe it: `secrets:` become file mounts,
`command:` becomes an argv array, `restart:` is preserved because compose files
use it where they cannot express a dependency, and the anonymous volume that
keeps a bind mount from hiding `node_modules` comes across too.

Without a compose file it infers the toolchain from what the repository
committed to — `.nvmrc`, `engines.node`, `.python-version`, `go.mod`,
`.tool-versions` — takes the install command from the lockfile rather than the
README, and puts the dependencies where both the install and the service can
see them. Where a repository declares that it reads its allowed-hosts setting
from the environment, the bay's own hostname is filled in.

**Wires services to each other by reference, not by literal.** A compose file
says `http://localhost:4000` and `postgres://…@db:5432/app`; both are wrong the
moment two instances exist. devbay rewrites them to `${bay.api.public_url}` and
`${bay.db.url}`, choosing between the browser plane and the container plane
from evidence rather than from variable names.

**Gives each bay its own browser origin.** `feature-x.myapp.localhost` rather
than `localhost:3001`. Browsers key cookies by host and ignore the port, so two
bays on different ports share one jar and the second is logged in as the first
— measured, not assumed: see the browser gate in
[ACCEPTANCE.md](ACCEPTANCE.md#the-browser-gate). This is the difference that is
hard to work around.

**Builds images from the repository.** `build:` with a context, dockerfile and
target, through BuildKit, honouring `.dockerignore`. The context is confined to
the worktree.

**Seeds a project's datastore once, not once per bay.** A service with
`fork: image` and `seed:` has its state captured into a per-project template
the first time it is built, and every later bay restores it and skips the
migration steps entirely. The template is keyed by the contents of the declared
sources, so changing a migration rebuilds it and a rebase does not.

**Materialises only what a task needs.** `needs: []` boots nothing, so a unit
run is milliseconds. `needs: [api]` starts the API and its dependencies and
nothing else.

**Returns typed test failures.** JUnit, Jest/Vitest JSON, `go test -json`,
pytest. A failure comes back as a name, a file, a line and an assertion — over
the CLI and over MCP.

**Applies host edits to running containers.** `devbay watch` watches on the
host, because the FUSE and virtiofs inotify patches were never merged and
polling inside containers costs real CPU per bay.

**Exposes all of it to an agent.** Seven stateless MCP tools over stdio. Every
tool takes an explicit bay id; there is no session state to lose.

**Keeps secrets out of everything an agent or a model can read.** References
until spawn time, scrubbed from logs, MCP responses, audit records and model
prompts. Minted credentials are revoked at teardown where the provider allows
it.

**Stands in for third-party services.** `externals:` expands into ordinary
services from a small catalogue — mailpit for SMTP, stripe-mock, minio for S3 —
each with its own hostname, health probe and teardown, so a bay can exercise
mail or object storage with no credentials at all.

**Refuses to run a command a human has not agreed to.** A repository may
declare `bin/dev` or `./scripts/setup.sh` -- committed, reviewable scripts that
no allowlist can anticipate -- and devbay will not execute one until a person
has read the exact argv and approved it. The approval is keyed by the project
and the whole argv array, so arguments nobody saw are a different command, and
it is remembered rather than asked again. A non-human caller cannot grant it.

**Confines each service's network to what it declares** (`DEVBAY_EGRESS=1`),
applied inside the container's own namespace, never the host's.

**Keeps the machine inside a budget.** Creating a bay beyond
`DEVBAY_MAX_BAYS` (five) cools the oldest resident bay of the project rather
than refusing or letting the machine swap. Cooling, not freezing, because
`docker pause` stops scheduling and not allocation. The focused bay is never
the one stopped.

**Runs what Docker runs.** devbay is an orchestration layer, so a stack that
comes up under `docker compose up` should come up here. That has consequences
it took a corpus of real repositories to see: a service that fails to become
healthy leaves the rest of the bay running and its own container in place to be
read, rather than taking the whole bay down with it; compose `labels:` are
passed through, because a proxy that discovers backends by label routes to
nothing without them; and a service may be handed the Docker daemon socket —
approval-gated, never by default — because a label-driven proxy, a container
manager and a test suite that starts its own containers are all things Docker
runs.

**Reverses itself completely.** `devbay rm` removes containers, volumes,
networks, built images, the worktree and the port block. A branch carrying
commits is kept, and it says so.

## What it does not do

**It is not a daemon.** Every command is a short-lived process; there is no
background supervision loop and nothing reacting to memory pressure between
commands. `devbay watch` is a foreground command you leave in a tab. What
scheduling exists happens when a command runs: creating a bay that would put
more than five (`DEVBAY_MAX_BAYS`) on the machine cools the oldest one that is
not focused, and says which.

**It does not run your code anywhere but here.** No cloud, no remote
environments, no state syncing between machines, no telemetry. This is a
decision, not a roadmap item: Codespaces and Gitpod occupy that space, and the
defensible position is being local, open, and safe to hand credentials.

**It does not deploy anything.** No staging, no preview environments, no CI
runner. A bay is a development environment that dies when you are done.

**It does not manage your branches.** It creates a worktree on a branch and
hands it back. Merging, rebasing and deleting are git's job and yours.

**It does not decide what your tests are.** `devbay init` will say it could not
find a test command rather than invent one, because a wrong test command is
worse than none — an agent trusts a green result.

**It does not author network policy.** `egress:` is stripped from anything a
detector or a model produces. If configuration derived from repository content
could widen the network policy, then repository content could widen it.

**It does not hide the manifest.** `devbay.yaml` is committed, readable and
reviewable in a pull request. A generated file you cannot audit is worse than
no generated file, because you will run it anyway.

**It does not support multi-repo or multi-machine bays.** One repository, one
machine, for now.

## Limits worth knowing before you rely on it

These are properties of the world, not bugs, and each one changed the design.

**`*.localhost` only resolves in browsers.** Chrome, Firefox and curl handle it;
Go, Node, Python and Safari (before macOS 26) do not. So devbay gives code the
`127.0.0.1:<port>` address and reserves hostnames for browsers. `devbay url`
prints both and says which is which.

**Frameworks keep their own list of hostnames they will answer for.** Django's
`ALLOWED_HOSTS`, Rails' `config.hosts`, Vite's `server.allowedHosts`. A bay
hostname is not on it, so the application returns 400 to a browser while
answering devbay's own probe — which uses `127.0.0.1` — perfectly. devbay
fills the setting in where the repository says it reads it from the
environment, and where it cannot, the boot says which setting to change. It is
the one failure that makes a working bay look broken.

**A health probe asks whether the service is up, not whether a path exists.**
An HTTP probe treats anything short of a server error as ready, because the
path is usually one devbay chose rather than read, and an API that serves
`/users` and nothing at `/` is working. A 5xx is still a failure — that is the
server saying it cannot serve.

**A published port accepts before the service listens.** Docker's userland
proxy binds the host port when the container is created, so a plain connect
probe reports a database ready while it is still running initdb. devbay's
`tcp:` probe holds the connection briefly and reads: an immediate hangup is the
forwarder, not a server. It is worth knowing because a `cmd:` probe has the
same trap in reverse -- `pg_isready` answers over the unix socket while
postgres is still refusing every TCP client.

**Seeded templates are crash-consistent.** The capture pauses the datastore
rather than stopping it, so the restored copy is what a power cut would leave
and the engine recovers it on start -- visible as one "automatic recovery in
progress" line in a new bay's log. Stopping the datastore instead would give a
cleaner copy at the cost of restarting a service the developer is already
using, which is a worse trade for a cache.

**A partly-applied egress policy fails closed.** The chain's default policy is
set to DROP before any rule is written, so a sidecar that dies mid-script
leaves the service with no outbound network rather than an unrestricted one.
The opposite ordering is the natural one to write and is silently wrong.

**Freezing does not free memory.** `docker pause` stops scheduling, not
allocation — measured at 30.7 MiB running, 30.0 MiB frozen. Only `devbay cool`
returns memory. On Docker Desktop with Apple's Virtualization.framework, even
then the VM keeps it; `devbay doctor` says so and recommends OrbStack.

**Ports are not guaranteed stable across machines.** They are deterministic per
project and branch, persisted, and probed for conflicts — but a collision moves
them, correctly.

**Built images are shared between bays on the same commit.** Content-addressed
tags mean two bays on one commit reuse one image, and the last teardown removes
it.

## Known unfinished

Stated plainly rather than left to be discovered.

- **Nothing reacts between commands.** The resident budget is applied when a
  bay is created, so five bays left running overnight stay running. There is no
  process watching, and adding one would make devbay a daemon.
- **A bay that half-boots is reported, not repaired.** The working services
  serve, the broken one keeps its container and its logs, and `devbay new`
  exits non-zero. Nothing tries to fix it.
- **No supervision surface.** `supervision:` is refused rather than ignored.
  Favicon tinting and a staleness banner need the proxy to transform
  responses, which means shipping a custom Caddy build rather than configuring
  a stock one -- a distribution burden that has not earned itself yet. Telling
  five browser tabs apart relies on the page, which is why every container is
  given `DEVBAY_BAY`.
- **Services cannot be shared between bays.** `scope: shared` is refused rather
  than ignored, and `fork:` — which only means anything for a shared service —
  is reported as inert. Every service runs once per bay.
- **AWS STS minting is not implemented.** GitHub App tokens are.
