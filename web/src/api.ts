// Plain fetch() against the daemon. The credential rides in the session
// cookie, so every call is same-origin and carries no header of its own; the
// one exception is signIn, which presents the token exactly once.

export class ApiError extends Error {
  constructor(
    message: string,
    public readonly status: number,
  ) {
    super(message);
  }
}

/** Fired when any call comes back 401, so the shell can fall back to login. */
export const unauthorized = new EventTarget();

export async function api<T>(method: string, path: string, body?: unknown): Promise<T | null> {
  const headers: Record<string, string> = {};
  const init: RequestInit = { method, headers, credentials: 'same-origin' };
  if (body !== undefined) {
    // application/json is the CSRF control on the daemon's side: a cross-site
    // form cannot send it.
    headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  const res = await fetch(path, init);
  if (res.status === 401) {
    unauthorized.dispatchEvent(new Event('unauthorized'));
    throw new ApiError('unauthorized', 401);
  }
  if (res.status === 204) return null;
  const text = await res.text();
  let data: unknown = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = null;
  }
  if (!res.ok) {
    const err = (data as { error?: string } | null)?.error;
    throw new ApiError(err || `${res.status}`, res.status);
  }
  return data as T;
}

/** Exchanges a bearer token for the session cookie. True when accepted. */
export async function signIn(token: string): Promise<boolean> {
  const res = await fetch('/session', {
    method: 'POST',
    headers: { Authorization: 'Bearer ' + token },
    credentials: 'same-origin',
  });
  return res.status === 204;
}

export async function signOut(): Promise<void> {
  await fetch('/session', { method: 'DELETE', credentials: 'same-origin' });
}
