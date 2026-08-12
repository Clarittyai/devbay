'use client';

import { useState } from 'react';

/**
 * The install command, one click to copy. The primary action of the page.
 *
 * Colour-only feedback, no motion: DESIGN_PRINCIPLES rules 3 and 10. The button
 * label changes rather than the shape.
 */
export default function CopyLine({ command }: { command: string }) {
  const [copied, setCopied] = useState(false);

  async function copy() {
    try {
      await navigator.clipboard.writeText(command);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      // Clipboard blocked. The command is on screen and selectable, which is
      // the fallback that always works.
    }
  }

  return (
    <div className="flex w-full items-stretch overflow-hidden rounded-full border border-gray-300 bg-white">
      <code className="flex-1 overflow-x-auto whitespace-nowrap px-4 py-3.5 font-mono text-[12px] leading-6 text-gray-800 sm:px-5 sm:text-sm">
        {command}
      </code>
      <button
        onClick={copy}
        aria-live="polite"
        className="shrink-0 border-l border-gray-300 bg-accent-600 px-4 text-sm font-semibold text-white transition-colors hover:bg-accent-700 sm:px-5"
      >
        {copied ? 'Copied' : 'Copy'}
      </button>
    </div>
  );
}
