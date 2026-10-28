package hub

// Phase 7 contract: moving a hub (spec "Moving the hub: mycel hub export
// and mycel hub import move the hub's state to another machine; nodes
// follow via a signed redirect"). Owned by P7-move.
//
// Export (`mycel hub export [--out <file>] [--passphrase-file <f>]`, hub
// running or stopped) writes one file, default `hub-<trust domain>-<date>.mycelhub`:
//
//	magic "MYCELHUB1" | argon2id salt (16) | nonce (12) | AES-256-GCM(tar.gz)
//
// The key is argon2id(passphrase, salt, t=3, m=64 MiB, p=4). The passphrase
// is read from --passphrase-file or the terminal (twice); it is never
// taken from a flag. The archive holds: hub.yaml, ca.crt, ca.key,
// server.crt, server.key, agent-token.key, hub.db (a `VACUUM INTO`
// snapshot taken through the running hub's local API when it runs, else
// the file), vapid.json and relay.url if present, acme/ cache, and a
// JetStream snapshot of every stream (CH, AUDIT, JOBLOG) taken with the
// JetStream snapshot API when the hub runs, else the raw jetstream/
// directory. The export is audited ("hub.export").
//
// Import (`mycel hub import <file> --advertise <url> [--bus-advertise]
// [--api-listen] [--bus-listen]`, on the new machine, no hub initialised
// there yet) restores everything into the new hub directory, rewrites the
// listen and advertise settings in hub.yaml, re-issues server.crt from the
// CA for the new advertise host, restores the JetStream streams, and
// prints the next step. The trust domain, CA, users, API keys, nodes,
// agents, channels, history, grants and join tokens are all preserved, so
// every node certificate and API key keeps working.
//
// Redirect. `mycel hub redirect <new url> [--bus <host:port>]` on the OLD
// hub (or `mycel hub import --redirect-from <old hub url>`, which calls it
// over the API with the admin key) does three things: it signs a Redirect
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
// newer than any redirect it followed before, persists the new hub and bus
// URLs in its node state, reconnects, and logs it. The CLI, on a "hub
// moved" error, verifies the header the same way (CA from config.yaml),
// prints the new URL and updates config.yaml when run with --follow (or
// after a y/N prompt on a terminal). `mycel join` with a token that names
// the old hub follows the redirect automatically, verifying the signature
// against the token's CA fingerprint.
//
// The redirect payload that is signed (identity.RedirectPayload) is:
//
//	"mycel-redirect/1\n" + trust domain + "\n" + hub_url + "\n" + bus_url + "\n" + issued_at (unix seconds, decimal) + "\n"
