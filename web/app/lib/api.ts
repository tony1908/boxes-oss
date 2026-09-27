import { loginRedirect } from './auth-redirect.mjs';

export class APIError extends Error {
  constructor(message: string, readonly status: number) {
    super(message);
    this.name = 'APIError';
  }
}

export async function apiResponse(path: string, init: RequestInit = {}): Promise<Response> {
  const headers = new Headers(init.headers);
  headers.set('Accept', 'application/json');
  if (init.body && !(init.body instanceof FormData)) headers.set('Content-Type', 'application/json');

  const response = await fetch(`/api/v1${path}`, {
    ...init,
    headers,
    credentials: 'same-origin',
  });

  if (!response.ok) {
    let message = `Request failed (${response.status})`;
    try {
      const payload = (await response.json()) as { error?: { message?: string } };
      message = payload.error?.message ?? message;
    } catch {
      // Keep the useful status message when the server response isn't JSON.
    }
    const redirect = loginRedirect(response.status, path, window.location);
    if (redirect) {
      window.location.assign(redirect);
      message = '';
    }
    throw new APIError(message, response.status);
  }

  return response;
}

export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const response = await apiResponse(path, init);
  if (response.status === 204) return undefined as T;
  return (await response.json()) as T;
}

export function jsonBody(value: unknown): string {
  return JSON.stringify(value);
}
