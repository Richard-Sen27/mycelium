#!/bin/sh
# A five-minute tour of Mycelium on one machine: a hub (which is a node too)
# and a second node, each with its own MYCEL_HOME under a temporary
# directory. It creates two users, joins the node with a token, posts to a
# channel, runs a job on the second node, requests and approves a grant and
# prints the UI addresses. Everything is torn down on exit.
#
#   scripts/demo.sh [--keep] [--port 17777]
#
#   --keep        keep the hub and node running until Ctrl-C (to try the UI)
#   --port N      hub API port (N+1 is the bus port); default 17777
#
# The binary: $MYCEL_BIN, else a build of ./cmd/mycel with the fake sandbox
# backend when Go is available (the demo then needs no VM), else bin/mycel
# or mycel on PATH. With a binary without the fake backend the job runs in
# the real sandbox (Lima on macOS, gVisor on Linux) when that is installed,
# and is skipped otherwise.

set -eu

PORT=17777
KEEP=0
while [ $# -gt 0 ]; do
	case "$1" in
	--keep) KEEP=1; shift ;;
	--port)
		[ $# -ge 2 ] || { echo "demo.sh: --port needs a value" >&2; exit 2; }
		PORT="$2"
		shift 2
		;;
	-h | --help) sed -n '2,18p' "$0"; exit 0 ;;
	*) echo "demo.sh: unknown argument $1" >&2; exit 2 ;;
	esac
done
BUS_PORT=$((PORT + 1))

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d 2>/dev/null || mktemp -d -t mycel-demo)"
HUB_HOME="$WORK/hub"
NODE_HOME="$WORK/beta"
HUB_PID=""
NODE_PID=""

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
say() { printf '%s\n' "$*"; }
die() { printf 'demo.sh: %s\n' "$*" >&2; exit 1; }

cleanup() {
	status=$?
	trap - EXIT INT TERM
	for pid in $NODE_PID $HUB_PID; do
		kill "$pid" 2>/dev/null || true
	done
	for pid in $NODE_PID $HUB_PID; do
		wait "$pid" 2>/dev/null || true
	done
	if [ "$status" -ne 0 ] && [ -d "$WORK" ]; then
		for f in "$WORK"/hub.out "$WORK"/beta.out; do
			[ -f "$f" ] && { say "--- $(basename "$f") (last lines)"; tail -n 20 "$f"; }
		done
	fi
	rm -rf "$WORK"
	say ""
	say "demo: stopped the hub and the node and removed $WORK"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# json_field NAME: the first string value of "NAME" in the JSON on stdin.
json_field() {
	sed -n 's/.*"'"$1"'":"\([^"]*\)".*/\1/p' | head -n 1
}

# hub_cli runs mycel as the admin on the hub machine; as_user KEY runs it
# with another member's API key (same hub URL and CA from the hub's config).
hub_cli() { MYCEL_HOME="$HUB_HOME" "$BIN" "$@"; }
as_user() {
	key="$1"
	shift
	MYCEL_HOME="$HUB_HOME" MYCEL_API_KEY="$key" "$BIN" "$@"
}
# show prints a command (as a member would type it) and runs it as admin.
show() {
	line="\$ mycel"
	for a in "$@"; do
		case "$a" in
		*[!A-Za-z0-9_./:=@,-]*) line="$line '$a'" ;;
		*) line="$line $a" ;;
		esac
	done
	say "$line"
	hub_cli "$@"
}

# wait_ready FILE: wait for the daemon's JSON ready line.
wait_ready() {
	i=0
	while [ $i -lt 120 ]; do
		if grep -q '"local_addr"' "$1" 2>/dev/null; then
			grep '"local_addr"' "$1" | head -n 1
			return 0
		fi
		i=$((i + 1))
		sleep 0.5
	done
	die "the daemon did not get ready (see $1)"
}

port_free() {
	if command -v nc >/dev/null 2>&1 && nc -z 127.0.0.1 "$1" >/dev/null 2>&1; then
		return 1
	fi
	return 0
}

# --- the binary -------------------------------------------------------------
step "binary"
if [ -n "${MYCEL_BIN:-}" ]; then
	BIN="$MYCEL_BIN"
elif command -v go >/dev/null 2>&1 && [ -f "$ROOT/go.mod" ]; then
	say "building mycel with the fake sandbox backend (go build -tags fakesandbox)"
	(cd "$ROOT" && go build -tags fakesandbox -o "$WORK/mycel" ./cmd/mycel)
	BIN="$WORK/mycel"
elif [ -x "$ROOT/bin/mycel" ]; then
	BIN="$ROOT/bin/mycel"
elif command -v mycel >/dev/null 2>&1; then
	BIN="$(command -v mycel)"
else
	die "no mycel binary: set MYCEL_BIN, install Go 1.27, or run make build"
fi
say "using $BIN ($("$BIN" version))"
FAKE=0
if grep -q "internal/sandbox/fake" "$BIN" 2>/dev/null; then
	FAKE=1
	say "sandbox: the fake backend (CI-only, no isolation: fine for a demo on one machine)"
else
	say "sandbox: the real backend of this OS"
fi
port_free "$PORT" || die "port $PORT is in use (try --port)"
port_free "$BUS_PORT" || die "port $BUS_PORT is in use (try --port)"

# --- the hub ----------------------------------------------------------------
step "1. a hub (MYCEL_HOME=$HUB_HOME)"
# --no-mdns: a hub on loopback for one machine has nothing to announce on the network.
say "\$ mycel hub init --advertise https://127.0.0.1:$PORT --node-name hub-node --no-mdns"
init="$(hub_cli hub init --advertise "https://127.0.0.1:$PORT" --api-listen "127.0.0.1:$PORT" \
	--bus-listen "127.0.0.1:$BUS_PORT" --trust-domain demo --node-name hub-node --no-mdns --json 2>/dev/null)"
say "hub initialised; trust domain $(printf '%s' "$init" | json_field trust_domain), CA fingerprint $(printf '%s' "$init" | json_field ca_fingerprint | cut -c1-16)…"
say "(the admin API key is printed once by hub init; the demo keeps it in $HUB_HOME/config.yaml)"

hub_args="--json --local-addr 127.0.0.1:0 --log-file $HUB_HOME/logs/daemon.log"
[ "$FAKE" -eq 1 ] && hub_args="$hub_args --allow-fake-profile"
say "\$ mycel daemon"
# shellcheck disable=SC2086 # hub_args is a list of plain words
MYCEL_HOME="$HUB_HOME" "$BIN" daemon $hub_args >"$WORK/hub.out" 2>&1 &
HUB_PID=$!
hub_ready="$(wait_ready "$WORK/hub.out")"
HUB_LOCAL="$(printf '%s' "$hub_ready" | sed -n 's/.*"node":{"local_addr":"\([^"]*\)".*/\1/p')"
say "hub and its node are up (pid $HUB_PID; hub API https://127.0.0.1:$PORT, local UI http://$HUB_LOCAL)"

# --- users ------------------------------------------------------------------
step "2. two members"
say "\$ mycel user add alice --role member"
ALICE_KEY="$(hub_cli user add alice --role member --json | json_field api_key)"
say "\$ mycel user add bob --role member"
BOB_KEY="$(hub_cli user add bob --role member --json | json_field api_key)"
[ -n "$ALICE_KEY" ] && [ -n "$BOB_KEY" ] || die "user add returned no API key"
say "alice and bob exist; their API keys were printed once to the admin (kept in shell variables here)"
show user ls

# --- a second node ----------------------------------------------------------
step "3. a second node, beta (MYCEL_HOME=$NODE_HOME)"
say "\$ mycel token create --name beta --caps sandbox,docker,ollama --ttl 15m"
tok="$(hub_cli token create --name beta --caps sandbox,docker,ollama --ttl 15m --json)"
TOKEN="$(printf '%s' "$tok" | json_field token)"
[ -n "$TOKEN" ] || die "token create returned no token"
say "join token created (one use, 15 minutes)"
say "\$ mycel join <token>"
MYCEL_HOME="$NODE_HOME" "$BIN" join "$TOKEN" --json >/dev/null 2>&1 || die "join failed"
node_env=""
[ "$FAKE" -eq 1 ] && node_env="MYCEL_FAKE_SANDBOX_OK=1"
say "\$ mycel daemon"
# shellcheck disable=SC2086 # node_env is empty or one assignment
env $node_env MYCEL_HOME="$NODE_HOME" "$BIN" daemon --json --local-addr 127.0.0.1:0 \
	--log-file "$NODE_HOME/logs/daemon.log" >"$WORK/beta.out" 2>&1 &
NODE_PID=$!
node_ready="$(wait_ready "$WORK/beta.out")"
NODE_LOCAL="$(printf '%s' "$node_ready" | sed -n 's/.*"local_addr":"\([^"]*\)".*/\1/p')"
i=0
until hub_cli nodes ls --json | grep -q '"beta"'; do
	i=$((i + 1))
	[ $i -lt 60 ] || die "beta never showed up in nodes ls"
	sleep 0.5
done
show nodes ls
# beta shares this machine with you: by default ("local first") its remote
# sandboxes pause while you use the keyboard, which would stall the job.
say "(beta runs on the machine you are using: share it always, not local-first, for the demo)"
show nodes share beta --when always --local-first=false

# --- a channel --------------------------------------------------------------
step "4. a channel"
show channels create '#demo' --open
say "\$ mycel post '#demo' 'hello from alice'          (as alice)"
as_user "$ALICE_KEY" post '#demo' "hello from alice" >/dev/null
say "\$ mycel post '#demo' 'hi alice, bob here'        (as bob)"
as_user "$BOB_KEY" post '#demo' "hi alice, bob here" >/dev/null
show read '#demo' --limit 10

# --- a job ------------------------------------------------------------------
step "5. a job on beta"
JOB=""
if [ "$FAKE" -eq 1 ]; then
	profile=fake
elif [ "$(uname -s)" = Darwin ] && command -v limactl >/dev/null 2>&1; then
	profile=default
elif [ "$(uname -s)" = Linux ] && command -v runsc >/dev/null 2>&1; then
	profile=default
else
	profile=""
	say "skipped: no sandbox backend here (build with Go for the fake one, or install Lima / gVisor)"
fi
if [ -n "$profile" ]; then
	say "\$ mycel job run --profile $profile --node beta --channel '#demo' -- sh -c 'echo hello from the sandbox; uname -s'   (as alice)"
	# Right after joining, beta has not reported its sandbox backends yet:
	# retry briefly.
	i=0
	until run="$(as_user "$ALICE_KEY" job run --json --profile "$profile" --node beta --channel '#demo' --timeout 5m -- \
		sh -c 'echo hello from the sandbox; uname -s' 2>&1)"; do
		i=$((i + 1))
		[ $i -lt 40 ] || die "job run: $run"
		sleep 0.5
	done
	JOB="$(printf '%s' "$run" | json_field id)"
	[ -n "$JOB" ] || die "job run returned no job id"
	i=0
	while :; do
		state="$(as_user "$ALICE_KEY" job get "$JOB" --json | json_field state)"
		case "$state" in
		*SUCCEEDED* | *FAILED* | *CANCELED* | *CANCELLED* | *TIMED_OUT* | *LOST*) break ;;
		esac
		i=$((i + 1))
		[ $i -lt 600 ] || die "job $JOB still $state after 5 minutes"
		sleep 0.5
	done
	say "job $JOB: $state"
	say "\$ mycel job logs $JOB"
	as_user "$ALICE_KEY" job logs "$JOB"
fi

# --- a grant ----------------------------------------------------------------
step "6. a grant, requested and approved"
# A grant attaches a resource to a running job or agent: start a job that
# waits, ask for egress to example.com for it, approve it as admin.
subject="agent:demo@beta"
LONG=""
if [ -n "$profile" ]; then
	say "\$ mycel job run --profile $profile --node beta --name waiting -- sleep 300   (as alice)"
	LONG="$(as_user "$ALICE_KEY" job run --json --profile "$profile" --node beta --name waiting --timeout 10m -- sleep 300 | json_field id)"
	i=0
	until as_user "$ALICE_KEY" job get "$LONG" --json | grep -q 'JOB_STATE_RUNNING'; do
		i=$((i + 1))
		[ $i -lt 120 ] || die "job $LONG never started"
		sleep 0.5
	done
	subject="job:$LONG"
fi
say "\$ mycel grants request --kind egress --allow example.com --subject $subject --duration 10m --reason 'demo'   (as alice)"
if req="$(as_user "$ALICE_KEY" grants request --kind egress --allow example.com --subject "$subject" --duration 10m --reason demo --json 2>&1)"; then
	GRANT="$(printf '%s' "$req" | json_field id)"
	say "grant $GRANT: $(printf '%s' "$req" | json_field state)"
	show grants ls --pending
	show approve "$GRANT" --duration 10m --note "demo approval"
	show grants get "$GRANT"
else
	say "the grant request was refused: $req"
fi
if [ -n "$LONG" ]; then
	show job cancel "$LONG"
fi

# --- the UI -----------------------------------------------------------------
step "7. the UI"
say "hub admin UI:   https://127.0.0.1:$PORT/   (self-signed by the hub CA; sign in with an API key)"
say "hub node UI:    http://$HUB_LOCAL"
say "beta node UI:   http://$NODE_LOCAL   (\`MYCEL_HOME=$NODE_HOME mycel ui\` opens it signed in)"
say "logs:           $HUB_HOME/logs/daemon.log, $NODE_HOME/logs/daemon.log"

if [ "$KEEP" -eq 1 ]; then
	say ""
	say "running until Ctrl-C. Try, in another terminal:"
	say "  MYCEL_HOME=$HUB_HOME $BIN nodes ls"
	wait "$HUB_PID"
fi
step "done"
