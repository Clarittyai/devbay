# Security policy

## Reporting a vulnerability

Please report privately, not in a public issue: use GitHub's **Report a
vulnerability** button under this repository's Security tab, which opens a
private advisory visible only to the maintainers.

Include what you did, what happened, and what you expected. A proof of concept
helps enormously; a repository that reproduces it helps more. You will get an
acknowledgement within a few days, and a fix or an explanation of why it is not
a vulnerability before any public disclosure.

## What devbay is defending

devbay runs untrusted repository content on a developer's machine, sometimes
with real credentials attached. Two boundaries carry almost all of the weight,
and a report that crosses either is serious even if it looks minor.

### 1. The airlock — generated configuration cannot escalate

A `devbay.yaml` may be produced by a detector or a language model that has read
the repository: its README, its CI config, its dependency manifests, and the
error output of its own code. All of that is attacker-influenceable. So a
proposal crosses one checkpoint before anything executes it (`internal/verify`):

- It must parse strictly and pass the full validator. Commands are argv arrays,
  so a shell string fails at decode, before any rule runs. `sh` is not in the
  `argv[0]` allowlist.
- `egress:` is removed from every proposal, unconditionally and without
  exception. Network policy is not something generated content gets to widen.
- Environment values are references, never literals. A credential in a manifest
  is a validation error.

**In scope:** any way to get an argv, an image, an environment literal, a mount,
or a network rule past that checkpoint that the validator was supposed to
reject. Any prompt injection that changes what devbay *executes*, as opposed to
what it merely proposes.

**Out of scope:** a model proposing a wrong-but-valid manifest. That is a
quality problem, and the verify loop exists because it is expected.

### 2. Secrets never enter model context or agent output

Credentials are references until spawn time and are scrubbed on the way out —
from logs returned over MCP, from the audit log, and from anything sent to a
model (`internal/scrub`, `internal/broker`, `internal/patch`).

**In scope:** any path by which a resolved credential reaches a model prompt, an
MCP response, a log line, an audit record, a manifest, or a bay that was not
granted it. Any minted credential that survives `devbay rm`.

### Also in scope

- Escaping a bay's isolation: reaching another bay's containers, database, or
  network; reading files outside the worktree; a path in an include pattern that
  escapes the repository.
- Anything that makes teardown incomplete — a container, volume, network,
  database fork, or credential left behind after `devbay rm`.
- Privilege escalation on the host from inside a bay.

### Out of scope

- Vulnerabilities in the images a manifest names. devbay pins by digest and
  allowlists registries; what is *inside* `postgres:16` is upstream's.
- The container runtime itself (Docker, OrbStack, Colima) — report those
  upstream.
- Attacks requiring the developer to run a manifest they have already read and
  approved. devbay makes the manifest reviewable on purpose; a human who
  approves `[bash, scripts/setup.sh]` has approved that script.
- Denial of service through resource exhaustion on the local machine.

## Supported versions

devbay is pre-1.0. Fixes land on `main` and in the next release; there is no
backport branch yet.

## Design notes

The reasoning behind these boundaries is in the README and in
`spec/devbay.schema.json`, where each rule's `description` says why it exists.
`CONTRIBUTING.md` lists the four conventions a change must not break.
