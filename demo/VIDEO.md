# The launch video

Ninety seconds. One idea: **two branches, one browser, and the sessions clobber
each other.** Everything else devbay does is a consequence of fixing that, so
the video shows the bug first and the product second.

Nothing here needs a slide. Every shot is a real terminal or a real browser, and
every command is in this repository, so the video can be re-recorded when the
output changes.

## Before recording

```sh
open -a Docker                       # or OrbStack; doctor will tell you
make build
cd examples/taskboard && git init -q . && git add -A && git commit -qm taskboard && cd ../..
bin/devbay doctor                    # should say: no blocking problems
```

Record at 1280×800 or larger, terminal font 18pt or up. Two windows: a terminal,
and a browser with no other tabs. Dark theme reads better when the video is
compressed.

---

## Shot 1 — the problem (0:00–0:12)

**Screen:** terminal, two panes, the same app running twice the ordinary way.

**Narration:**
> You are running two branches of the same app. One on port 3000, one on 3001.
> This is the part everyone already does.

**Command:**
```sh
sh demo/cookie-jar.sh        # stop it after the two bays come up, or narrate over it
```

---

## Shot 2 — the bug (0:12–0:32) — **the shot that matters**

**Screen:** browser. Already recorded as `demo/cookie-jar.gif` if you would
rather cut that in than perform it.

**Narration:**
> Log in to the first one. Now open the second one — different port, same
> browser. It thinks you are logged in as the first. Browsers key cookies by
> host and ignore the port, so both branches share one cookie jar. Every session
> you create in one, you destroy in the other.

**Actions:** visit `127.0.0.1:<port-a>/login`, then `127.0.0.1:<port-b>/`, and
let the viewer read `cookie=session=alpha-session`. Hold for two seconds.

---

## Shot 3 — the fix (0:32–0:45)

**Screen:** browser, same two bays, the hostnames devbay gave them.

**Narration:**
> devbay gives every bay its own origin. Same two apps, same browser, same jar —
> and now the second one has never seen you. That is not a workaround. The class
> of bug is gone.

**Actions:** `alpha.cookies.localhost/login`, then `beta.cookies.localhost/` →
`cookie=(none)`. Hold.

---

## Shot 4 — what it costs (0:45–1:05)

**Screen:** terminal.

**Narration:**
> Three branches, three full stacks — four services each, their own database,
> their own ports, their own hostname. All three in about four seconds.

**Command:**
```sh
sh demo/three-bays.sh
```

Let the three `created …` lines and the table of hostnames land on screen. This
is the claim people disbelieve, so do not cut away early.

---

## Shot 5 — the agent (1:05–1:20)

**Screen:** terminal, then Claude Code / Cursor.

**Narration:**
> An agent drives the same thing over MCP. It asks for a bay, runs a task, and
> gets back the file, the line and the assertion — not output to guess at.

**Commands:**
```sh
bin/devbay mcp install       # writes the config for Claude Code, Cursor and Codex
bin/devbay run add-search unit
```

Show a failing task if you can arrange one: the `failures[]` array with `file`
and `line` is the whole argument for the agent interface.

---

## Shot 6 — the end (1:20–1:30)

**Screen:** terminal.

**Narration:**
> When you are done, it takes itself apart — containers, volumes, networks,
> worktrees. Nothing left behind.

**Commands:**
```sh
bin/devbay rm add-search --force
docker ps -a --filter label=dev.devbay.project=taskboard   # empty
```

**Final card:** `curl -fsSL devbay.claritty.ai/install | sh`

---

## What to say if someone asks

- **Why not just docker compose?** Compose runs one stack. Run two and they
  fight over ports, volumes and the cookie jar. devbay is compose-aware: it
  reads your compose file and generates the manifest from it.
- **Does it work with my repo?** It reads compose files, devcontainers, GitHub
  Actions services, Procfiles and package manifests. `devbay init` tells you
  what it could not work out rather than guessing.
- **What does it not do?** Read `docs/CAPABILITIES.md` on camera if you like —
  the limits are written down, which is the answer to the question underneath
  the question.
