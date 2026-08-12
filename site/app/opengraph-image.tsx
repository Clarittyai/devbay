import { ImageResponse } from 'next/og';

/**
 * The social card. Generated rather than a checked-in PNG so it cannot drift
 * from the headline it quotes.
 */
export const runtime = 'nodejs';
export const alt = 'devbay — run every branch for real';
export const size = { width: 1200, height: 630 };
export const contentType = 'image/png';

export default async function Image() {
  return new ImageResponse(
    (
      <div
        style={{
          height: '100%',
          width: '100%',
          display: 'flex',
          flexDirection: 'column',
          justifyContent: 'space-between',
          background: '#0F1417',
          padding: '72px 80px',
          fontFamily: 'ui-sans-serif, system-ui, sans-serif',
        }}
      >
        <div style={{ display: 'flex', alignItems: 'center', gap: 18 }}>
          <div style={{ display: 'flex', alignItems: 'flex-end', gap: 7, height: 46 }}>
            <div style={{ width: 11, height: 30, borderRadius: 4, background: 'rgba(255,255,255,0.34)' }} />
            <div style={{ width: 11, height: 44, borderRadius: 4, background: '#2DD4BF' }} />
            <div style={{ width: 11, height: 30, borderRadius: 4, background: 'rgba(255,255,255,0.34)' }} />
            <div style={{ width: 8, height: 20, borderRadius: 4, background: 'rgba(255,255,255,0.34)' }} />
          </div>
          <div style={{ fontSize: 38, fontWeight: 600, color: '#ffffff', letterSpacing: -1 }}>devbay</div>
        </div>

        <div
          style={{
            display: 'flex',
            fontSize: 76,
            fontWeight: 700,
            color: '#ffffff',
            letterSpacing: -3,
            lineHeight: 1.08,
            maxWidth: 900,
          }}
        >
          Stop guessing. Run every branch for real.
        </div>

        <div style={{ display: 'flex', fontSize: 28, color: '#8b9899', letterSpacing: -0.5 }}>
          Its own containers, database, ports and browser origin. Five at once.
        </div>
      </div>
    ),
    size,
  );
}
