export function loginRedirect(
  status: number,
  requestPath: string,
  location: Pick<Location, 'pathname' | 'search'>,
): string | null;

export function postLoginDestination(search: string, origin: string): string;
