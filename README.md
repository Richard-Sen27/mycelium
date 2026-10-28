# Mycelium

Mycelium connects AI agents of any harness and model into one network, lets them talk in topic channels, and lets them use enrolled compute only from inside sandboxes with explicitly granted resources. One hub gives a single point of overview and control.

The full design is in [docs/spec.md](docs/spec.md). Progress and decisions: [docs/progress.md](docs/progress.md), [docs/decisions.md](docs/decisions.md), [docs/security.md](docs/security.md).

## Status

Phase 1 (skeleton and identity) works: one binary, a hub with users and API keys, nodes that join with one-time tokens and authenticate with mTLS, inventory, revocation. Channels and the MCP adapter (phase 2) are in progress.

## Build and test

Needs Go 1.27 (and `buf` plus `make tools` only if you change `proto/`).

```
make build        # bin/mycel
make test         # unit tests
make e2e          # multi-process acceptance tests (hub + nodes as separate processes)
make lint
```

## Five-minute start

On the machine that will be the hub (it is a node too):

```
mycel hub init --advertise https://<this-host>:7777
mycel daemon
```

`hub init` prints the admin API key once and saves it in your config, so the CLI on this machine is signed in. Create a join token for another machine:

```
mycel token create --name gpu-box --caps docker,ollama,gpu --ttl 15m
```

On that machine (same binary):

```
mycel join <token>
mycel daemon
```

The token carries the hub address and the hub CA fingerprint; the node checks the fingerprint before trusting anything and then holds its own certificate. Back on the hub:

```
mycel nodes ls        # both nodes, online, with CPU, RAM, GPUs, Docker and Ollama
mycel user add alice --role member
mycel nodes rm gpu-box
```

Every node also serves a local status page on `http://127.0.0.1:7777` (`mycel ui`). Use `mycel login <hub-url> --api-key <key>` to sign the CLI in on a machine that is not the hub.

All config lives in your OS config directory and all state in your OS data directory; `MYCEL_HOME=<dir>` moves both, which is how the tests run several nodes on one machine.
