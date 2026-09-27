import type { SVGProps } from 'react';

export type IconName = 'sandboxes'|'templates'|'environments'|'nodes'|'api-keys'|'webhooks'|'settings'|'copy'|'external-link'|'menu'|'close'|'plus'|'check'|'chevron-down'|'desktop';
const paths: Record<IconName, React.ReactNode> = {
 sandboxes:<><rect x="2" y="2" width="8" height="8"/><rect x="6" y="6" width="8" height="8"/></>,
 templates:<><path d="M4 2.5h8v11l-4-2-4 2z"/><rect x="2" y="4" width="2" height="9" rx=".5"/></>,
 environments:<><path d="M3 3v10M8 3v10M13 3v10"/><path d="M1.5 6h3M6.5 10h3M11.5 5h3"/></>,
 nodes:<><rect x="2" y="2" width="12" height="4" rx="1"/><rect x="2" y="10" width="12" height="4" rx="1"/><path d="M4 4h.01M4 12h.01M7 4h5M7 12h5"/></>,
 'api-keys':<><circle cx="5.5" cy="8.5" r="3"/><path d="m8 6 5.5-3.5M10 5l2 2M11.5 3.5l2 2"/></>,
 webhooks:<><path d="M3 4h3a3 3 0 0 1 3 3v2a3 3 0 0 0 3 3h1"/><path d="m11 9 3 3-3 3"/><circle cx="3" cy="4" r="1"/></>,
 settings:<><circle cx="8" cy="8" r="2.5"/><path d="M8 1.5v1.5M8 13v1.5M1.5 8H3M13 8h1.5M3.4 3.4l1.1 1.1m7 7 1.1 1.1m0-9.2-1.1 1.1m-7 7-1.1 1.1"/></>,
 copy:<><rect x="5" y="5" width="8" height="9" rx="1"/><path d="M10 3V2.5A1.5 1.5 0 0 0 8.5 1h-5A1.5 1.5 0 0 0 2 2.5v8A1.5 1.5 0 0 0 3.5 12H4"/></>,
 'external-link':<><path d="M9 3h4v4M13 3 7 9"/><path d="M12 9v4H3V4h4"/></>, menu:<path d="M2 4h12M2 8h12M2 12h12"/>, close:<path d="m3 3 10 10M13 3 3 13"/>, plus:<path d="M8 2v12M2 8h12"/>, check:<path d="m2.5 8.5 3.5 3 7.5-8"/>, 'chevron-down':<path d="m3 6 5 5 5-5"/>, desktop:<><rect x="1.5" y="2" width="13" height="9" rx="1"/><path d="M8 11v3m-3 0h6"/></>
};
export function Icon({ name, size = 16, ...props }: SVGProps<SVGSVGElement> & { name: IconName; size?: number }) {
 return <svg viewBox="0 0 16 16" width={size} height={size} fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false" {...props}>{paths[name]}</svg>;
}
