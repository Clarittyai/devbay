import type { Line, Tone } from '@/lib/samples';

/**
 * devbay's own output, rendered with the meaning it has in a shell.
 *
 * The tones map one to one onto the ANSI helpers in cmd/devbay/main.go: green
 * for ok, yellow for a note, red for an error, dim for detail, bold for the
 * thing being named. Getting these right is the difference between a screenshot
 * of a terminal and a picture of one.
 *
 * No window chrome. DESIGN_PRINCIPLES rule 7: no fake traffic lights.
 */

const tone: Record<Tone, string> = {
  ok: 'text-term-ok',
  warn: 'text-term-warn',
  bad: 'text-term-bad',
  accent: 'text-term-accent',
  dim: 'text-surface-dim',
  strong: 'text-white font-semibold',
  plain: '',
};

export function TerminalLines({ lines }: { lines: Line[] }) {
  return (
    <>
      {lines.map((line, i) => (
        <div key={i} className="whitespace-pre">
          {line.length === 0 ? (
            ' '
          ) : (
            line.map((run, j) => (
              <span key={j} className={tone[run.t]}>
                {run.s}
              </span>
            ))
          )}
        </div>
      ))}
    </>
  );
}

export function Prompt({ command }: { command: string }) {
  return (
    <div className="whitespace-pre">
      <span className="text-surface-dim">$ </span>
      <span className="text-white">{command}</span>
    </div>
  );
}

export default function Terminal({
  command,
  lines,
  label,
  className = '',
}: {
  command?: string;
  lines: Line[];
  label?: string;
  className?: string;
}) {
  return (
    <div className={`terminal ${className}`}>
      {label ? (
        <div className="mb-3 font-sans text-[11px] font-semibold uppercase tracking-[0.14em] text-surface-dim">
          {label}
        </div>
      ) : null}
      {command ? <Prompt command={command} /> : null}
      <TerminalLines lines={lines} />
    </div>
  );
}
