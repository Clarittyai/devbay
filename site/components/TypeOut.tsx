'use client';

import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import { TerminalLines, Prompt } from './Terminal';
import type { Line } from '@/lib/samples';

/**
 * The hero terminal, typing itself.
 *
 * It types the command a character at a time, then reveals the output lines at
 * the pace they actually appear when a bay boots: the waves land in bursts, the
 * summary lands last.
 *
 * It starts from the finished state rather than an empty one, and only clears
 * itself once the client has confirmed there is someone to watch it play. That
 * ordering is what keeps the terminal readable everywhere the animation cannot
 * run: with JavaScript off, under prefers-reduced-motion, and in a hidden tab,
 * which is how screenshot services and link preview crawlers load a page. The
 * decision happens before paint, so a viewer who does get the animation never
 * sees the finished output flash first.
 *
 * The container reserves its final height throughout, so nothing on the page
 * moves while it plays.
 */

// useLayoutEffect is the right hook for a decision that must not be visible,
// but React logs a warning if it reaches the server renderer, where it is a
// no-op. On the server there is nothing to decide.
const useBeforePaint = typeof window === 'undefined' ? useEffect : useLayoutEffect;

export default function TypeOut({
  command,
  lines,
  className = '',
}: {
  command: string;
  lines: Line[];
  className?: string;
}) {
  // The finished state, which is also what the server renders.
  const [typed, setTyped] = useState(command);
  const [shown, setShown] = useState(lines.length);
  const [done, setDone] = useState(true);
  const ref = useRef<HTMLDivElement>(null);

  useBeforePaint(() => {
    const node = ref.current;
    if (!node) return;
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;
    if (document.visibilityState === 'hidden') return;

    // From here on there is a viewer and motion is wanted, so it is safe to
    // clear the terminal and play it.
    setTyped('');
    setShown(0);
    setDone(false);

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

    // Above the fold already, which is where the hero terminal lives.
    if (node.getBoundingClientRect().top < window.innerHeight) {
      play();
      return () => timers.forEach(clearTimeout);
    }

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

    // A floor, for the case where the observer never reports at all.
    const failOpen = setTimeout(() => {
      observer.disconnect();
      play();
    }, 1500);

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
