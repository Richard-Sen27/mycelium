package hub

// Phase 7 contract: moving a hub (spec "Moving the hub: mycel hub export
// and mycel hub import move the hub's state to another machine; nodes
// follow via a signed redirect"). Owned by P7-move.
//
// Export (`mycel hub export [--out <file>] [--passphrase-file <f>]
// [--exclude-artifacts]`, hub running or stopped) writes one file, default
// `hub-<trust domain>-<date>.mycelhub`, streamed (neither export nor
// import holds the archive in memory):
//
//	header  = magic "MYCELHUB2" | argon2id salt (16) | base nonce (12)
//	then    = AES-256-GCM segments of 1 MiB plaintext (the last may be shorter or empty)
//	nonce   = base nonce XOR the big-endian segment counter (last 8 bytes)
//	AAD     = header | 0x01 on the last segment, 0x00 on every other
//
// so the header is authenticated in every segment, segments cannot be
// reordered, and a file cut at a segment boundary fails. The key is
// argon2id(passphrase, salt, t=3, m=64 MiB, p=4); passphrases have at least
// 12 characters. The passphrase is read from --passphrase-file or the
// terminal (twice); it is never taken from a flag. The archive (tar.gz)
// holds: hub.yaml, ca.crt, ca.key, server.crt, server.key,
// agent-token.key, hub.db (a `VACUUM INTO` snapshot taken through the
// running hub's local API when it runs, else from the file under
// hub.lock), vapid.json and relay.url if present, acme/ cache, artifacts/
// (unless --exclude-artifacts) and jobs/, and a JetStream snapshot of every
// stream (CH, AUDIT, JOBLOG) taken with the JetStream snapshot API when
// the hub runs, else the raw jetstream/ directory. The export is audited
// ("hub.export", with its outcome).
//
// Import (`mycel hub import <file> --advertise <url> [--bus-advertise]
// [--api-listen] [--bus-listen]`, on the new machine; the hub directory
// must be missing or empty) restores everything into a fresh temporary
// directory next to it, rewrites the listen and advertise settings in
// hub.yaml, re-issues server.crt from the CA for the new advertise host,
// restores the JetStream streams, renames the result into place, and
// prints the next step. relay.url is never restored: the new hub needs
// its own relay token. The trust domain, CA, users, API keys, nodes,
// agents, channels, history, grants and join tokens are all preserved, so
// every node certificate and API key keeps working.
//
// Redirect. `mycel hub redirect <new url> [--bus <host:port>] [--force]`
// on the OLD hub first checks the target (`/healthz` over TLS verified
// against the hub CA, and a TLS handshake with the bus) and refuses when it
// fails unless --force (`mycel hub import --redirect-from <old hub url>`
// calls the API with the admin key and skips the check, since the new hub
// starts only after the import). It then does three things: it signs a Redirect
// with the CA key (identity.SignRedirect), queues it to every node at the
// head of its control queue (including nodes that are offline: it is sent
// when they reconnect), and switches the old hub to "moved" mode, where
// every RPC fails with FailedPrecondition "hub moved to <url>" carrying the
// signed redirect in the `Mycel-Redirect` response header (base64 of the
// Redirect proto), posts and jobs are refused, and the node control
// channel only sends redirects. Moved mode is persisted in hub.yaml
// (`moved_to`), survives restarts, and is audited.
//
// Following. A node receiving Redirect verifies the signature against its
// pinned CA (identity.VerifyRedirect), checks issued_at is within 24 h and
// newer than any redirect it followed before, connects to the new bus
// (retrying with backoff while keeping its current connection), and only
// then persists the new hub and bus URLs in its node state and logs it.
// The CLI, on a "hub moved" error, verifies the header the same way (CA
// from config.yaml), refuses one older than the last it followed, prints
// the new URL and updates config.yaml when run with --follow-redirect (or
// after a y/N prompt on a terminal). `mycel join` with a token that names
// the old hub follows the redirect automatically, verifying the signature
// against the token's CA fingerprint.
//
// The redirect payload that is signed (identity.RedirectPayload) is:
//
//	"mycel-redirect/1\n" + trust domain + "\n" + hub_url + "\n" + bus_url + "\n" + issued_at (unix seconds, decimal) + "\n"
