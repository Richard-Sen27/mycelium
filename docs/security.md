# Security notes

Threat model and what is not covered. Grows with each phase; see docs/spec.md
"Security, policy and control" for the designed-for threats.

## Phase 1

Covered:

- Node identity by mTLS. Private keys are generated on the node and never leave it.
- Join tokens are single-use and short-lived; the hub stores only a hash of the secret.
- The join token carries the hub CA fingerprint; the node verifies the CA before trusting anything.
- Revocation: a removed node is dropped from the bus within seconds and its certificate is refused on reconnect and on renewal.
- Agent tokens are short-lived, HMAC-signed, and bound to the node (and sandbox) they were issued to; a token copied elsewhere does not verify against another connection.
- API keys are stored hashed. Role checks go through the Policy interface and default to deny.
- Per-node NATS permissions: a node can only publish telemetry and RPC requests under its own name and only subscribe to its own control subject and inbox; publish and subscribe never overlap on a subject (reply-permission escalation, see D012). The hub validates the reply subject of every node RPC.

Not covered yet:

- Hardware-backed node keys (Secure Enclave, TPM): planned behind the identity API.
- Passkeys for humans: phase 6.
- Rate limiting of the enrol endpoint is a simple per-IP token bucket; there is no global limit yet.
- Revoking an agent takes effect at its next token refresh: an already issued agent token stays cryptographically valid until it expires (at most one hour). Removing a node drops its bus connection immediately, which is what carries agent traffic.
- Audit, revoked-serial and issued-certificate tables are not pruned beyond expired serials at startup.
- An individual API key cannot be revoked yet; removing the user revokes all of their keys (a `RevokeApiKey` RPC comes with the phase 2 contracts).
- Hub TLS certificates from Let's Encrypt: phase 7. Until then the hub serves a CA-signed server certificate and clients pin the CA.
