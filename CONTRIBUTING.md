# Contributing to devbay

Thanks for looking. This document is short and specific: it covers how to build
and test, and the four conventions that are load-bearing enough to be worth
stating.

## Building

devbay needs Go 1.26+ and a container runtime. Docker Desktop, OrbStack, and
Colima all work; `devbay doctor` will tell you what it found and what it thinks
of it.

```sh
make build          # ./bin/devbay
make test           # unit tests only, no containers, seconds
make test-all       # the full suite, including Docker integration tests
make check          # build + vet + race + full tests, what CI runs
```

`make test` passes `-short`, which skips every test that needs a container. Use
it while iterating. Use `make test-all` before opening a pull request — most of
the interesting bugs in this codebase have been in the parts that talk to
Docker, and they do not reproduce without it.

## Testing conventions

**Tests are named for the claim they make, not the function they call.**
`TestSecretsNeverReachTheModel`, not `TestPatch2`. When one fails, the name
should tell you what broke about the product, and the failure message should
tell you what was expected in the same terms.

**Every bug found becomes a permanent test.** The comment above such a test says
what went wrong and why the naive version was wrong, because the next person to
read it will otherwise "simplify" it back. There are a lot of these; they are
the most valuable part of the suite.

**Docker tests clean up after themselves and assert that they did.** Integration
tests snapshot container, volume, and network state and diff it afterwards.
A test that leaks a container is a failing test.

## The four conventions

**1. `egress:` is never authorable by a generator.** Anything that produces a
manifest — the deterministic detector, the model patcher, `devbay import` —
has its `egress:` stripped by `internal/verify` before the manifest reaches the
executor. If content from a repository could widen the network policy, an
injected instruction could widen the network policy. Do not add a code path
that carries `egress:` across that boundary, and do not "fix" the stripping
because a test repo needed a domain.

**2. Commands are argv arrays, always.** There is no `sh -c` anywhere near
manifest-derived content, and `sh` is deliberately absent from the `argv[0]`
allowlist. `[sh, -c, "curl … | sh"]` would pass every other rule and make R1
decorative.

**3. Secrets are references until spawn time, and scrubbed on the way out.**
Nothing puts a resolved credential in a manifest, a log returned to an agent, a
prompt, or an audit record. `internal/scrub` is the last line, not the only one.

**4. Teardown is total.** `devbay rm` reverses `devbay new`. A leaked database
fork, container, volume, network, or minted credential after teardown is a bug
at the same severity as a crash, and there is a test asserting each.

## Changing the manifest format

`spec/devbay.schema.json` is the source of truth for the format; the Go
validator reads its patterns from the same place so the two cannot drift. A
format change is three edits and a gate:

1. The schema, including the `description` fields — they explain *why* a rule
   exists and are read by both humans and models.
2. `internal/manifest` — types, parser, validator.
3. The five hand-written manifests in `testdata/repos/`, which are real repos
   at pinned commits rather than synthetic fixtures.

If more than one of those five needs a construct the new schema cannot express,
the schema is wrong. That gate is recorded in `spec/GATE.md` and it has already
forced one redesign.

## Commits and pull requests

Ordinary conventions: present tense, explain why rather than what, keep a
change to one concern. There is no CLA and no commit-message linter.

A pull request that changes behaviour should say which acceptance criterion or
constraint it serves, and include a test that fails without it.

## Reporting security issues

Do not open a public issue. See [SECURITY.md](SECURITY.md).
