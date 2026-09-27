export function loginRedirect(status, requestPath, location) {
  if (status !== 401 || requestPath.startsWith('/auth/')) return null;
  if (location.pathname === '/login/' || location.pathname === '/login' || location.pathname === '/signup/' || location.pathname === '/signup') return null;

  const next = `${location.pathname}${location.search}`;
  return `/login/?next=${encodeURIComponent(next)}`;
}

export function postLoginDestination(search, origin) {
  const requested = new URLSearchParams(search).get('next');
  if (!requested || !requested.startsWith('/') || requested.startsWith('//') || requested.includes('\\')) return '/';

  try {
    const destination = new URL(requested, origin);
    if (destination.origin !== origin || ['/login', '/signup'].includes(destination.pathname.replace(/\/$/, ''))) return '/';
    return `${destination.pathname}${destination.search}${destination.hash}`;
  } catch {
    return '/';
  }
}
