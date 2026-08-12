'use client';

import { useEffect, useRef, useState } from 'react';
import { TerminalLines, Prompt } from './Terminal';
import type { Line } from '@/lib/samples';

/**
 * The hero terminal, typing itself.
 *
 * It types the command a character at a time, then reveals the output lines at
 * the pace they actually appear when a bay boots: the waves land in bursts, the
 * summary lands last. It starts when it scrolls into view and runs once.
 *
 * With prefers-reduced-motion the whole thing renders complete and static.
 * The container reserves its final height either way, so nothing on the page
 * moves while it plays.
 */
export default function TypeOut({
  command,
  lines,
  className = '',
}: {
  command: string;
  lines: Line[];
  className?: string;
}) {
  const [typed, setTyped] = useState('');
  const [shown, setShown] = useState(0);
  const [done, setDone] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const reduce =
      typeof window !== 'undefined' &&
      window.matchMedia('(prefers-reduced-motion: reduce)').matches;

    if (reduce) {
      setTyped(command);
      setShown(lines.length);
      setDone(true);
      return;
    }

    const node = ref.current;
    if (!node) return;

    const timers: ReturnType<typeof setTimeout>[] = [];
    const play = () => {
      command.split('').forEach((_, i) => {
        timers.push(setTimeout(() => setTyped(command.slice(0, i + 1)), 45 * i));
      });

      const after = 45 * command.length + 320;
      lines.forEach((_, i) => {
        timers.push(setTimeout(() => setShown(i + 1), after + 200 * i));
      });
      timers.push(setTimeout(() => setDone(true), after + 200 * lines.length));
    };

    const observer = new IntersectionObserver(
      (entries) => {
        if (!entries[0]?.isIntersecting) return;
        observer.disconnect();
        clearTimeout(failOpen);
        play();
      },
      { threshold: 0.3 },
    );
    observer.observe(node);

    // Chrome does not fire IntersectionObserver in a hidden tab, so the
    // animation starts on its own if the callback never arrives.
    const failOpen = setTimeout(() => {
      observer.disconnect();
      play();
    }, 1200);

    return () => {
      observer.disconnect();
      clearTimeout(failOpen);
      timers.forEach(clearTimeout);
    };
  }, [command, lines]);

  return (
    <div ref={ref} className={`terminal ${className}`}>
      <div className="whitespace-pre">
        <span className="text-surface-dim">$ </span>
        <span className="text-white">{typed}</span>
        {!done ? (
          <span
            aria-hidden
            className="ml-px inline-block h-[1.05em] w-[0.55em] translate-y-[0.15em] bg-surface-text/80"
          />
        ) : null}
      </div>
      <TerminalLines lines={lines.slice(0, shown)} />
      {/* Reserve the rest of the height so the page never jumps as it plays. */}
      {shown < lines.length ? (
        <div aria-hidden className="invisible">
          <TerminalLines lines={lines.slice(shown)} />
        </div>
      ) : null}
    </div>
  );
}

export { Prompt };
