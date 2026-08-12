'use client';

import { useEffect, useRef, useState, type ReactNode } from 'react';

/**
 * The scroll entrance: 20px rise, 600ms, once, entrance only.
 *
 * The important part is what it does NOT do. It never hides content that is
 * already on screen.
 *
 * The obvious implementation renders everything at opacity 0 and waits for an
 * IntersectionObserver to reveal it. That fails in more places than it looks.
 * Chrome does not fire IntersectionObserver in a hidden tab and throttles
 * timers there too, so a background tab, a screenshot service, a social preview
 * crawler or a headless capture all get a blank page. The server-rendered HTML
 * is blank as well, which anything that does not execute JavaScript will read.
 *
 * So the markup renders visible, and on mount the client hides only the
 * elements that are below the fold before animating them in. Anything already
 * in view stays put. Nothing is ever hidden without a live observer able to
 * bring it back, and a reader with JavaScript off sees the whole page.
 */

const EASE = 'cubic-bezier(0.25, 0.46, 0.45, 0.94)';

type Phase = 'static' | 'hidden' | 'shown';

export default function Reveal({
  children,
  delay = 0,
  y = 20,
  duration = 0.6,
  className = '',
}: {
  children: ReactNode;
  delay?: number;
  y?: number;
  duration?: number;
  className?: string;
}) {
  // 'static' is the server-rendered state: visible, no transition.
  const [phase, setPhase] = useState<Phase>('static');
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const node = ref.current;
    if (!node) return;
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;

    // No viewer, so nothing to animate for. A hidden tab fires no
    // IntersectionObserver and throttles timers, which is exactly the state a
    // screenshot service or preview crawler loads the page in. Leaving the
    // content alone means they capture the finished page.
    if (document.visibilityState === 'hidden') return;

    // Already on screen: leave it alone. Hiding it now would be a flash at
    // best, and a permanently blank hero wherever the observer never runs.
    if (node.getBoundingClientRect().top < window.innerHeight) return;

    setPhase('hidden');

    const reveal = () => setPhase('shown');
    const observer = new IntersectionObserver(
      (entries) => {
        if (entries[0]?.isIntersecting) {
          reveal();
          observer.disconnect();
        }
      },
      { rootMargin: '0px 0px -12% 0px' },
    );
    observer.observe(node);

    // A floor, for the case where the observer never reports at all.
    const failOpen = setTimeout(reveal, 1500);

    return () => {
      observer.disconnect();
      clearTimeout(failOpen);
    };
  }, []);

  const hidden = phase === 'hidden';
  return (
    <div
      ref={ref}
      // min-w-0 because this is nearly always a grid item, and a grid item
      // defaults to min-width:auto: it refuses to shrink below its content's
      // min-content width. One unbreakable line of terminal output then widens
      // the whole column and the page scrolls sideways on a phone. Every code
      // block here scrolls inside its own container, so the column is free to
      // shrink. Harmless in normal flow, where the wrapper is also used.
      className={`min-w-0 ${className}`}
      style={{
        opacity: hidden ? 0 : 1,
        transform: hidden ? `translateY(${y}px)` : 'none',
        transition:
          phase === 'static'
            ? undefined
            : `opacity ${duration}s ${EASE} ${delay}s, transform ${duration}s ${EASE} ${delay}s`,
      }}
    >
      {children}
    </div>
  );
}
