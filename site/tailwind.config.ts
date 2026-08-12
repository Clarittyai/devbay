import type { Config } from 'tailwindcss';

/**
 * The Claritty marketing palette, lifted from clarity-website/tailwind.config.js
 * so this site inherits the brand rather than approximating it. Only the parts
 * this page uses are carried over.
 *
 * Two additions specific to devbay, both borrowed from elsewhere in the brand
 * rather than invented: `surface` is the platform's `--surface-deep` (#0d1117),
 * the always-dark code ground, and the `term` set names the four colours devbay
 * itself prints with (see the ANSI helpers in cmd/devbay/main.go) so terminal
 * output on the page means what it means in a real shell.
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
        // Interactive brand colour, shared with app.claritty.ai.
        accent: {
          DEFAULT: '#5B7FFF',
          // #5B7FFF at 68% lightness only reaches 3.54:1 against white, so
          // anything with white text on it uses 600.
          600: '#2957FF',
          700: '#4338CA',
        },
        canvas: '#FBF9F5',
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
