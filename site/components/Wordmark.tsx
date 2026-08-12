/**
 * devbay's mark: four bays side by side, one of them focused.
 *
 * It encodes the product rather than decorating it. Parallel slots are what
 * devbay makes, and exactly one of them holds the project's canonical hostname
 * at a time, which is the filled bar. The same shape works at 16px in a browser
 * tab and at 200px in a header.
 */
export function BayMark({ className = '' }: { className?: string }) {
  return (
    <svg
      className={className}
      viewBox="0 0 24 24"
      fill="none"
      xmlns="http://www.w3.org/2000/svg"
      role="img"
      aria-label="devbay"
    >
      <rect x="1.5" y="5" width="4" height="14" rx="1.5" fill="currentColor" opacity="0.32" />
      <rect x="7.5" y="3" width="4" height="18" rx="1.5" className="fill-accent" />
      <rect x="13.5" y="5" width="4" height="14" rx="1.5" fill="currentColor" opacity="0.32" />
      <rect x="19.5" y="7" width="3" height="10" rx="1.5" fill="currentColor" opacity="0.32" />
    </svg>
  );
}

/**
 * The wordmark, set in the mono face. A tool you type deserves a name that
 * looks like something you type.
 */
export default function Wordmark({ className = '' }: { className?: string }) {
  return (
    <span className={`inline-flex items-center gap-2 ${className}`}>
      <BayMark className="h-[18px] w-[18px] text-ink" />
      <span className="font-mono text-[15px] font-semibold tracking-[-0.02em] text-ink">devbay</span>
    </span>
  );
}
