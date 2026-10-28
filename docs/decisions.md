# Decisions log

Each entry: context, choice, reason. Newest at the bottom. Items in the spec's
"Decisions for v1" are not repeated here.

## D001 Module path

- Context: the repo's remote is `github.com/Richard-Sen27/mycelium`.
- Choice: module path `github.com/richard-sen27/mycelium` (lowercase).
- Reason: Go module paths are case-sensitive and lowercase is conventional; GitHub resolves either case. Change it before publishing if the repo moves.

## D002 Node ↔ hub transport is NATS over mTLS from phase 1

- Context: the spec says a node opens one outbound QUIC/WebSocket connection and that the bus is embedded NATS (phase 2). Building a separate WebSocket transport in phase 1 and replacing it in phase 2 means two transports.
- Choice: the hub embeds the NATS server from phase 1. Nodes connect outbound to it over TLS (TCP) with their node certificate as the client certificate. Hub-to-node control and node-to-hub telemetry/RPC ride on NATS subjects with per-identity permissions. Phase 2 adds JetStream streams, the envelope and channels on the same connection.
- Reason: one connection, built-in reconnect, request-reply and per-subject ACLs. NATS also has a WebSocket listener that can be switched on later for hostile networks without changing any client code.

## D003 Generated code lives in `gen/` and is committed

- Context: `proto/` holds definitions; CI on three OSes should not need buf or protoc plugins to build.
- Choice: `buf generate` writes Go code to `gen/mycel/v1` (protobuf) and `gen/mycel/v1/mycelv1connect` (Connect). Generated files are committed; a lint job regenerates and fails on drift.
- Reason: `go build ./...` works from a clean checkout on every runner.

## D004 SQLite via `modernc.org/sqlite`

- Context: the hub needs zero-ops state; cross-compiling with CGO is painful.
- Choice: pure-Go SQLite driver `modernc.org/sqlite`.
- Reason: static binaries for all three platforms without a C toolchain.

## D005 Join token format

- Context: the token must carry the hub address and the CA fingerprint, be single-use and short-lived.
- Choice: `mcl1_` + base64url(protobuf `JoinTokenPayload{hub_url, token_id, secret, ca_fingerprint}`). The hub stores only `sha256(secret)`, the name, caps ceiling, owner, expiry and used-at.
- Reason: self-describing, versioned, copy-pasteable; the secret never sits in the database in clear.

## D006 One daemon command, roles from data directory

- Context: "roles are configuration, never a different install"; the hub machine is also a node.
- Choice: `mycel daemon` starts every role whose state exists under the data directory: `hub/` (created by `mycel hub init`) and `node/` (created by `mycel join`). `mycel hub init` also enrols the local machine as a node, signed in-process, so the hub machine is a node without a network round trip.
- Reason: one process per machine, one command to run it, and the hub laptop is a node for free.

## D007 Identity material and directories

- Context: one config file per node in the OS config dir; data in the OS data dir.
- Choice: config at `<UserConfigDir>/mycel/config.yaml`, data at `<UserDataDir>/mycel/` where `UserDataDir` is `~/Library/Application Support` (macOS), `%LOCALAPPDATA%` (Windows), `$XDG_DATA_HOME` or `~/.local/share` (Linux). `MYCEL_HOME=<dir>` overrides both to `<dir>/config.yaml` and `<dir>/data/`, which the multi-process tests use. Keys are ECDSA P-256 PEM files with mode 0600. Secure Enclave and TPM backing is a later addition behind the same `identity` API.
- Reason: standard locations, trivial isolation for tests, and a single override.

## D008 Ports

- Context: every node serves the UI on `localhost:7777`; the hub also serves its API and the bus.
- Choice: hub API + UI on `:7777` (HTTPS, hub CA-signed server cert), bus (NATS TLS) on `:7778`. Ordinary nodes serve the local UI on `127.0.0.1:7777` over plain HTTP. All ports are flags.
- Reason: one well-known UI port everywhere; the bus port is only ever reached by `mycel` itself and is returned by the enrol call, so nobody types it.

## D009 Human and CLI authentication in phase 1

- Context: the spec wants passkeys for humans in the UI and scoped API keys; phase 1 has only a CLI.
- Choice: API keys (`mck_` prefix, stored hashed) bound to a user; `mycel hub init` creates the first admin and an admin key; `mycel user add` creates members with their own key. Passkeys come with the UI phase. Keys carry a scope list; phase 1 keys have the single scope `user`, meaning "act as this user with their role".
- Reason: the smallest thing that gives admin and member roles real meaning before the UI exists.
