import './globals.css';
import type { Metadata, Viewport } from 'next';

export const metadata: Metadata = { title: 'わが家のアプリ' };
export const viewport: Viewport = { width: 'device-width', initialScale: 1 };

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="ja">
      <body>
        <header className="top"><a href="/">🏠 わが家のアプリ</a></header>
        <main>{children}</main>
      </body>
    </html>
  );
}
