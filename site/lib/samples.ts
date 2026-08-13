/**
 * Every artifact shown on the page, as data.
 *
 * These are not mock-ups. Each one is either copied from the repository or
 * assembled from the exact format strings devbay prints with, so the page can
 * be re-checked against the tool rather than drifting away from it. Sources are
 * named per block.
 */

/** How devbay colours its own output: the ANSI helpers in cmd/devbay/main.go. */
export type Tone = 'ok' | 'warn' | 'bad' | 'dim' | 'plain' | 'strong' | 'accent';

export type Line = { t: Tone; s: string }[];

const p = (s: string): Line => [{ t: 'plain', s }];

/* ------------------------------------------------------------------ hero */

/** `devbay new`: wave lines from internal/engine/engine.go, summary from main.go. */
export const heroCommand = 'devbay new add-search';

export const heroOutput: Line[] = [
  [{ t: 'dim', s: '  wave 0: cache, db' }],
  [{ t: 'dim', s: '  cache healthy' }],
  [{ t: 'dim', s: '  db healthy' }],
  [{ t: 'dim', s: '  wave 1: api' }],
  [{ t: 'dim', s: '  api healthy' }],
  [{ t: 'dim', s: '  wave 2: web' }],
  [{ t: 'dim', s: '  web healthy' }],
  [
    { t: 'ok', s: 'created ' },
    { t: 'strong', s: 'add-search' },
    { t: 'plain', s: ' (search) on add-search in 1.284s' },
  ],
  [
    { t: 'plain', s: '  api              ' },
    { t: 'accent', s: 'http://api.add-search.taskboard.localhost' },
  ],
  [
    { t: 'plain', s: '  web              ' },
    { t: 'accent', s: 'http://add-search.taskboard.localhost' },
  ],
];

/* --------------------------------------------------------- cookie gate */

/**
 * docs/ACCEPTANCE.md, "The browser gate". Measured in Chrome on 2026-08-11
 * against examples/cookie-isolation, which ships in the repo.
 */
export const cookieRows = {
  ports: {
    reached: '127.0.0.1:40160 / :41540',
    alpha: 'session=alpha-session',
    beta: 'session=alpha-session',
    leaked: true,
  },
  hostnames: {
    reached: '<bay>.cookies.localhost',
    alpha: 'session=alpha-session',
    beta: '(none)',
    leaked: false,
  },
} as const;

/* ------------------------------------------------------------- velocity */

/** `devbay ls`: header and row format from cmd/devbay/main.go:209-222. */
export const lsHeader = 'BAY            ALIAS        STATE    URL';

export const lsRows: { bay: string; alias: string; state: string; url: string; focused?: boolean }[] =
  [
    { bay: 'add-search', alias: 'search', state: 'hot', url: 'http://add-search.taskboard.localhost', focused: true },
    { bay: 'fix-login', alias: 'login', state: 'warm', url: 'http://fix-login.taskboard.localhost' },
    { bay: 'bump-deps', alias: 'deps', state: 'warm', url: 'http://bump-deps.taskboard.localhost' },
    { bay: 'retry-webhook', alias: 'webhook', state: 'warm', url: 'http://retry-webhook.taskboard.localhost' },
    { bay: 'audit-log', alias: 'audit', state: 'cold', url: 'http://audit-log.taskboard.localhost' },
  ];

/** Measured on this machine: five bays of the four-service example. */
export const cost = { bays: 5, containers: 20, memoryMiB: 289 };

/** `devbay run` on a task with `needs: []`. */
export const unitRun: Line[] = [
  [
    { t: 'ok', s: 'pass ' },
    { t: 'strong', s: 'unit' },
    { t: 'plain', s: ' in 412ms' },
    { t: 'dim', s: '  (37 passed, 0 failed, 1 skipped)' },
  ],
];

/* ------------------------------------------------------------ certainty */

/** The R2 gate refusing, then the same command after `devbay approve`. */
export const approvalRefused: Line[] = [
  [
    { t: 'bad', s: 'error' },
    { t: 'plain', s: ' bay: 1 command(s) in devbay.yaml have not been approved:' },
  ],
  p(''),
  [{ t: 'plain', s: '  R2 services/web/start' }],
  [{ t: 'strong', s: '    ./bin/dev' }],
  p(''),
  [{ t: 'plain', s: 'Read them, then run:' }],
  p(''),
  [{ t: 'plain', s: '  devbay approve' }],
];

export const approvalGranted: Line[] = [
  [{ t: 'dim', s: '  R2 services/web/start' }],
  [{ t: 'strong', s: '  ./bin/dev' }],
  [{ t: 'dim', s: '  this runs inside the bay, with the bay’s environment and secrets' }],
  [{ t: 'plain', s: 'approve? [y/N] y' }],
  p(''),
  [
    { t: 'ok', s: 'ok' },
    { t: 'plain', s: ' approved 1 of 1' },
  ],
];

/* -------------------------------------------------------- observability */

/** `devbay run` on a failure: the block from cmd/devbay/main.go:275-307. */
export const typedFailure: Line[] = [
  [
    { t: 'bad', s: 'fail ' },
    { t: 'strong', s: 'unit' },
    { t: 'plain', s: ' in 214ms' },
    { t: 'dim', s: '  (1 passed, 1 failed, 0 skipped)' },
  ],
  p(''),
  [{ t: 'strong', s: '  test_subtraction' }],
  [{ t: 'dim', s: '  suite.py:42' }],
  [{ t: 'plain', s: '  assert 5 - 3 == 1' }],
];

/** `devbay status` on a bay whose one broken service is still being restarted. */
export const degradedStatus: Line[] = [
  [
    { t: 'strong', s: 'add-dashboards' },
    { t: 'plain', s: '  dash  ' },
    { t: 'bad', s: 'mixed' },
    { t: 'plain', s: '  add-dashboards' },
  ],
  [{ t: 'plain', s: 'services' }],
  [
    { t: 'plain', s: '  grafana        running    ' },
    { t: 'dim', s: 'http://127.0.0.1:43480' },
  ],
  [
    { t: 'plain', s: '  prometheus     ' },
    { t: 'warn', s: 'restarting' },
    { t: 'dim', s: ' http://127.0.0.1:43481' },
  ],
];

export const degradedLog =
  'level=ERROR msg="Error loading config" file=/etc/prometheus/prometheus.yml ' +
  'err="expected Alertmanager api version to be one of [v2] but got v1"';

/* --------------------------------------------------------- zero config */

/** examples/taskboard/docker-compose.yml, trimmed to the two services shown. */
export const composeIn = `services:
  db:
    image: postgres:16-alpine
    environment:
      POSTGRES_PASSWORD: postgres
      POSTGRES_DB: taskboard

  web:
    build: ./web
    ports: ["3000:3000"]
    depends_on: [api]
    environment:
      # Called from the browser, so this has
      # to become a browser address.
      API_URL: http://localhost:4000`;

/** What `devbay init` writes from it, including the provenance header. */
export const manifestOut = `# Where this came from:
#   compose  docker-compose.yml — service "db" from
#            image postgres:16-alpine
#   convention  health probe for "db" from the
#            postgres image family
#
# STILL TO DECIDE — devbay could not work these out:
#   - service "web" was given a placeholder probe
#     \`GET /\`; point it at a real health endpoint

services:
  db:
    image: postgres:16-alpine
    port: 5432
    health: {cmd: [pg_isready, -U, postgres]}
    env:
      POSTGRES_PASSWORD: postgres
      POSTGRES_DB: taskboard

  web:
    build: {context: ./web}
    port: 3000
    primary: true
    needs: [api]
    env:
      API_URL: \${bay.api.public_url}`;

/* ------------------------------------------------------- agent clients */

/** The three clients `devbay mcp install` writes, and where each keeps them. */
export const clients = [
  { name: 'Claude Code', file: '.mcp.json', scope: 'committed with the repository' },
  { name: 'Cursor', file: '.cursor/mcp.json', scope: 'committed with the repository' },
  { name: 'Codex CLI', file: '~/.codex/config.toml', scope: 'your user config' },
];

/** Real output from `devbay mcp install`. */
export const mcpInstall: Line[] = [
  [
    { t: 'ok', s: '  wrote   ' },
    { t: 'plain', s: 'Claude Code  ' },
    { t: 'dim', s: '.mcp.json' },
  ],
  [
    { t: 'ok', s: '  wrote   ' },
    { t: 'plain', s: 'Cursor       ' },
    { t: 'dim', s: '.cursor/mcp.json' },
  ],
  [
    { t: 'ok', s: '  wrote   ' },
    { t: 'plain', s: 'Codex CLI    ' },
    { t: 'dim', s: '~/.codex/config.toml' },
  ],
  [{ t: 'dim', s: '       Codex reads this at startup, so restart it.' }],
  [
    { t: 'ok', s: '  wrote   ' },
    { t: 'plain', s: 'Claude Code  ' },
    { t: 'dim', s: 'CLAUDE.md' },
  ],
  [
    { t: 'ok', s: '  wrote   ' },
    { t: 'plain', s: 'Codex CLI    ' },
    { t: 'dim', s: 'AGENTS.md' },
  ],
  [
    { t: 'ok', s: '  wrote   ' },
    { t: 'plain', s: 'Cursor       ' },
    { t: 'dim', s: '.cursor/rules/devbay.mdc' },
  ],
  p(''),
  [
    { t: 'ok', s: 'done' },
    { t: 'plain', s: ' ask your agent to create a bay and run' },
  ],
  [{ t: 'plain', s: '     a task. It has seven tools:' }],
  [{ t: 'dim', s: '  bay_create, bay_list, bay_run_task,' }],
  [{ t: 'dim', s: '  bay_logs, bay_url, bay_status, bay_destroy' }],
];

/* -------------------------------------------------------------- agents */

/** internal/mcp/tools.go */
export const mcpTools = [
  { name: 'bay_create', title: 'Create an isolated environment' },
  { name: 'bay_list', title: 'List environments' },
  { name: 'bay_run_task', title: 'Run a declared task' },
  { name: 'bay_logs', title: 'Read service logs' },
  { name: 'bay_url', title: 'Get a service URL' },
  { name: 'bay_status', title: 'Inspect one environment' },
  { name: 'bay_destroy', title: 'Destroy an environment' },
];

/** The struct in internal/engine/task.go, with report.Failure inside it. */
export const mcpResult = `{
  "task": "unit",
  "exit_code": 1,
  "duration_ms": 214,
  "total": 2,
  "passed": 1,
  "failed": 1,
  "parsed": true,
  "failures": [
    {
      "name": "test_subtraction",
      "file": "suite.py",
      "line": 42,
      "message": "assert 5 - 3 == 1"
    }
  ]
}`;

/* ------------------------------------------------------------ evidence */

export const evidence = [
  {
    figure: '29 of 35',
    label: 'real compose stacks devbay serves',
    detail:
      'Every stack in docker/awesome-compose, copied unmodified, each compared with what docker compose up does with the same repository on the same machine. Compose serves 23. There is no stack compose runs that devbay does not.',
  },
  {
    figure: '313',
    label: 'tests across 19 packages',
    detail:
      'Run under the race detector on every push. The integration packages talk to real Docker rather than a fake.',
  },
  {
    figure: '21',
    label: 'acceptance scenarios',
    detail:
      'Each drives the real binary against real containers and states what would have to be observed for its claim to be false.',
  },
];

export const limits = [
  {
    head: 'It is not a daemon.',
    body: 'Every command is a short-lived process. Nothing watches your machine between them.',
  },
  {
    head: 'It does not run your code anywhere but here.',
    body: 'No cloud, no remote environments, no state syncing, no telemetry. That is a decision, not a roadmap item.',
  },
  {
    head: 'It does not deploy anything.',
    body: 'No staging, no preview environments, no CI runner. A bay dies when you are done with it.',
  },
  {
    head: 'It does not hide the manifest.',
    body: 'devbay.yaml is committed and reviewable in a pull request. A generated file you cannot audit is worse than none.',
  },
];

/* ---------------------------------------------------------------- misc */

/**
 * Served by app/install/route.ts, which passes through the installer on main.
 * Short enough to read before you run it, and readable in a browser first.
 */
export const installCommand = 'curl -fsSL devbay.claritty.ai/install | sh';

export const repoUrl = 'https://github.com/Clarittyai/devbay';
