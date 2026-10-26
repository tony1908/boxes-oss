import type { Metadata } from 'next';
import { IBM_Plex_Mono, IBM_Plex_Sans } from 'next/font/google';
import './styles.css';

const plexSans = IBM_Plex_Sans({ subsets: ['latin'], weight: ['400', '500', '600'], variable: '--font-plex-sans' });
const plexMono = IBM_Plex_Mono({ subsets: ['latin'], weight: ['400'], variable: '--font-plex-mono' });

export const metadata: Metadata = { title: 'Paddock — machines for agents', description: 'Manage self-hosted agent sandboxes, environments, and snapshots.', icons: { icon: '/icon.svg', apple: '/apple-icon.svg' } };

export default function RootLayout({ children }: Readonly<{ children: React.ReactNode }>) {
  return <html lang="en" className={`${plexSans.variable} ${plexMono.variable}`}><body>{children}</body></html>;
}
