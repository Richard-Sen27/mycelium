package hub

// Phase 6 contract: the hub's browser-facing surface (owned by P6-api).
//
// The single-page app from internal/web is served by the hub at "/" (every
// path that is not an API, /ui/*, /ca.pem, /healthz or /api/* route falls
// back to index.html). The minimal approvals page under /ui/* from phase 5
// stays as the no-JavaScript fallback and the API-key sign-in form.
//
// Unauthenticated:
//   GET /api/ui-config → {"mode":"hub","hub":"<external url>","rp_id":"<host>","passkeys":true|false,"push":true|false,"version":"..."}
//     (no secrets; the SPA uses it to pick its mode and the WebAuthn RP).
//
// Sessions. One cookie session mechanism serves /ui/* and the SPA: the
// HttpOnly, Secure, SameSite=Strict cookie from phase 5, 24 h, at most 8 per
// user. A session is created by POST /ui/login (API key, rate-limited) or a
// passkey login. The session's CSRF token is returned by GET /api/auth/me
// and must be sent as the `Mycel-Csrf` header on every cookie-authenticated
// request that is not a plain GET, including every Connect RPC (Connect uses
// POST). authMiddleware resolves a session cookie into the user only when
// the header matches and the Origin/Sec-Fetch-Site check passes (sameOrigin);
// otherwise the request proceeds unauthenticated. API keys in Authorization
// keep working for the CLI and need no CSRF header.
//
// Auth routes (JSON; 401 without a session unless noted; same rate limits as
// /ui/login for the unauthenticated ones):
//   GET    /api/auth/me                      → {"user_id","name","role","csrf","passkeys":[{"id","name","created_at","last_used_at"}]}
//   POST   /api/auth/logout                  → 204; ends the session
//   POST   /api/auth/passkey/register/begin  → WebAuthn PublicKeyCredentialCreationOptions (go-webauthn JSON); body {"name":"<label>"}
//   POST   /api/auth/passkey/register/finish → 201 {"id"}; body: the browser's credential JSON
//   POST   /api/auth/passkey/login/begin     → (no session) PublicKeyCredentialRequestOptions for discoverable credentials; body {} or {"user":"<name>"}
//   POST   /api/auth/passkey/login/finish    → (no session) 200 {"user_id","csrf"} and the session cookie; body: the assertion JSON
//   DELETE /api/auth/passkey/{id}            → 204; a user may not delete their last passkey while they have no API key
// Passkeys use github.com/go-webauthn/webauthn with RP id and origins from
// hub.yaml (`ui.rp_id`, `ui.origins`; defaults derived from the hub's
// external URL). Credentials are stored per user (id, public key, sign
// count, transports, name, created, last used); sign counts are checked.
// User verification is preferred, resident keys required (discoverable
// login). Challenges live 2 minutes, one pending per session or IP.
//
// Push (JSON, session required):
//   GET    /api/push/vapid          → {"public_key":"<base64url>"}
//   POST   /api/push/subscriptions  → 201; body: the PushSubscription JSON; at most 8 per user
//   DELETE /api/push/subscriptions  → 204; body {"endpoint"}
// VAPID keys are generated on first use and stored in <hub dir>/vapid.json
// (0600). The hub pushes {"type":"approval","grant_id","kind","node"} to the
// admins and the node owner when a grant becomes PENDING, and
// {"type":"freeze"} on a network freeze; never a reason text or a path.
// Library: github.com/SherClockHolmes/webpush-go. Failing endpoints (404/410)
// are removed.
//
// Node read scope. A request authenticated by a node certificate that carries
// the header `Mycel-Scope: network:read` acts as the node owner's user
// (role member, visibility of that owner) but may only call the read-only
// methods listed in readOnlyMethods (every List*/Get*/Whoami/FreezeStatus/
// ListProfiles/ListMessages/BlackboardGet/Watch/WatchEvents/Search/ListAudit
// plus GET /api/artifacts/{id}); anything else is PermissionDenied "node read
// scope" and audited. The daemon's local UI reaches the hub this way (see
// internal/daemon/localapi.go). Without the header a node certificate keeps
// its phase 1–5 node RPC rights and no user rights.
//
// EventService (proto/mycel/v1/ui.proto): WatchEvents fans out from the
// hub's row changes (one in-memory ring of the last 10 000 events or 1 h,
// whichever is smaller; after_seq older than the ring → FailedPrecondition
// "too old"), filtered per caller by the same visibility as the List RPCs;
// a heartbeat every 15 s; at most 4 streams per user and 1 per node scope.
// Search runs over SQLite (names, titles and message bodies via LIKE or
// FTS5) with the caller's visibility. ListAudit reads the AUDIT stream (or
// a SQLite mirror of it); members only see entries whose actor is
// themselves, their nodes or their agents; admins see everything.
//
// Security headers: every SPA response carries web.Headers; the Connect and
// JSON routes add `Cache-Control: no-store`. The hub's existing HTTP server
// timeouts apply; WatchEvents is exempt from the write timeout like Watch.
