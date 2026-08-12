import Wordmark from '@/components/Wordmark';
import Reveal from '@/components/Reveal';
import Terminal from '@/components/Terminal';
import TypeOut from '@/components/TypeOut';
import CookieToggle from '@/components/CookieToggle';
import CopyLine from '@/components/CopyLine';
import CodeCompare from '@/components/CodeCompare';
import {
  approvalGranted,
  approvalRefused,
  composeIn,
  cost,
  degradedLog,
  degradedStatus,
  evidence,
  heroCommand,
  heroOutput,
  installCommand,
  limits,
  lsRows,
  manifestOut,
  clients,
  mcpInstall,
  mcpResult,
  mcpTools,
  repoUrl,
  typedFailure,
  unitRun,
} from '@/lib/samples';

/**
 * One production deployment, always showing the shipped release. The tag is
 * read at build and refreshed hourly, so the version on the page and the
 * version the install line gives you cannot drift apart.
 */
export const revalidate = 3600;

async function latestVersion(): Promise<string | null> {
  try {
    const res = await fetch('https://api.github.com/repos/Clarittyai/devbay/releases/latest', {
      headers: { Accept: 'application/vnd.github+json' },
      next: { revalidate: 3600 },
    });
    if (!res.ok) return null;
    const data = (await res.json()) as { tag_name?: unknown };
    return typeof data.tag_name === 'string' ? data.tag_name : null;
  } catch {
    return null;
  }
}

export default async function Page() {
  const version = await latestVersion();

  return (
    <>
      <Nav version={version} />
      <main>
        <Hero />
        <CookieSection />
        <VelocitySection />
        <CertaintySection />
        <ObservabilitySection />
        <ConfigSection />
        <ClientSection />
        <AgentSection />
        <EvidenceSection />
        <LimitsSection />
        <InstallSection version={version} />
      </main>
      <Footer version={version} />
    </>
  );
}

/* ------------------------------------------------------------------- nav */

function Nav({ version }: { version: string | null }) {
  return (
    <header className="sticky top-0 z-50 border-b border-gray-200/80 bg-canvas/90 backdrop-blur">
      <div className="mx-auto flex max-w-6xl items-center gap-4 px-4 py-3.5 sm:px-6 lg:px-8">
        <Wordmark />
        {version ? (
          <span className="tnum hidden rounded-full border border-gray-300 px-2 py-0.5 font-mono text-[11px] text-gray-500 sm:inline">
            {version}
          </span>
        ) : null}
        <div className="ml-auto flex items-center gap-5">
          <a
            href={`${repoUrl}/blob/main/docs/CAPABILITIES.md`}
            className="hidden text-sm font-medium text-gray-600 transition-colors hover:text-gray-900 sm:inline"
          >
            Docs
          </a>
          <a
            href={repoUrl}
            className="text-sm font-medium text-gray-600 transition-colors hover:text-gray-900"
          >
            GitHub
          </a>
        </div>
      </div>
    </header>
  );
}

/* ------------------------------------------------------------------ hero */

function Hero() {
  return (
    <section className="relative overflow-hidden">
      {/* One quiet accent wash. Rule 8: restraint. */}
      <div
        aria-hidden
        className="absolute inset-0"
        style={{ background: 'radial-gradient(120% 80% at 50% 0%, #eef1ff 0%, #FBF9F5 58%)' }}
      />
      <div className="relative mx-auto max-w-6xl px-4 pb-16 pt-16 sm:px-6 sm:pb-24 sm:pt-24 lg:px-8">
        <div className="grid items-center gap-12 lg:grid-cols-[minmax(0,1fr)_minmax(0,1.05fr)] lg:gap-16">
          <Reveal>
            <h1 className="text-[2.2rem] font-extrabold leading-[1.05] text-gray-900 sm:text-5xl lg:text-[3.6rem]">
              Stop guessing. Run every branch for real.
            </h1>
            <p className="mt-6 max-w-xl text-lg leading-relaxed text-gray-600">
              Every branch gets its own containers, database, ports and browser origin. Five at once,
              on your machine, walled off from each other. Nothing runs without your OK.
            </p>

            <div className="mt-8 max-w-xl">
              <CopyLine command={installCommand} />
            </div>

            <div className="mt-5 flex flex-wrap items-center gap-2 text-sm text-gray-600">
              <Chip strong="Seconds" rest="to your first bay" />
              <Chip strong="Local only" rest="no telemetry" />
              <Chip strong="You approve" rest="every command" />
            </div>
          </Reveal>

          <Reveal delay={0.12}>
            <TypeOut command={heroCommand} lines={heroOutput} />
          </Reveal>
        </div>
      </div>
    </section>
  );
}

function Chip({ strong, rest }: { strong: string; rest: string }) {
  return (
    <span className="inline-flex items-center gap-1.5 rounded-full border border-gray-200 bg-white/70 px-3 py-1">
      <span className="font-semibold text-gray-900">{strong}</span> {rest}
    </span>
  );
}

/* ----------------------------------------------------------- the problem */

function CookieSection() {
  return (
    <section className="section border-t border-gray-200">
      <div className="grid items-start gap-10 lg:grid-cols-2 lg:gap-16">
        <Reveal>
          <div className="eyebrow">Isolation</div>
          <h2 className="h-section mt-3">Two branches, one cookie jar.</h2>
          <p className="lede mt-5">
            Browsers key cookies by host and ignore the port. Two branches on{' '}
            <code className="rounded bg-gray-900/[0.06] px-1.5 py-0.5 font-mono text-[0.9em]">
              localhost:3000
            </code>{' '}
            and{' '}
            <code className="rounded bg-gray-900/[0.06] px-1.5 py-0.5 font-mono text-[0.9em]">
              localhost:3001
            </code>{' '}
            share one session. Log in to one, and the other is already logged in as you.
          </p>
          <p className="mt-4 text-[15px] leading-relaxed text-gray-600">
            Giving each bay its own origin removes the bug instead of working around it. Try the
            toggle.
          </p>
          <p className="mt-6 text-sm text-gray-500">
            Measured in Chrome on 2026-08-11 against{' '}
            <a
              href={`${repoUrl}/tree/main/examples/cookie-isolation`}
              className="font-medium text-accent-600 underline underline-offset-4 transition-colors hover:text-accent-700"
            >
              examples/cookie-isolation
            </a>
            , which ships in the repository so you can repeat it.
          </p>
        </Reveal>

        <Reveal delay={0.1}>
          <CookieToggle />
        </Reveal>
      </div>
    </section>
  );
}

/* -------------------------------------------------------------- velocity */

function VelocitySection() {
  return (
    <section className="section border-t border-gray-200">
      <div className="grid items-center gap-10 lg:grid-cols-2 lg:gap-16">
        {/* Visual left on this one. Rule 9: the visual side alternates. */}
        <Reveal className="order-2 lg:order-1">
          <div className="frame overflow-x-auto">
            <table className="w-full font-mono text-[12px]">
              <thead>
                <tr className="border-b border-gray-200 text-left text-[11px] uppercase tracking-[0.12em] text-gray-500">
                  <th className="px-3 py-2.5 font-medium">Bay</th>
                  <th className="px-3 py-2.5 font-medium">State</th>
                  <th className="px-3 py-2.5 font-medium">URL</th>
                </tr>
              </thead>
              <tbody>
                {lsRows.map((r) => (
                  <tr key={r.bay} className="border-b border-gray-100 last:border-0">
                    <td className="whitespace-nowrap px-3 py-2.5 text-gray-900">
                      {r.bay}
                      {r.focused ? <span className="ml-1.5 text-accent-600">*</span> : null}
                    </td>
                    <td className="px-3 py-2.5">
                      <span
                        className={
                          r.state === 'cold'
                            ? 'text-gray-400'
                            : r.state === 'hot'
                              ? 'font-semibold text-green'
                              : 'text-green'
                        }
                      >
                        {r.state}
                      </span>
                    </td>
                    <td className="whitespace-nowrap px-3 py-2.5 text-gray-500">{r.url}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p className="tnum mt-4 text-sm text-gray-500">
            {cost.bays} bays, {cost.containers} containers, {cost.memoryMiB} MiB in total. Measured
            inside the VM.
          </p>
          <Terminal className="mt-4" command="devbay run add-search unit" lines={unitRun} />
        </Reveal>

        <Reveal className="order-1 lg:order-2">
          <div className="eyebrow">Velocity</div>
          <h2 className="h-section mt-3">Five branches, running at once.</h2>
          <p className="lede mt-5">
            Each one boots in about a second and answers on its own hostname. Your agent can hold
            four experiments open while you review the fifth.
          </p>
          <p className="mt-4 text-[15px] leading-relaxed text-gray-600">
            A task declares the services it needs, so devbay starts only those. A unit suite declares
            none and boots nothing at all, which is why it comes back in milliseconds.
          </p>
        </Reveal>
      </div>
    </section>
  );
}

/* ------------------------------------------------------------- certainty */

function CertaintySection() {
  return (
    <section className="section border-t border-gray-200">
      <div className="grid items-center gap-10 lg:grid-cols-2 lg:gap-16">
        <Reveal>
          <div className="eyebrow">Certainty</div>
          <h2 className="h-section mt-3">Nothing runs without your OK.</h2>
          <p className="lede mt-5">
            A repository can ask to run <code className="font-mono text-[0.9em]">./bin/dev</code>.
            devbay will not execute it until you have read the exact command and agreed to it.
          </p>
          <p className="mt-4 text-[15px] leading-relaxed text-gray-600">
            The decision is remembered, and it is keyed to the whole command. Approving{' '}
            <code className="font-mono text-[0.9em]">bin/dev</code> does not approve{' '}
            <code className="font-mono text-[0.9em]">bin/dev --seed-prod</code>. An agent cannot make
            the decision for you: the gate checks that a person answered.
          </p>
        </Reveal>

        <Reveal delay={0.1} className="space-y-3">
          <Terminal label="Before you agree" command="devbay new add-search" lines={approvalRefused} />
          <Terminal label="After" command="devbay approve" lines={approvalGranted} />
        </Reveal>
      </div>
    </section>
  );
}

/* --------------------------------------------------------- observability */

function ObservabilitySection() {
  return (
    <section className="section border-t border-gray-200">
      <div className="grid items-center gap-10 lg:grid-cols-2 lg:gap-16">
        <Reveal className="order-2 space-y-3 lg:order-1">
          <Terminal label="A failing test" lines={typedFailure} />
          <Terminal label="A bay with one broken service" lines={degradedStatus} />
          <div className="overflow-x-auto rounded-2xl border border-gray-200 bg-white p-4 font-mono text-[12px] leading-relaxed text-gray-600">
            {degradedLog}
          </div>
        </Reveal>

        <Reveal className="order-1 lg:order-2">
          <div className="eyebrow">Observability</div>
          <h2 className="h-section mt-3">See exactly what ran.</h2>
          <p className="lede mt-5">
            A failure comes back as a name, a file, a line and the assertion. Nobody has to read
            scrollback to find out what broke.
          </p>
          <p className="mt-4 text-[15px] leading-relaxed text-gray-600">
            When one service will not start, the bay stays up. The broken service is named, its
            container and its logs are kept, and everything else keeps serving. That is what Docker
            does with a stack, and a tool that deleted the evidence instead would be harder to work
            with, not safer.
          </p>
        </Reveal>
      </div>
    </section>
  );
}

/* ------------------------------------------------------------ zero config */

function ConfigSection() {
  return (
    <section className="section border-t border-gray-200">
      <Reveal>
        <div className="max-w-2xl">
          <div className="eyebrow">Zero config</div>
          <h2 className="h-section mt-3">It writes the file. You can read it.</h2>
          <p className="lede mt-5">
            <code className="font-mono text-[0.9em]">devbay init</code> reads the compose file, the
            Actions workflow and the Procfile you already have. Every line it writes says where it
            came from, and it lists what it could not work out rather than guessing.
          </p>
        </div>
      </Reveal>

      <Reveal delay={0.1} className="mt-9">
        <CodeCompare
          leftLabel="docker-compose.yml, yours"
          rightLabel="devbay.yaml, written for you"
          left={composeIn}
          right={manifestOut}
          highlight="${bay.api.public_url}"
        />
      </Reveal>

      <Reveal delay={0.15}>
        <p className="mt-5 max-w-prose text-[15px] leading-relaxed text-gray-600">
          The literal address became a reference. A hardcoded{' '}
          <code className="font-mono text-[0.9em]">localhost:4000</code> is wrong the moment a second
          copy exists, so devbay rewrites it to the bay asking the question. The file is committed
          and reviewable in a pull request.
        </p>
      </Reveal>
    </section>
  );
}

/* --------------------------------------------------------------- clients */

function ClientSection() {
  return (
    <section className="section border-t border-gray-200">
      <div className="grid items-center gap-10 lg:grid-cols-2 lg:gap-16">
        <Reveal className="order-2 lg:order-1">
          <Terminal command="devbay mcp install" lines={mcpInstall} />
          <ul className="mt-6 border-t border-gray-200">
            {clients.map((c) => (
              <li
                key={c.name}
                className="flex flex-wrap items-baseline justify-between gap-x-6 gap-y-1 border-b border-gray-200 py-3"
              >
                <span className="font-semibold text-ink">{c.name}</span>
                <code className="font-mono text-[13px] text-gray-500">{c.file}</code>
              </li>
            ))}
          </ul>
        </Reveal>

        <Reveal className="order-1 lg:order-2">
          <div className="eyebrow">Your agent</div>
          <h2 className="h-section mt-3">One command, and your agent can use it.</h2>
          <p className="lede mt-5">
            Claude Code, Cursor and Codex all speak MCP, and each keeps its servers somewhere
            different, in a different format. devbay writes the entry for all three.
          </p>
          <p className="mt-4 text-[15px] leading-relaxed text-gray-600">
            It merges rather than overwrites, so a config holding six servers still holds six
            afterwards, and the Codex file keeps its comments. Run it again after moving the binary
            and it corrects itself. The Claude Code and Cursor entries are committed with the
            repository, so everyone who clones it gets the same tools without being told.
          </p>
        </Reveal>
      </div>
    </section>
  );
}

/* ---------------------------------------------------------------- agents */

function AgentSection() {
  return (
    <section className="section border-t border-gray-200">
      <div className="grid items-center gap-10 lg:grid-cols-2 lg:gap-16">
        <Reveal>
          <div className="eyebrow">Agents</div>
          <h2 className="h-section mt-3">Typed results, not stdout to scrape.</h2>
          <p className="lede mt-5">
            Seven stateless tools over MCP. Every one takes an explicit bay, so there is no session
            to lose.
          </p>
          <p className="mt-4 text-[15px] leading-relaxed text-gray-600">
            An agent asks for a task and gets counts and failures with a file and a line. It can fix
            the test rather than parse the output. Credentials never appear in what it reads.
          </p>
          <ul className="mt-7 space-y-0 border-t border-gray-200">
            {mcpTools.map((t) => (
              <li key={t.name} className="flex flex-wrap items-baseline gap-x-4 gap-y-1 border-b border-gray-200 py-2.5">
                <code className="font-mono text-[13px] font-semibold text-gray-900">{t.name}</code>
                <span className="text-sm text-gray-500">{t.title}</span>
              </li>
            ))}
          </ul>
        </Reveal>

        <Reveal delay={0.1}>
          <div className="terminal">
            <div className="mb-3 font-sans text-[11px] font-semibold uppercase tracking-[0.14em] text-surface-dim">
              bay_run_task, what the agent receives
            </div>
            <pre className="whitespace-pre">{mcpResult}</pre>
          </div>
        </Reveal>
      </div>
    </section>
  );
}

/* -------------------------------------------------------------- evidence */

function EvidenceSection() {
  return (
    <section className="section border-t border-gray-200">
      <Reveal>
        <div className="max-w-2xl">
          <div className="eyebrow">Evidence</div>
          <h2 className="h-section mt-3">Checked against repositories nobody wrote it for.</h2>
        </div>
      </Reveal>

      <div className="mt-10 border-t border-gray-200">
        {evidence.map((e, i) => (
          <Reveal key={e.figure} delay={i * 0.08}>
            <div className="grid gap-2 border-b border-gray-200 py-7 sm:grid-cols-[minmax(0,15rem)_minmax(0,1fr)] sm:gap-10">
              <div>
                <div className="tnum text-3xl font-bold tracking-tight text-gray-900 sm:text-4xl">
                  {e.figure}
                </div>
                <div className="mt-1 text-sm font-medium text-gray-500">{e.label}</div>
              </div>
              <p className="max-w-prose text-[15px] leading-relaxed text-gray-600">{e.detail}</p>
            </div>
          </Reveal>
        ))}
      </div>

      <Reveal>
        <p className="mt-6 text-sm text-gray-500">
          The method and the per-stack results are in{' '}
          <a
            href={`${repoUrl}/blob/main/docs/ACCEPTANCE.md`}
            className="font-medium text-accent-600 underline underline-offset-4 transition-colors hover:text-accent-700"
          >
            docs/ACCEPTANCE.md
          </a>
          . <code className="font-mono text-[0.9em]">scripts/corpus.sh</code> runs it on your machine.
        </p>
      </Reveal>
    </section>
  );
}

/* ---------------------------------------------------------------- limits */

function LimitsSection() {
  return (
    <section className="section border-t border-gray-200">
      <Reveal>
        <div className="max-w-2xl">
          <h2 className="h-section">What it does not do.</h2>
          <p className="lede mt-5">
            Worth knowing before you install it, and none of these are on a roadmap.
          </p>
        </div>
      </Reveal>

      <div className="mt-10 border-t border-gray-200">
        {limits.map((l, i) => (
          <Reveal key={l.head} delay={i * 0.06}>
            <div className="grid gap-2 border-b border-gray-200 py-6 sm:grid-cols-[minmax(0,22rem)_minmax(0,1fr)] sm:gap-10">
              <div className="font-semibold text-gray-900">{l.head}</div>
              <p className="max-w-prose text-[15px] leading-relaxed text-gray-600">{l.body}</p>
            </div>
          </Reveal>
        ))}
      </div>
    </section>
  );
}

/* --------------------------------------------------------------- install */

function InstallSection({ version }: { version: string | null }) {
  return (
    <section className="section border-t border-gray-200">
      <Reveal>
        <div className="max-w-2xl">
          <h2 className="h-section">Install it.</h2>
          <p className="lede mt-5">
            One binary. It needs git and Docker, and it tells you if either is unhappy before you
            create anything.
          </p>
        </div>
        <div className="mt-8 max-w-2xl">
          <CopyLine command={installCommand} />
        </div>
        <div className="mt-5 flex flex-wrap items-center gap-x-6 gap-y-2 text-sm text-gray-500">
          <a
            href={repoUrl}
            className="font-medium text-accent-600 transition-colors hover:text-accent-700"
          >
            Source on GitHub
          </a>
          <span>Apache-2.0</span>
          <span>macOS and Linux</span>
          {version ? <span className="tnum font-mono">{version}</span> : null}
        </div>
        <p className="mt-6 max-w-prose text-sm text-gray-500">
          Then run <code className="font-mono">devbay doctor</code> to check the machine, and{' '}
          <code className="font-mono">devbay init</code> in a repository to see what it proposes.
        </p>
      </Reveal>
    </section>
  );
}

/* ---------------------------------------------------------------- footer */

function Footer({ version }: { version: string | null }) {
  return (
    <footer className="border-t border-gray-200">
      <div className="mx-auto flex max-w-6xl flex-wrap items-center gap-x-6 gap-y-3 px-4 py-8 text-sm text-gray-500 sm:px-6 lg:px-8">
        <Wordmark />
        <span className="tnum">{version ?? ''}</span>
        <span>Apache-2.0</span>
        <a href={repoUrl} className="transition-colors hover:text-gray-900">
          GitHub
        </a>
        <a
          href={`${repoUrl}/blob/main/docs/CAPABILITIES.md`}
          className="transition-colors hover:text-gray-900"
        >
          What it does
        </a>
        <a
          href={`${repoUrl}/blob/main/docs/ACCEPTANCE.md`}
          className="transition-colors hover:text-gray-900"
        >
          How it is checked
        </a>
      </div>
    </footer>
  );
}
