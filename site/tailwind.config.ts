import type { Config } from 'tailwindcss';

/**
 * devbay's own palette.
 *
 * Deliberately not the parent brand's indigo: devbay is an open-source tool
 * that stands on its own, and a developer deciding whether to install a CLI
 * should not have to work out whose product it is first.
 *
 * The accent is a deep teal for one concrete reason beyond taste. Terminal
 * output is the main visual on this page, and devbay prints in green for ok,
 * yellow for a note and red for a failure. A brand colour drawn from any of
 * those would make the output ambiguous, so the accent sits where none of them
 * are. `surface` is the always-dark code ground; `term` names the four colours
 * devbay actually prints with, from the ANSI helpers in cmd/devbay/main.go.
 */
const config: Config = {
  content: ['./app/**/*.{ts,tsx}', './components/**/*.{ts,tsx}', './lib/**/*.{ts,tsx}'],
  theme: {
    screens: {
      sm: '640px',
      md: '920px',
      lg: '1024px',
      xl: '1280px',
      '2xl': '1536px',
    },
    extend: {
      colors: {
        accent: {
          DEFAULT: '#0D9488',
          // White text needs 600 or darker: DEFAULT alone is 3.1:1 on white.
          600: '#0B7268',
          700: '#095A52',
          // For the dark surface, where the deep teal would disappear.
          bright: '#5EEAD4',
        },
        ink: '#0F1417',
        canvas: '#FFFFFF',
        tint: '#F5F8F8',
        // The always-dark surface for code and terminal output.
        surface: {
          DEFAULT: '#0d1117',
          raised: '#161b22',
          line: '#21262d',
          text: '#c9d1d9',
          dim: '#7d8590',
        },
        // What devbay's own output colours mean.
        term: {
          ok: '#3fb950',
          warn: '#d29922',
          bad: '#f85149',
          accent: '#79c0ff',
        },
        purple: { DEFAULT: '#AF52DE' },
        teal: { DEFAULT: '#5AC8FA' },
        green: { DEFAULT: '#34C759' },
        orange: { DEFAULT: '#FF9500' },
      },
      fontFamily: {
        sans: [
          '-apple-system',
          'BlinkMacSystemFont',
          'SF Pro Display',
          'Inter',
          'system-ui',
          'sans-serif',
        ],
        mono: ['SF Mono', 'Monaco', 'Cascadia Code', 'Roboto Mono', 'monospace'],
      },
      borderRadius: {
        xl: '1rem',
        '2xl': '1.5rem',
        '3xl': '2rem',
      },
      maxWidth: {
        prose: '68ch',
      },
    },
  },
  plugins: [],
};

export default config;
