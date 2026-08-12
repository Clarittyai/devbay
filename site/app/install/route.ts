/**
 * devbay.claritty.ai/install
 *
 * Serves the installer from the repository so the command on the page is short
 * enough to read, remember and type. It is a pass-through, not a copy: the
 * script comes from main at request time, so there is one installer and it
 * cannot drift from the one the README gives you.
 *
 * Cached for an hour at the edge, and the response is plain text so that
 * anybody who is sensibly suspicious of piping a URL into a shell can open it
 * in a browser and read it first.
 */

const SOURCE = 'https://raw.githubusercontent.com/Clarittyai/devbay/main/install.sh';

export const revalidate = 3600;

export async function GET() {
  const upstream = await fetch(SOURCE, { next: { revalidate: 3600 } });

  if (!upstream.ok) {
    // Fail loudly rather than piping half a script into a shell.
    return new Response(
      `#!/bin/sh\necho "devbay: could not fetch the installer (${upstream.status})." >&2\n` +
        `echo "Install from ${SOURCE} instead." >&2\nexit 1\n`,
      { status: 502, headers: { 'Content-Type': 'text/plain; charset=utf-8' } },
    );
  }

  return new Response(await upstream.text(), {
    headers: {
      'Content-Type': 'text/plain; charset=utf-8',
      'Cache-Control': 'public, max-age=0, s-maxage=3600, stale-while-revalidate=86400',
    },
  });
}
