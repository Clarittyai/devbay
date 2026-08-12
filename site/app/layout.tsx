import type { Metadata, Viewport } from 'next';
import './globals.css';

const title = 'devbay — run every branch for real';
const description =
  'Every branch gets its own containers, database, ports and browser origin. Five at once, on your machine, walled off from each other. Nothing runs without your OK.';
const url = 'https://devbay.claritty.ai';

export const metadata: Metadata = {
  metadataBase: new URL(url),
  title,
  description,
  applicationName: 'devbay',
  authors: [{ name: 'Claritty', url: 'https://claritty.ai' }],
  keywords: [
    'devbay',
    'development environments',
    'coding agents',
    'git worktree',
    'docker',
    'isolated environments',
    'MCP',
  ],
  alternates: { canonical: url },
  openGraph: {
    type: 'website',
    url,
    siteName: 'devbay',
    title,
    description,
  },
  twitter: {
    card: 'summary_large_image',
    site: '@ClarittyAI',
    title,
    description,
  },
  robots: { index: true, follow: true },
};

export const viewport: Viewport = {
  themeColor: '#FBF9F5',
  width: 'device-width',
  initialScale: 1,
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
