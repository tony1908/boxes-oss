# Desktop sessions

Opening the Desktop tab creates or reuses a desktop session for the sandbox. Sessions last eight hours by default and can be configured with `--desktop-session-ttl` (or `BOXES_DESKTOP_SESSION_TTL`). Public desktop sessions retain a seven day lifetime.

The control plane holds the session token in memory and sends only its hash and expiry to the node agent. A server restart forgets desktop sessions; the next Desktop tab visit creates a new session. `GET /api/v1/sandboxes/{id}/desktop` reports the current URL and expiry without creating a session and returns 404 when no current session exists.

The browser keeps the desktop iframe mounted while switching between tabs on the same sandbox. It renews sessions within 30 minutes of expiry and changes the iframe URL only when the token changes. Stopping or deleting a sandbox removes its session.
