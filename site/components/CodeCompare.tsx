/**
 * The file you already have, and the file devbay writes from it.
 *
 * Two flat frames side by side. The point the layout has to make is that the
 * literal address on the left became a reference on the right, so the right
 * pane is the wider one and the changed line is the only thing highlighted.
 */
export default function CodeCompare({
  left,
  right,
  leftLabel,
  rightLabel,
  highlight,
}: {
  left: string;
  right: string;
  leftLabel: string;
  rightLabel: string;
  /** Substring to mark in the right pane. */
  highlight?: string;
}) {
  return (
    <div className="grid gap-4 lg:grid-cols-2">
      <Pane label={leftLabel} body={left} muted />
      <Pane label={rightLabel} body={right} highlight={highlight} />
    </div>
  );
}

function Pane({
  label,
  body,
  muted = false,
  highlight,
}: {
  label: string;
  body: string;
  muted?: boolean;
  highlight?: string;
}) {
  return (
    <div className="frame flex flex-col">
      <div className="border-b border-gray-200 px-5 py-3 font-mono text-[11px] font-semibold uppercase tracking-[0.14em] text-gray-500">
        {label}
      </div>
      <pre
        className={`flex-1 overflow-x-auto px-5 py-4 font-mono text-[12.5px] leading-[1.7] ${
          muted ? 'text-gray-500' : 'text-gray-800'
        }`}
      >
        {highlight ? mark(body, highlight) : body}
      </pre>
    </div>
  );
}

function mark(body: string, needle: string) {
  const at = body.indexOf(needle);
  if (at === -1) return body;
  return (
    <>
      {body.slice(0, at)}
      <mark className="rounded bg-accent/15 px-1 font-semibold text-accent-600">{needle}</mark>
      {body.slice(at + needle.length)}
    </>
  );
}
