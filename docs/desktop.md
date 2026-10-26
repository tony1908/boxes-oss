# Desktop sessions

Opening the Desktop tab (or `paddock desktop <id>`) creates or reuses a desktop
session for the box. Sessions last eight hours by default; set the default with
`paddock-server --desktop-session-ttl` or `PADDOCK_DESKTOP_SESSION_TTL`. Public
sessions (`--public` on the CLI, or `public` on the API) last seven days.

A session is reused while it has more than 30 minutes left. Creating a session
for a box that already has one with a different public/private mode replaces
it.

## Storage and restarts

The server stores the session token encrypted in its database. On startup it
reloads unexpired sessions, so a server restart does not invalidate an active
desktop session. Expired sessions are dropped on load.

The control plane sends the local agent only a hash of the token together with
the session's host and expiry, never the token itself. The agent uses that hash
to validate requests from Traefik's forward-auth check.

## API

`POST /api/v1/sandboxes/{id}/desktop` creates or reuses a session and returns
its URL and expiry. `GET /api/v1/sandboxes/{id}/desktop` reports the current
URL and expiry without creating a session; it returns `404` with error code
`desktop_not_found` when no current session exists.

Stopping or deleting a box removes its desktop session.

## Browser behavior

The web UI keeps the desktop iframe mounted while switching between tabs for
the same box. It renews the session when less than 30 minutes remain.

## Forward authentication

Desktop routes sit behind Traefik and use forward-auth against the local agent
(`PADDOCK_FORWARD_AUTH_ADDR`, default `127.0.0.1:7001`). See
[self-hosting.md](self-hosting.md#traefik-and-desktop-authentication).
