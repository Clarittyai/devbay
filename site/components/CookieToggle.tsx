'use client';

import { useState } from 'react';

/**
 * The one thing on this page that is worth more than all the copy.
 *
 * Two bays of the same application, each setting a session cookie. Reach them
 * on loopback ports and the second bay is logged in as the first, because
 * browsers key cookies by host and ignore the port. Reach them on their own
 * hostnames and they are separate.
 *
 * The strings are what examples/cookie-isolation actually printed in Chrome on
 * 2026-08-11. Nothing here is illustrative.
 */

type Mode = 'ports' | 'hostnames';

const data: Record<
  Mode,
  { label: string; alpha: { host: string; cookie: string }; beta: { host: string; cookie: string } }
> = {
  ports: {
    label: 'On loopback ports',
    alpha: { host: '127.0.0.1:40160', cookie: 'session=alpha-session' },
    beta: { host: '127.0.0.1:41540', cookie: 'session=alpha-session' },
  },
  hostnames: {
    label: 'On bay hostnames',
    alpha: { host: 'alpha.cookies.localhost', cookie: 'session=alpha-session' },
    beta: { host: 'beta.cookies.localhost', cookie: '(none)' },
  },
};

export default function CookieToggle() {
  const [mode, setMode] = useState<Mode>('ports');
  const view = data[mode];
  const leaked = mode === 'ports';

  return (
    <div>
      <div
        role="radiogroup"
        aria-label="How the two bays are reached"
        className="inline-flex rounded-full border border-gray-300 bg-white p-1"
      >
        {(Object.keys(data) as Mode[]).map((key) => (
          <button
            key={key}
            role="radio"
            aria-checked={mode === key}
            onClick={() => setMode(key)}
            className={`rounded-full px-4 py-2 text-sm font-semibold transition-colors ${
              mode === key ? 'bg-gray-900 text-white' : 'text-gray-600 hover:text-gray-900'
            }`}
          >
            {data[key].label}
          </button>
        ))}
      </div>

      <div className="mt-5 space-y-3">
        <Response
          bay="alpha"
          host={view.alpha.host}
          cookie={view.alpha.cookie}
          note="logged in here"
        />
        <Response
          bay="beta"
          host={view.beta.host}
          cookie={view.beta.cookie}
          note={leaked ? "logged in as alpha, without asking" : 'a separate session'}
          leaked={leaked}
          clean={!leaked}
        />
      </div>

      <p className="mt-5 text-sm leading-relaxed text-gray-600">
        {leaked ? (
          <>
            Bay beta received bay alpha&apos;s session. Nothing about either application is wrong and
            nothing appears in either log.
          </>
        ) : (
          <>
            Bay beta received nothing. Same two applications, same machine, same moment. The only
            change is the address.
          </>
        )}
      </p>
    </div>
  );
}

function Response({
  bay,
  host,
  cookie,
  note,
  leaked = false,
  clean = false,
}: {
  bay: string;
  host: string;
  cookie: string;
  note: string;
  leaked?: boolean;
  clean?: boolean;
}) {
  return (
    <div
      className={`overflow-x-auto rounded-2xl border p-4 font-mono text-[13px] leading-relaxed ${
        leaked
          ? 'border-red-300 bg-red-50'
          : clean
            ? 'border-green-300 bg-green-50/70'
            : 'border-gray-200 bg-white'
      }`}
    >
      <div className="whitespace-pre text-gray-900">
        <span className="text-gray-600">bay=</span>
        {bay}
        <span className="text-gray-600"> host=</span>
        {host}
      </div>
      <div className="mt-1 whitespace-pre">
        <span className="text-gray-600">cookie=</span>
        <span className={leaked ? 'font-semibold text-red-700' : 'text-gray-900'}>{cookie}</span>
      </div>
      <div
        className={`mt-2 font-sans text-xs font-semibold ${
          leaked ? 'text-red-700' : clean ? 'text-green-700' : 'text-gray-600'
        }`}
      >
        {note}
      </div>
    </div>
  );
}
