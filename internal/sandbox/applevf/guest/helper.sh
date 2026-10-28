#!/bin/bash
# Mycelium applevf guest helper, run as the sandbox user through
# `limactl shell --shell /usr/local/libexec/mycel/shell` (bash without any
# profile or rc file). String arguments arrive as "b" + base64 so that no
# text is ever interpreted by a shell on the host or in the guest, whatever
# quoting `limactl shell` applies. Exec requests (argv, environment) never
# travel in argv at all: `putreq` receives them on stdin into a 0600 file
# that `exec` reads and deletes.
set -u -o pipefail
WS=/workspace
RUN=/tmp/.mycel-exec

dec() { printf '%s' "${1#b}" | base64 -d; }
die() { echo "mycel-helper: $*" >&2; exit 125; }

# confine <relative path>: prints the absolute path under /workspace or fails.
confine() {
	local p
	p=$(realpath -m -- "$WS/$1") || die "bad path"
	case "$p" in "$WS" | "$WS"/*) printf '%s' "$p" ;; *) die "path escapes /workspace" ;; esac
}

id_ok() { case "$1" in "" | *[!0-9a-f]*) die "bad id" ;; esac; }

rundir() {
	[ ! -L "$RUN" ] || die "$RUN is a symlink"
	mkdir -p "$RUN" || die "cannot create $RUN"
	[ -O "$RUN" ] || die "$RUN is not ours"
	chmod 0700 "$RUN" || die "cannot chmod $RUN"
}

# ancestors: pids of this process and its ancestors, space separated.
ancestors() {
	local p=$$ out=" "
	while [ "$p" -gt 1 ] 2>/dev/null; do
		out="$out$p "
		p=$(awk '{print $4}' "/proc/$p/stat" 2>/dev/null) || break
	done
	printf '%s' "$out"
}

# usermanager: pids of this user's systemd --user instance and its (sd-pam).
usermanager() {
	local out=" " p
	for p in $(pgrep -u "$(id -u)" -P 1 -x systemd 2>/dev/null); do
		[ "$(readlink "/proc/$p/exe")" = /usr/lib/systemd/systemd ] || continue
		out="$out$p "
		for c in $(pgrep -P "$p" -x '(sd-pam)' 2>/dev/null); do out="$out$c "; done
	done
	printf '%s' "$out"
}

op=${1:-}
shift || true
case "$op" in
putreq)
	id_ok "${1:-}"
	rundir
	umask 077
	cat > "$RUN/$1.req" || die "cannot store request"
	;;
exec)
	id_ok "${1:-}"
	req="$RUN/$1.req"
	pidfile="$RUN/$1.pid"
	[ -f "$req" ] || die "no request"
	mapfile -d '' -t f < "$req"
	rm -f -- "$req"
	dir=${f[0]}
	n=${f[1]}
	case "$n" in "" | *[!0-9]*) die "bad request" ;; esac
	envs=("${f[@]:2:n}")
	argv=("${f[@]:2+n}")
	[ "${#argv[@]}" -gt 0 ] || die "empty argv"
	cd -- "$(confine "$dir")" || exit 126
	# shellcheck disable=SC2163 # kv is NAME=value, validated on the host
	for kv in "${envs[@]}"; do export "$kv"; done
	# A new session makes the command a process group leader; its pid is the
	# group id that `signal` targets.
	# shellcheck disable=SC2016 # expanded by the inner shell
	setsid -w /bin/bash --noprofile --norc -c 'echo "$$" > "$0"; exec "$@"' "$pidfile" "${argv[@]}"
	rc=$?
	rm -f -- "$pidfile"
	exit "$rc"
	;;
signal)
	id_ok "${1:-}"
	sig=${2:-}
	case "$sig" in TERM | KILL | INT | HUP | QUIT | USR1 | USR2) ;; *) die "bad signal" ;; esac
	for _ in $(seq 1 50); do
		[ -s "$RUN/$1.pid" ] && break
		sleep 0.1
	done
	[ -s "$RUN/$1.pid" ] || exit 0 # already exited (or never started)
	pgid=$(cat "$RUN/$1.pid" 2>/dev/null)
	case "$pgid" in "" | *[!0-9]*) die "bad pid file" ;; esac
	[ "$pgid" -gt 1 ] || die "refusing process group $pgid"
	kill -s "$sig" -- "-$pgid" 2>/dev/null || true
	;;
copyin)
	dst=$(confine "$(dec "$1")")
	mode=$2
	case "$mode" in "" | *[!0-7]*) die "bad mode" ;; esac
	[ "$dst" != "$WS" ] || die "path is /workspace"
	mkdir -p -- "$(dirname -- "$dst")" || exit 1
	cat > "$dst" && chmod "$mode" -- "$dst"
	;;
copyout)
	src=$(confine "$(dec "$1")")
	if [ "$src" = "$WS" ]; then rel=.; else rel=${src#"$WS"/}; fi
	exec tar -C "$WS" -cf - -- "$rel"
	;;
untar)
	case "${1:-}" in
	gz) exec tar -C "$WS" -xzf - --no-same-owner ;;
	plain) exec tar -C "$WS" -xf - --no-same-owner ;;
	*) die "bad format" ;;
	esac
	;;
glob)
	cd "$WS" || exit 1
	shopt -s nullglob globstar
	IFS=
	for a in "$@"; do
		pat=$(dec "$a")
		# shellcheck disable=SC2206 # the pattern is expanded on purpose
		for m in $pat; do
			[ -e "$m" ] || continue
			p=$(confine "$m") || continue
			printf '%s\0' "${p#"$WS"/}"
		done
	done
	;;
pause)
	# SIGSTOP every process of the user except this helper's own chain (its
	# ssh session) and the systemd user manager; repeat until a pass finds no
	# new process, so children forked meanwhile are caught too.
	skip="$(ancestors)$(usermanager)"
	if [ -n "${DOCKER_HOST:-}" ] && command -v docker >/dev/null 2>&1; then
		docker ps -q 2>/dev/null | xargs -r docker pause >/dev/null 2>&1 || true
	fi
	stopped=" "
	for _ in $(seq 1 20); do
		new=0
		mapfile -t pids < <(pgrep -u "$(id -u)")
		for pid in "${pids[@]}"; do
			case "$skip$stopped" in *" $pid "*) continue ;; esac
			kill -s STOP "$pid" 2>/dev/null || true
			stopped="$stopped$pid "
			new=1
		done
		[ "$new" = 1 ] || break
	done
	;;
resume)
	skip="$(ancestors)"
	mapfile -t pids < <(pgrep -u "$(id -u)")
	for pid in "${pids[@]}"; do
		case "$skip" in *" $pid "*) continue ;; esac
		kill -s CONT "$pid" 2>/dev/null || true
	done
	if [ -n "${DOCKER_HOST:-}" ] && command -v docker >/dev/null 2>&1; then
		docker ps -q --filter status=paused 2>/dev/null | xargs -r docker unpause >/dev/null 2>&1 || true
	fi
	;;
stats)
	read -r load _ </proc/loadavg
	echo "load $load"
	awk '/^MemTotal:/ {print "memtotal " $2} /^MemAvailable:/ {print "memavail " $2}' /proc/meminfo
	echo "disk $(df -B1 --output=used "$WS" | tail -n 1 | tr -d ' ')"
	echo "procs $(pgrep -c -u "$(id -u)")"
	;;
check)
	# Succeeds only when the sandbox user has no root access left.
	if sudo -n true 2>/dev/null; then
		die "sudo is still available"
	fi
	[ "$(getent passwd "$(id -un)" | cut -d: -f7)" = /usr/local/libexec/mycel/shell ] || die "login shell not set"
	echo "mycel: locked"
	;;
*)
	die "unknown operation"
	;;
esac
