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
- Per-node NATS permissions: a node can only publish and subscribe under its own subjects.

Not covered yet:

- Hardware-backed node keys (Secure Enclave, TPM): planned behind the identity API.
- Passkeys for humans: phase 6.
- Rate limiting of the enrol endpoint.
- Hub TLS certificates from Let's Encrypt: phase 7. Until then the hub serves a CA-signed server certificate and clients pin the CA.
