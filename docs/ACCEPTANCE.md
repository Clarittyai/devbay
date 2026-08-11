# Acceptance

The question this answers is "does devbay do the job", not "do the units pass".
It drives the real binary, against a real repository, doing what a developer
does, and it fails if any claim in [CAPABILITIES.md](CAPABILITIES.md) stops
being true.

```sh
make acceptance
```

Roughly three minutes, needs Docker, and leaves nothing behind. It is separate
from `make test` because it is slow and because it is asking a different
question — the unit and integration suites check that the parts work; this
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

Scenario J deliberately does not assert that application data survives. That is
the application's business: this example's cache declares no volume, so losing
its contents when it stops is the cache behaving correctly. An acceptance suite
that asserted otherwise would be testing redis.

## What it deliberately does not check

Anything under "Known unfinished" in CAPABILITIES.md. Asserting behaviour that
does not exist yet produces a suite that is red for reasons nobody intends to
fix, and a red suite nobody trusts is worse than a smaller honest one.

## When a scenario fails

The failure names the claim, not the assertion. `E: a needs:[] task booted 4
containers` is a product regression with an obvious owner; `expected 0, got 4`
is a puzzle. That is the whole reason this suite is written separately from the
unit tests rather than folded into them.
