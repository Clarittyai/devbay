'use client';

import { useEffect, useRef, useState, type ReactNode } from 'react';

/**
 * The scroll entrance, matching clarity-website's Reveal: 20px rise, 600ms,
 * EASE [0.25, 0.46, 0.45, 0.94], once, entrance only.
 *
 * Written in CSS rather than framer-motion for two reasons. It is the only
 * animation on the page, so a 50KB library to fade one div is a poor trade on a
 * page whose job is to load fast. And more importantly it fails open: if the
 * IntersectionObserver never delivers a callback, a timer reveals the content
 * anyway.
 *
 * That is not hypothetical. Chrome does not fire IntersectionObserver at all
 * while a tab is hidden, so a page that only reveals on intersection can render
 * completely blank in a background tab, a screenshot service, or anything
 * driving the browser without a visible window. Content that hides itself and
 * waits for permission to come back is a bad default for a marketing page.
 */

const EASE = 'cubic-bezier(0.25, 0.46, 0.45, 0.94)';

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
  const [shown, setShown] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const reduce = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    if (reduce) {
      setShown(true);
      return;
    }

    const node = ref.current;
    if (!node) return;

    const reveal = () => setShown(true);
    const observer = new IntersectionObserver(
      (entries) => {
        if (entries[0]?.isIntersecting) {
          reveal();
          observer.disconnect();
        }
      },
      // Fires slightly before the element is fully on screen, so content
      // appears as you scroll rather than popping in late.
      { rootMargin: '0px 0px -12% 0px' },
    );
    observer.observe(node);

    // The floor. If the observer never reports, show the content regardless.
    const failOpen = setTimeout(reveal, 1200);

    return () => {
      observer.disconnect();
      clearTimeout(failOpen);
    };
  }, []);

  return (
    <div
      ref={ref}
      className={className}
      style={{
        opacity: shown ? 1 : 0,
        transform: shown ? 'none' : `translateY(${y}px)`,
        transition: `opacity ${duration}s ${EASE} ${delay}s, transform ${duration}s ${EASE} ${delay}s`,
        willChange: shown ? undefined : 'opacity, transform',
      }}
    >
      {children}
    </div>
  );
}
