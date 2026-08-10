# M−1 spec gate — verdict

**Verdict: PASS, after redesign. Proceed to M0.**

The gate condition was: *write `devbay.yaml` by hand for five dissimilar real repos; if more than one of five needs a construct the schema cannot express, redesign the schema before writing a daemon.*

Four of the five needed such a construct. The schema was redesigned. All five now validate, and 23 rule-violation cases are rejected. This is the gate working, not the gate failing — the whole point of spending a week on YAML was to find these before a month of Go encoded them.

## What was tested

Five real repositories, cloned and read at pinned commits (see each `pin.json`), not written from memory:

| Fixture | Stack | Why it was chosen |
|---|---|---|
| `documenso` | npm workspaces + turbo, React Router SSR, Prisma/Postgres, Redis, MinIO, SMTP | monorepo; multi-port services; requires install scripts |
| `mastodon` | Rails 7, Procfile with 4 processes, Postgres, Redis | two processes with no port and nothing to probe |
| `saleor` | Django + GraphQL, uv, Postgres, Celery | Python toolchain; a third no-port worker; CI already emits JUnit |
| `gitea` | Go + Vite frontend, Postgres, Elasticsearch | compiled language; streaming test output; CI already digest-pins |
| `fastapi-template` | FastAPI, Postgres, React/Vite, mail catcher | ships the exact ordering pattern the schema needed |

Reproduce:

```
python spec/validate.py testdata/repos/*/devbay.yaml   # 5 OK, 2 approval warnings
python spec/test_rules.py                              # 23/23 rules enforced
```

## Findings that changed the schema

### G1 — a service can expose more than one port · 3 of 5 repos

`port` was a single integer. MinIO serves an S3 API on 9002 and a console on 9001; a mail catcher serves SMTP and a web UI; Traefik serves proxy and dashboard. There was no way to write any of them.

**Fix:** `port` remains the single *primary* port — the one that gets a hostname and is probed — so routing stays unambiguous. Added `ports:`, a name→number map, routable as `<name>.<service>.<bay>.<project>.localhost` and referenceable as `${bay.<svc>.ports.<name>}`.

### G2 — real repos contain processes with nothing to probe · 3 of 5 repos

R5 made `health` mandatory and offered only `http`, `tcp`, and `cmd`. Mastodon's Sidekiq worker and Vite asset watcher, and Saleor's Celery worker, have no port, no HTTP surface, and no readiness command. The only way to satisfy R5 was to invent a probe that does not exist — which would report healthy while the worker was dead, silently defeating the verification loop that R5 exists to guarantee.

**Fix:** two more probe forms, in order of preference.

- `log: <RE2 regex>` — healthy on first match against stdout/stderr. This turned out to be *better* than an HTTP probe for most dev servers, not merely a fallback: Vite prints `ready in 412 ms`, Sidekiq prints `Starting processing, hit Ctrl-C to stop`, Celery prints `celery@host ready.`. Matching the ready line the process already emits is genuine readiness.
- `process: true` — liveness only, and the validator warns on it. Kept because an honest weak probe beats a dishonest strong one.

R5 is unchanged in force: a long-running service still cannot omit `health`.

### G3 — ordering-sensitive setup was unexpressible · 5 of 5 repos

`setup:` was a flat list of commands run *after* all services are healthy. Every repo needs the opposite: migrations must finish *before* the app starts. Documenso, Mastodon, Saleor, Gitea and the FastAPI template all have this, and the FastAPI template had independently converged on the answer — a `prestart` service that others wait on with `depends_on: {condition: service_completed_successfully}`.

**Fix:** `kind: oneshot`. A oneshot runs to completion, is healthy on exit 0, requires `run` instead of `start`, and is exempt from R5 because its exit code *is* the probe. Depending on one via `needs` means "wait for it to succeed".

`setup:` was then **deleted**. Ordering is expressed by `needs` alone, there is one dependency mechanism instead of two, and each setup step gets its own container, logs, and located failure.

### G4 — `seed:` had no execution context · found while writing fixture 1

`seed: {migrate: <argv>, data: <argv>}` on a database service was incoherent: a Prisma or Alembic migration does not run inside the Postgres container, it runs inside the *application* container using the application's dependency tree. The schema never said where the command ran.

**Fix:** `seed: {after: [<oneshot names>], sources: [<globs>]}`. Seeding is expressed as ordinary oneshot services — reusing G3 — so the execution context is explicit. `sources` decides when the seed image is stale.

### G5 — env var names with dots · 1 of 5 repos

Env names were `[A-Za-z_][A-Za-z0-9_]*`. Elasticsearch requires `discovery.type` and `xpack.security.enabled`. Relaxed to permit dots.

### G6 — streaming test output has no file path · 1 of 5 repos

`report` required `path`. `go test -json` streams newline-delimited events to stdout and writes nothing. `path` is now optional, and omitting it means "parse the command's stdout".

## Findings that did NOT change the schema

These are the more interesting result: places where the schema held under pressure.

- **Compound dev commands.** Documenso's `dev` is `translate:compile && turbo run dev`; Gitea's `make watch` runs two watchers; Mastodon's `bin/dev` runs a four-line Procfile. None is expressible under R1, and none needs to be: each decomposes into oneshots and services with `needs` edges. This is the main work M3 introspection must do, and it is mechanical.
- **Inline `VAR=value` command prefixes.** `env PORT=3000 RAILS_ENV=development bundle exec puma` and `SECRET_KEY=dummy python manage.py collectstatic` are shell syntax. They move into `env`, which is strictly clearer.
- **Repo scripts outside the argv[0] allowlist.** Mastodon's `bin/rspec` and `bin/flatware` are the R2 escape hatch working exactly as designed: permitted, surfaced for one-time approval with the exact argv shown, no `raw:` block required. Two warnings across five repos is a tolerable approval burden.
- **Test wrappers become unnecessary.** Documenso runs Playwright under `start-server-and-test "npm run start" http://localhost:3000 "playwright test"`. With `needs: [web, db, storage, mail]` devbay has already started and health-probed the server, so the task reduces to `[npx, playwright, test]`. The manifest removed work rather than adding it — the clearest evidence in the set that task-scoped materialization pays for itself.
- **Shell in third-party image startup.** Documenso's compose starts MinIO with `entrypoint: sh -c 'mkdir -p /data/... && minio server ...'`. Routing it through `externals: {s3: {emulate: minio}}` keeps that shell out of the manifest entirely: devbay owns emulator launch configuration, the manifest names the role.
- **Repos shipping their own reverse proxy.** The FastAPI template ships Traefik, which would fight devbay's proxy for `:80`. It is simply omitted from the manifest. No schema construct needed — but M3 needs a rule for it.

## Evidence collected for later milestones

- **CI is the highest-signal introspection input, as planned in M3.** Gitea's workflows already pin `postgres:14@sha256:2f4394…` and `elasticsearch:8.19.15@sha256:aeda96…`; those digests are copied verbatim into the fixture. Saleor's e2e workflow already runs `pytest --junit-xml=e2e-test-results.xml`. Mastodon's `services:` block already carries `--health-cmd pg_isready --health-interval 10s`, which maps 1:1 onto `health`. None of this was inferred.
- **Host-side watching is not a devbay invention.** The FastAPI template already ships compose `develop.watch` with `action: sync` and `action: rebuild`. C4's design matches what the ecosystem already does.
- **`install_scripts: false` will be hit immediately.** Documenso's root `postinstall` is `patch-package`; the repo does not install without it. The default is still correct — this is precisely the decision that should be visible and approved rather than silent.

## Carried into M0 as open items

1. ~~**YAML anchors and merge keys.**~~ **RESOLVED.** The Mastodon fixture uses `&rails_env` / `<<: *rails_env` to avoid repeating eight environment variables across four services. Verified under `gopkg.in/yaml.v3` v3.0.1: merge keys resolve identically to PyYAML, so validation never sees the anchor and the fixture is safe.
2. **Digest pinning.** Only the Gitea fixture pins by digest, because those digests were available in its CI. The others use floating tags. M0's validator should warn, and `devbay init` should resolve tags to digests at write time.
3. ~~**RE2 compatibility.**~~ **RESOLVED.** All four patterns (R7 interpolation, R3 credential screen, slug, duration) compile under Go's `regexp` and match identically to Python on the full case set.
4. **`health.log` needs a bounded buffer.** Matching a regex against a stream must not retain unbounded output, and must stop scanning once healthy.
5. **`health.process` is currently unused.** It was added for workers with nothing to probe, but all three such workers across the fixtures (Mastodon's Sidekiq and Vite, Saleor's Celery) turned out to print a deterministic ready line, so every one of them uses `log` instead. `process` is retained as an honest fallback and the validator warns on it, but no fixture exercises it — so it is unproven, and should be reconsidered if nothing has needed it by M1.
