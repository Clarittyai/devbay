# taskboard

A small, dependency-free stack for trying devbay: a browser client, an API, a
cache it actually uses, and a database. Nothing here needs a package install,
so it boots in seconds on a clean machine.

```
web  ──browser fetch──▶  api  ──▶  cache (redis)
                          └──▶  db    (postgres)
```

## Try it

```sh
cd examples/taskboard
git init && git add -A && git commit -m "taskboard"

devbay init          # reads docker-compose.yml and writes devbay.yaml
devbay new my-change # a bay: worktree, containers, ports, hostname, data
```

Open the URL it prints. Add a task, then create a second bay and add a
different one — the two do not see each other's data, and each page says which
bay it is.

## What it demonstrates

**Services wired to each other by reference.** The compose file says
`API_URL: http://localhost:4000` and `DATABASE_URL: postgres://…@db:5432/…`.
Both are wrong once more than one instance exists, so `devbay init` rewrites
them: the browser-facing one becomes `${bay.api.public_url}`, the datastore
ones become `${bay.db.url}` and `${bay.cache.url}`. Nothing in the generated
manifest names a fixed host or port.

**Images built from source.** `api` and `web` have Dockerfiles, and their
build contexts become their watch lists.

**Tasks that materialise only what they need.** `unit` declares `needs: []`
and boots nothing — it is the fast path an agent uses after every change.
`integration` declares `needs: [api]`, so devbay starts the API and its
dependencies and nothing else.

**Typed failures.** The unit task writes JUnit XML and devbay parses it, so a
broken test comes back as a name, a file, a line and an assertion rather than
as a wall of output to grep.

**Live editing.** `devbay watch my-change` applies edits in that bay's
worktree to its containers.

**Bay identity.** Every container is given `DEVBAY_BAY`, `DEVBAY_PROJECT` and
`DEVBAY_SERVICE`; the page title and `/healthz` use it, which is what makes
five open tabs tellable apart.

## Adding the tasks

`devbay init` cannot invent a test command, so it says so and leaves `tasks`
empty. Add:

```yaml
tasks:
  unit:
    run: [node, --test, --test-reporter=junit,
          --test-reporter-destination=reports/junit.xml, api/server.test.js]
    needs: []
    report: {format: junit, path: reports/junit.xml}

  integration:
    run: [node, -e, "fetch(process.env.API+'/healthz').then(r=>r.json()).then(d=>{if(!d.ok)throw new Error('unhealthy')})"]
    needs: [api]
    env: {API: "${bay.api.url}"}
```
