# Acceptance

The question this answers is "does devbay do the job", not "do the units pass".
It drives the real binary, against a real repository, doing what a developer
does, and it fails if any claim in [CAPABILITIES.md](CAPABILITIES.md) stops
being true.

```sh
make acceptance
```

A few minutes, needs Docker, and leaves nothing behind — including the
developer's own state, which it never reads or writes: the suite runs with its
own `HOME`, so a pass means something on a machine that already has bays
running. It is separate from `make test` because it is slow and because it is
asking a different question — the unit and integration suites check that the parts work; this
checks that the tool does.

## The scenarios

Each one states the claim, then what would have to be observed for the claim to
be false. Nothing here asserts an implementation detail: every check is
something a developer could see for themselves.

| # | Claim | Fails if |
|---|---|---|
| A | The binary runs and can diagnose the machine | `devbay doctor` reports a blocking problem |
| B | An unseen repository can be adopted | `init` produces a manifest that does not validate, or leaves a fixed host or port in any `env:` |
| C | A bay boots and serves | the primary service does not answer on its own hostname |
| D | Two bays are independent | both hostnames do not serve, data written in one is visible in the other, or a container cannot name its own bay |
| E | Feedback is fast | a `needs: []` task boots a container, or takes longer than five seconds |
| F | A failing test is actionable | the failure lacks a name, a file, a line or the assertion text |
| G | Only the declared subgraph is materialised | a task with `needs: [api]` starts a service the task did not ask for |
| H | Host edits reach containers | an edit in the bay's worktree is not served after `devbay watch` reports applying it |
| I | An agent sees what the CLI sees | MCP reports a different state, URL or task result than the CLI |
| J | Resting states are honest and reversible | cooling does not free the containers, thawing does not restore service, or the bay's checkout does not survive |
| K | It recovers from a restart | with the proxy container destroyed, a running bay's hostname does not come back |
| L | Work is never lost | a branch carrying commits is deleted by `devbay rm` |
| M | Teardown is total | any container, volume, network, built image or worktree survives `devbay rm` |
| N | Secrets do not leak | a planted credential appears in logs or in any MCP response |
| O | Undeclared egress is blocked | a service with no `egress:` reaches the internet, or cannot reach its own bay's peers |
| P | Emulators remove the need for credentials | a bay declaring `externals:` cannot exercise the dependency without a real key |
| Q | An unapproved command does not run | a command outside the allowlist executes without a human's approval, or a non-human caller can grant it |
| R | Seeding is paid for once | the second bay of a project re-runs the migration suite, restores incomplete data, or shares a database with the first |
| S | Bays stay within a budget | a new bay pushes the machine past the resident budget, or the focused bay is the one stopped |
| T | An unseen repository just works | `devbay init` then `devbay new` on a repository with no compose file and no manifest does not produce a serving bay without human edits |
| U | An unseen compose stack just works | a transcribed stack loses its secret file, its command, its restart policy or the volume that shields its dependencies |

Scenario J deliberately does not assert that application data survives. That is
the application's business: this example's cache declares no volume, so losing
its contents when it stops is the cache behaving correctly. An acceptance suite
that asserted otherwise would be testing redis.

## The corpus check

The scenarios above run against a repository devbay was written for, plus two
(T and U) it has never seen. Neither answers the question a developer actually
has: *will it work on mine?*

That is answered separately, by hand, against real repositories -- the forty
stacks in [docker/awesome-compose](https://github.com/docker/awesome-compose)
and a handful of real Procfile applications. Each one is measured against the
only fair baseline, which is what `docker compose up` does with the same
repository on the same machine at the same moment:

```sh
make build
git clone --depth 1 https://github.com/docker/awesome-compose /tmp/awesome-compose
for d in /tmp/awesome-compose/*/; do scripts/corpus.sh "$d"; done
```

Each line is one stack:

```
<stack> compose=<up|down> devbay=<up-CODE|boot-failed:...|no-serve-CODE>
```

The baseline matters more than it looks. Several of these stacks do not work
under compose on an arm64 Mac at all -- an image with no arm64 build, a service
that races a peer it does not depend on -- and counting those against devbay
sends the work in the wrong direction. The bar is that **devbay boots what
compose boots**, and where the two differ it is devbay's job to explain why.

### Where it stood on 2026-08-12

35 stacks, on an arm64 Mac with Docker Desktop: **devbay served 29, compose
23**, and **there is no stack compose runs that devbay does not**.

Six work under devbay and not compose, every one of them because compose binds
the ports the stack declares and something on the machine already held one —
which is the collision devbay exists to remove.

Six work under neither: `nginx-flask-mysql`, `nginx-nodejs-redis`,
`nginx-wsgi-flask`, `vuejs` and both `wasmedge-*` stacks. Their own builds or
their own services fail, identically under `docker compose build`. devbay runs
the same build and reports the same error.

Two of those 29 are **degraded** — `prometheus-grafana` and
`react-rust-postgres`. Each has one service that cannot come up: a config the
current Prometheus image rejects, and a cargo too old for a crate published
since. Compose leaves the rest of the stack running and so does devbay; the
difference is that devbay names the broken service, keeps its container and
its logs, and exits non-zero.

The number is a snapshot of one machine on one day, not a score. What it is
for is noticing when a change to `internal/introspect` makes things worse.

**Measure on an idle machine.** `corpus.sh` removes the proxy container before
each stack, because most of these stacks publish `:80` and the baseline would
otherwise report a port clash that has nothing to do with the stack. That is
correct for a measurement run and destructive to anything else using devbay at
the same time: a bay booted in another terminal loses its routes mid-test, and
the stack being measured at that moment records a 502 it would not otherwise
have. It reads as a real failure and it is not reproducible afterwards, which
is the worst kind of number to publish. Run the corpus with nothing else
touching Docker, and re-run any stack that fails before believing it.

### The regression check after the 2026-08-12 detection changes

Reading compose healthchecks, mapping `service_completed_successfully` to a
oneshot, folding CI services into the compose service they duplicate, and
finding tasks below the repository root all change what `init` writes, so the
corpus was consulted before and after. The full run was not repeated; two
narrower checks were, and both are cheap enough to repeat on any future change
to detection.

**Detection, all 37 stacks with a compose file.** `init` then `validate`, no
containers, comparing the old binary with the new one built from a worktree at
the previous commit:

| | before | after |
|---|---|---|
| services detected | 75 | 75 |
| tasks detected | 3 | 11 |
| stacks with at least one task | 3 | 9 |
| validation errors | 0 | 0 |

The services are unchanged, which is the point: the change was meant to read
more of what a repository already says, not to invent more of it.

**Boots, the eight stacks that carry a healthcheck.** Six were run in full,
including all five whose `depends_on` gates on `condition: service_healthy` —
the ones where transcribing a healthcheck wrongly turns into a stack that never
comes up. devbay served every one:

```
nginx-golang-postgres  compose=down  devbay=up-200
nginx-golang-mysql     compose=up    devbay=up-200
spring-postgres        compose=down  devbay=up-200
postgresql-pgadmin     compose=down  devbay=up-302
react-java-mysql       compose=up    devbay=up-200
nginx-aspnet-mysql     compose=up    devbay=up-200
```

Two of these are worth reading twice. `nginx-golang-postgres` ships
`test: ["CMD", "pg_isready"]`, the probe that answers on the unix socket while
the entrypoint's temporary init server is up and the real one is not listening
yet — so the racy healthcheck this release repairs is not hypothetical, it is
in the corpus. And `nginx-golang-mysql` writes its healthcheck as a shell
command substituting a secret file, which a manifest cannot express without
breaking R1; that one is declined, the reason is recorded in the generated
file, and the service falls back to the image-family probe and boots.

It is not in CI: it needs the network, most of an hour, and tens of gigabytes
of images. It is what to run before believing a change to `internal/introspect`
is an improvement, because that package cannot be judged against fixtures
written by the same person who wrote the package.

## The browser gate

One claim cannot be checked from Go, because it is a claim about a browser's
cookie jar. It is also the claim the whole design rests on, so it is written
out here and checked by hand.

Two bays of any application that sets a session cookie. Visit each of them
twice: once through the host ports, which is the `localhost:3001` world devbay
replaces, and once through the bay hostnames.

```
                             bay alpha              bay beta
  127.0.0.1:40160 / :41540   session=alpha-session  session=alpha-session   ← leaked
  <bay>.<project>.localhost  session=alpha-session  (none)                  ← isolated
```

The top row is the bug: browsers key cookies by host and ignore the port, so
two bays on the same loopback address share one jar and the second bay is
logged in as the first. Nothing about the two applications is wrong; nothing in
either log says anything. The bottom row is what a per-bay origin buys.

Checked on 2026-08-11 in Chrome, against a two-bay stack whose service reports
the `Cookie` header it received. The bay hostnames resolved with no setup — no
`/etc/hosts`, no resolver, no flags.

## What it deliberately does not check

Anything under "Known unfinished" in CAPABILITIES.md. Asserting behaviour that
does not exist yet produces a suite that is red for reasons nobody intends to
fix, and a red suite nobody trusts is worse than a smaller honest one.

## When a scenario fails

The failure names the claim, not the assertion. `E: a needs:[] task booted 4
containers` is a product regression with an obvious owner; `expected 0, got 4`
is a puzzle. That is the whole reason this suite is written separately from the
unit tests rather than folded into them.
